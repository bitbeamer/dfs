package managed

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	processcommand "github.com/bitbeamer/dfs/internal/command"
	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/optimization"
	"github.com/bitbeamer/dfs/internal/repository"
	quic "github.com/quic-go/quic-go"
)

const ALPN = "dfs-managed-v1"
const PairALPN = "dfs-pair-v2"

const managedDialTimeout = time.Second
const requestHeaderTimeout = 20 * time.Second
const requestHeaderLimit = 64 << 10

const (
	contentAvailabilityBudget = 1500 * time.Millisecond
	contentHedgeDelay         = 75 * time.Millisecond
	peerBackoffInitial        = 2 * time.Second
	peerBackoffMaximum        = time.Minute
)

type Request struct {
	Operation string          `json:"operation"`
	Service   string          `json:"service,omitempty"`
	Key       string          `json:"key,omitempty"`
	Offset    int64           `json:"offset,omitempty"`
	Length    int64           `json:"length,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type Response struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Size      int64           `json:"size,omitempty"`
	TotalSize int64           `json:"total_size,omitempty"`
	AnnexUUID string          `json:"annex_uuid,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type Server struct {
	repo        *repository.Repository
	annexUUID   string
	listener    *quic.Listener
	diagnostic  func(context.Context) ([]byte, error)
	pair        func(context.Context, string, net.Addr, json.RawMessage) (json.RawMessage, error)
	pairClone   func(context.Context, string, string) error
	changed     func(string, []string)
	stop        context.CancelFunc
	done        chan struct{}
	once        sync.Once
	connections atomic.Int64
	hookOnce    sync.Once
	hookErr     error
}

func Start(repo *repository.Repository, address string, diagnostic func(context.Context) ([]byte, error), pairingCertificate *tls.Certificate, pair func(context.Context, string, net.Addr, json.RawMessage) (json.RawMessage, error), pairClone func(context.Context, string, string) error, changed func(string, []string)) (*Server, error) {
	private, _, err := membership.EnsureKey(repo.Config.Repository)
	if err != nil {
		return nil, err
	}
	certificate, err := certificate(private, repo.Config.PeerID)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13,
		NextProtos: []string{ALPN}, ClientAuth: tls.RequireAnyClientCert}
	tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) != 1 {
			return errors.New("DFS managed transport requires one peer certificate")
		}
		peerCertificate, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		public, ok := peerCertificate.PublicKey.(ed25519.PublicKey)
		if !ok {
			return errors.New("DFS managed transport peer key is not Ed25519")
		}
		return verifyTrustedPublicKey(repo.Config.Repository, public)
	}
	if pairingCertificate != nil && pair != nil {
		managedConfig := tlsConfig.Clone()
		tlsConfig.NextProtos = []string{ALPN, PairALPN}
		tlsConfig.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			for _, protocol := range hello.SupportedProtos {
				if protocol == PairALPN {
					return &tls.Config{Certificates: []tls.Certificate{*pairingCertificate}, MinVersion: tls.VersionTLS13, NextProtos: []string{PairALPN}}, nil
				}
			}
			return managedConfig, nil
		}
	}
	listener, err := quic.ListenAddr(address, tlsConfig, &quic.Config{MaxIdleTimeout: 2 * time.Minute, KeepAlivePeriod: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	annexUUID, _ := exec.Command("git", "-C", repo.Config.Repository, "config", "--get", "annex.uuid").Output()
	server := &Server{repo: repo, annexUUID: strings.TrimSpace(string(annexUUID)), listener: listener,
		diagnostic: diagnostic, pair: pair, pairClone: pairClone, changed: changed, stop: cancel, done: make(chan struct{})}
	go server.serve(ctx)
	return server, nil
}

func (s *Server) Addr() net.Addr { return s.listener.Addr() }

func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		s.stop()
		err = s.listener.Close()
		<-s.done
	})
	return err
}

func (s *Server) serve(ctx context.Context) {
	defer close(s.done)
	for {
		connection, err := s.listener.Accept(ctx)
		if err != nil {
			return
		}
		s.connections.Add(1)
		go s.serveConnection(connection)
	}
}

func (s *Server) serveConnection(connection *quic.Conn) {
	defer connection.CloseWithError(0, "")
	for {
		stream, err := connection.AcceptStream(connection.Context())
		if err != nil {
			return
		}
		go s.serveStream(stream, connection.ConnectionState().TLS.NegotiatedProtocol, connection.RemoteAddr())
	}
}

func (s *Server) serveStream(stream *quic.Stream, protocol string, remote net.Addr) {
	defer stream.Close()
	header, reader, err := readRequestHeader(stream, requestHeaderTimeout)
	if err != nil {
		writeResponse(stream, Response{Error: "read managed transport request"})
		return
	}
	var request Request
	if err := json.Unmarshal(header, &request); err != nil {
		writeResponse(stream, Response{Error: "decode managed transport request"})
		return
	}
	if protocol == PairALPN {
		if request.Operation == "pair-clone" && s.pairClone != nil {
			s.servePairClone(stream, request.Payload)
			return
		}
		if s.pair == nil || (request.Operation != "pair-start" && request.Operation != "pair-complete" &&
			request.Operation != "join-request" && request.Operation != "join-status") {
			writeResponse(stream, Response{Error: "unsupported pairing operation"})
			return
		}
		payload, err := s.pair(stream.Context(), request.Operation, remote, request.Payload)
		if err != nil {
			writeResponse(stream, Response{Error: err.Error()})
			return
		}
		writeResponse(stream, Response{OK: true, Payload: payload})
		return
	}
	if protocol != ALPN {
		writeResponse(stream, Response{Error: "unsupported DFS transport protocol"})
		return
	}
	s.repo.LogContentRead("content request received", "operation", request.Operation)
	switch request.Operation {
	case "ping":
		writeResponse(stream, Response{OK: true, AnnexUUID: s.annexUUID})
	case "diagnostic":
		if s.diagnostic == nil {
			writeResponse(stream, Response{Error: "diagnostics unavailable"})
			return
		}
		data, err := s.diagnostic(stream.Context())
		if err != nil {
			writeResponse(stream, Response{Error: err.Error()})
			return
		}
		writeResponse(stream, Response{OK: true, Size: int64(len(data))})
		_, _ = stream.Write(data)
	case "reconcile":
		if s.changed == nil {
			writeResponse(stream, Response{Error: "reconciliation unavailable"})
			return
		}
		s.changed("peer requested membership reconciliation", nil)
		writeResponse(stream, Response{OK: true})
	case "git":
		s.serveGit(stream, reader, request.Service)
	case "annex-get":
		s.serveContent(stream, request.Key, 0, 0)
	case "annex-range":
		s.serveContent(stream, request.Key, request.Offset, request.Length)
	case "annex-has":
		s.serveHasContent(stream, request.Key)
	case "benchmark":
		serveBenchmark(stream, request.Offset, request.Length)
	case "optimize":
		state, err := OptimizeLocal(stream.Context(), s.repo, nil)
		if err != nil {
			writeResponse(stream, Response{Error: err.Error()})
			return
		}
		payload, err := json.Marshal(state)
		if err != nil {
			writeResponse(stream, Response{Error: "encode optimization result"})
			return
		}
		writeResponse(stream, Response{OK: true, Payload: payload})
	default:
		writeResponse(stream, Response{Error: "unsupported managed transport operation"})
	}
}

type requestHeaderReader interface {
	io.Reader
	SetReadDeadline(time.Time) error
}

func readRequestHeader(stream requestHeaderReader, timeout time.Duration) ([]byte, *bufio.Reader, error) {
	if err := stream.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReaderSize(stream, requestHeaderLimit+1)
	header, err := reader.ReadSlice('\n')
	if err != nil || len(header) > requestHeaderLimit {
		return nil, nil, errors.New("managed transport request header exceeds its limit or deadline")
	}
	if err := stream.SetReadDeadline(time.Time{}); err != nil {
		return nil, nil, err
	}
	return header, reader, nil
}

func (s *Server) serveHasContent(stream *quic.Stream, key string) {
	if key == "" || strings.ContainsAny(key, "\r\n\x00") {
		writeResponse(stream, Response{Error: "invalid annex key"})
		return
	}
	command := exec.CommandContext(stream.Context(), "git", "annex", "contentlocation", key)
	command.Dir = s.repo.Config.Repository
	output, err := command.Output()
	if err != nil {
		writeResponse(stream, Response{Error: "annex content is unavailable"})
		return
	}
	relative := strings.TrimSpace(string(output))
	path := filepath.Join(s.repo.Config.Repository, filepath.FromSlash(relative))
	annexRoot := filepath.Join(s.repo.Config.Repository, ".git", "annex", "objects") + string(os.PathSeparator)
	abs, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(abs, annexRoot) {
		writeResponse(stream, Response{Error: "annex returned an unsafe content path"})
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeResponse(stream, Response{Error: "annex content is unavailable"})
		return
	}
	writeResponse(stream, Response{OK: true, TotalSize: info.Size(), AnnexUUID: s.annexUUID})
}

func RequestReconcile(ctx context.Context, repo *repository.Repository, peerID string) error {
	connection, stream, _, _, err := Open(ctx, repo, peerID, Request{Operation: "reconcile"})
	if err != nil {
		return err
	}
	defer connection.CloseWithError(0, "")
	return stream.Close()
}

type pairCloneRequest struct {
	SessionID        string `json:"session_id"`
	CompletionSecret string `json:"completion_secret"`
}

func (s *Server) servePairClone(stream *quic.Stream, payload json.RawMessage) {
	var request pairCloneRequest
	if json.Unmarshal(payload, &request) != nil || request.SessionID == "" || request.CompletionSecret == "" {
		writeResponse(stream, Response{Error: "invalid pairing clone request"})
		return
	}
	if err := s.pairClone(stream.Context(), request.SessionID, request.CompletionSecret); err != nil {
		writeResponse(stream, Response{Error: err.Error()})
		return
	}
	stateDirectory := filepath.Join(s.repo.Config.Repository, ".git", "dfs")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		writeResponse(stream, Response{Error: "prepare pairing bundle"})
		return
	}
	bundle, err := os.CreateTemp(stateDirectory, "pair-clone-*.bundle")
	if err != nil {
		writeResponse(stream, Response{Error: "create pairing bundle"})
		return
	}
	bundlePath := bundle.Name()
	_ = bundle.Close()
	defer os.Remove(bundlePath)
	if err := s.repo.WithWorkTreeLock(func() error {
		command := exec.CommandContext(stream.Context(), "git", "bundle", "create", bundlePath, "--all")
		command.Dir = s.repo.Config.Repository
		return command.Run()
	}); err != nil {
		writeResponse(stream, Response{Error: "create pairing bundle"})
		return
	}
	file, err := os.Open(bundlePath)
	if err != nil {
		writeResponse(stream, Response{Error: "open pairing bundle"})
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeResponse(stream, Response{Error: "inspect pairing bundle"})
		return
	}
	if err := writeResponse(stream, Response{OK: true, Size: info.Size()}); err != nil {
		return
	}
	_, _ = io.Copy(stream, file)
}

func PairCall(ctx context.Context, address, certificateSHA256, operation string, input, output any) error {
	connection, err := dialPairing(ctx, address, certificateSHA256)
	if err != nil {
		return err
	}
	defer connection.CloseWithError(0, "")
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, _ := json.Marshal(Request{Operation: operation, Payload: payload})
	if _, err := stream.Write(append(request, '\n')); err != nil {
		return err
	}
	_ = stream.Close()
	line, err := bufio.NewReader(stream).ReadBytes('\n')
	if err != nil {
		return err
	}
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		return err
	}
	if !response.OK {
		return errors.New(response.Error)
	}
	return json.Unmarshal(response.Payload, output)
}

// PairProbe verifies that a discovered pairing endpoint is still online and
// presents the certificate advertised over mDNS. It does not create a stream
// or mutate peer state.
func PairProbe(ctx context.Context, address, certificateSHA256 string) error {
	connection, err := dialPairing(ctx, address, certificateSHA256)
	if err != nil {
		return err
	}
	_ = connection.CloseWithError(0, "")
	return nil
}

func dialPairing(ctx context.Context, address, certificateSHA256 string) (*quic.Conn, error) {
	address = strings.TrimPrefix(strings.TrimSpace(address), "quic://")
	if address == "" {
		return nil, errors.New("pairing QUIC endpoint is empty")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{PairALPN}, InsecureSkipVerify: true}
	tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) != 1 {
			return errors.New("pairing certificate is missing")
		}
		digest := sha256.Sum256(rawCerts[0])
		if !strings.EqualFold(hex.EncodeToString(digest[:]), certificateSHA256) {
			return errors.New("pairing certificate does not match invitation")
		}
		return nil
	}
	connection, err := quic.DialAddr(ctx, address, tlsConfig, &quic.Config{HandshakeIdleTimeout: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	return connection, nil
}

func PairClone(ctx context.Context, address, certificateSHA256, sessionID, completionSecret, destination string) error {
	address = strings.TrimPrefix(strings.TrimSpace(address), "quic://")
	if address == "" {
		return errors.New("pairing QUIC endpoint is empty")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{PairALPN}, InsecureSkipVerify: true}
	tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) != 1 {
			return errors.New("pairing certificate is missing")
		}
		digest := sha256.Sum256(rawCerts[0])
		if !strings.EqualFold(hex.EncodeToString(digest[:]), certificateSHA256) {
			return errors.New("pairing certificate does not match invitation")
		}
		return nil
	}
	connection, err := quic.DialAddr(ctx, address, tlsConfig, &quic.Config{HandshakeIdleTimeout: 10 * time.Second})
	if err != nil {
		return err
	}
	defer connection.CloseWithError(0, "")
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(pairCloneRequest{SessionID: sessionID, CompletionSecret: completionSecret})
	request, _ := json.Marshal(Request{Operation: "pair-clone", Payload: payload})
	if _, err := stream.Write(append(request, '\n')); err != nil {
		return err
	}
	_ = stream.Close()
	reader := bufio.NewReader(stream)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		return err
	}
	if !response.OK {
		return errors.New(response.Error)
	}
	temporary := destination + ".new"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(file, reader, response.Size)
	if syncErr := file.Sync(); copyErr == nil {
		copyErr = syncErr
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(temporary)
		return copyErr
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func (s *Server) serveGit(stream *quic.Stream, input io.Reader, service string) {
	if service != "git-upload-pack" && service != "git-receive-pack" && service != "git-upload-archive" {
		writeResponse(stream, Response{Error: "unsupported Git service"})
		return
	}
	var refsBefore string
	var refsBeforeOK bool
	var treeBefore string
	var pinRefBefore string
	if service == "git-receive-pack" {
		refsBefore, refsBeforeOK = gitRefsValue(stream.Context(), s.repo.Config.Repository)
		treeBefore = worktreeTree(stream.Context(), s.repo.Config.Repository)
		pinRefBefore = gitRefValue(stream.Context(), s.repo.Config.Repository, membership.PinRef)
	}
	commandName := service
	commandArgs := []string{s.repo.Config.Repository}
	if service == "git-receive-pack" {
		s.hookOnce.Do(func() { s.hookErr = installReceiveGuard(s.repo.Config.Repository) })
		if s.hookErr != nil {
			writeResponse(stream, Response{Error: "prepare guarded Git receive"})
			return
		}
		commandName = "git"
		commandArgs = []string{"-c", "core.hooksPath=" + filepath.Join(s.repo.Config.Repository, ".git", "dfs", "managed-hooks"), "receive-pack", s.repo.Config.Repository}
	}
	command := exec.CommandContext(stream.Context(), commandName, commandArgs...)
	processcommand.ConfigureCancellation(command)
	command.Stdin, command.Stdout, command.Stderr = input, stream, io.Discard
	if err := command.Start(); err != nil {
		writeResponse(stream, Response{Error: err.Error()})
		return
	}
	if err := writeResponse(stream, Response{OK: true}); err != nil {
		if command.Cancel != nil {
			_ = command.Cancel()
		} else {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
		return
	}
	if err := command.Wait(); err == nil && service == "git-receive-pack" && s.changed != nil {
		// Git invokes receive-pack even when every advertised ref is already at
		// the requested object. Do not turn that no-op exchange into another
		// reconciliation pass: periodic peers would otherwise keep each other in
		// a permanent receive/sync loop and continuously disturb the FUSE mount.
		refsAfter, refsAfterOK := gitRefsValue(stream.Context(), s.repo.Config.Repository)
		if refsBeforeOK && refsAfterOK && refsAfter == refsBefore {
			return
		}
		treeAfter := worktreeTree(stream.Context(), s.repo.Config.Repository)
		reason := "managed Git receive"
		if gitRefValue(stream.Context(), s.repo.Config.Repository, membership.PinRef) != pinRefBefore {
			reason = "pin policy changed"
		}
		s.changed(reason, changedPaths(stream.Context(), s.repo.Config.Repository, treeBefore, treeAfter))
	}
}

func installReceiveGuard(repositoryPath string) error {
	directory := filepath.Join(repositoryPath, ".git", "dfs", "managed-hooks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	executable, err := receiveGuardExecutable()
	if err != nil {
		return err
	}
	hook := "#!/bin/sh\nset -eu\nexec " + shellArgument(executable) + " --repo " + shellArgument(repositoryPath) + " internal receive-guard\n"
	path := filepath.Join(directory, "pre-receive")
	temporary, err := os.CreateTemp(directory, "pre-receive-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.WriteString(hook); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

var receiveGuardExecutable = os.Executable

func shellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// ValidateReceiveUpdates validates pre-receive input for managed control refs.
func ValidateReceiveUpdates(repositoryPath string, input io.Reader) error {
	updates := make(map[string]membership.RefUpdate)
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return errors.New("invalid managed pre-receive update")
		}
		old, next, ref := fields[0], fields[1], fields[2]
		if ref != membership.SharedRef && ref != membership.PinRef && ref != membership.ConfigRef {
			continue
		}
		if zeroObjectID(next) {
			return fmt.Errorf("DFS control ref %s cannot be deleted", ref)
		}
		if !zeroObjectID(old) {
			command := exec.Command("git", "-C", repositoryPath, "merge-base", "--is-ancestor", old, next)
			command.Env = managedReceiveGitEnvironment(os.Environ())
			if output, err := command.CombinedOutput(); err != nil {
				return fmt.Errorf("DFS control ref %s cannot be rewound: %s", ref, strings.TrimSpace(string(output)))
			}
		}
		updates[ref] = membership.RefUpdate{Old: old, New: next}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return membership.ValidateControlUpdates(repositoryPath, updates)
}

func managedReceiveGitEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		name := value
		if index := strings.IndexByte(name, '='); index >= 0 {
			name = name[:index]
		}
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_PREFIX":
			continue
		default:
			result = append(result, value)
		}
	}
	return result
}

func zeroObjectID(value string) bool {
	return value != "" && strings.Trim(value, "0") == ""
}

func gitRefsValue(ctx context.Context, repositoryPath string) (string, bool) {
	output, err := exec.CommandContext(ctx, "git", "-C", repositoryPath, "for-each-ref", "--format=%(refname) %(objectname)", "refs").Output()
	if err != nil {
		return "", false
	}
	return string(output), true
}

func gitRefValue(ctx context.Context, repositoryPath, ref string) string {
	output, err := exec.CommandContext(ctx, "git", "-C", repositoryPath, "rev-parse", "--verify", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func worktreeTree(ctx context.Context, repositoryPath string) string {
	output, err := exec.CommandContext(ctx, "git", "-C", repositoryPath, "rev-parse", "HEAD^{tree}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func changedPaths(ctx context.Context, repositoryPath, before, after string) []string {
	if before == "" || after == "" || before == after {
		return nil
	}
	output, err := exec.CommandContext(ctx, "git", "-C", repositoryPath, "diff", "--name-only", "-z", before, after).Output()
	if err != nil {
		return nil
	}
	parts := bytes.Split(output, []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			paths = append(paths, string(part))
		}
	}
	return paths
}

func userVisibleRefs(ctx context.Context, repositoryPath string) []byte {
	output, err := exec.CommandContext(ctx, "git", "-C", repositoryPath, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads").Output()
	if err != nil {
		return nil
	}
	var visible []byte
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		if bytes.HasSuffix(line, []byte(" refs/heads/git-annex")) ||
			bytes.HasSuffix(line, []byte(" refs/heads/synced/git-annex")) ||
			bytes.HasSuffix(line, []byte(" refs/heads/dfs-membership")) ||
			bytes.HasSuffix(line, []byte(" refs/heads/dfs-pins")) {
			continue
		}
		visible = append(visible, line...)
		visible = append(visible, '\n')
	}
	return visible
}

func (s *Server) serveContent(stream *quic.Stream, key string, offset, length int64) {
	started := time.Now()
	if key == "" || strings.ContainsAny(key, "\r\n\x00") {
		writeResponse(stream, Response{Error: "invalid annex key"})
		return
	}
	command := exec.CommandContext(stream.Context(), "git", "annex", "contentlocation", key)
	command.Dir = s.repo.Config.Repository
	output, err := command.Output()
	s.repo.LogContentRead("content lookup completed", "duration", time.Since(started), "error", err)
	if err != nil {
		writeResponse(stream, Response{Error: "annex content is unavailable"})
		return
	}
	relative := strings.TrimSpace(string(output))
	path := filepath.Join(s.repo.Config.Repository, filepath.FromSlash(relative))
	annexRoot := filepath.Join(s.repo.Config.Repository, ".git", "annex", "objects") + string(os.PathSeparator)
	abs, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(abs, annexRoot) {
		writeResponse(stream, Response{Error: "annex returned an unsafe content path"})
		return
	}
	file, err := os.Open(abs)
	if err != nil {
		writeResponse(stream, Response{Error: "open annex content"})
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeResponse(stream, Response{Error: "inspect annex content"})
		return
	}
	if offset < 0 || length < 0 || offset > info.Size() {
		writeResponse(stream, Response{Error: "invalid annex content range"})
		return
	}
	if length == 0 {
		length = info.Size() - offset
	}
	if length > info.Size()-offset {
		writeResponse(stream, Response{Error: "annex content range exceeds object size"})
		return
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		writeResponse(stream, Response{Error: "seek annex content"})
		return
	}
	if err := writeResponse(stream, Response{OK: true, Size: length, TotalSize: info.Size()}); err != nil {
		return
	}
	_, _ = io.CopyN(stream, file, length)
}

func Dial(ctx context.Context, repo *repository.Repository, peerID string) (*quic.Conn, membership.Record, error) {
	target, err := trustedMember(repo.Config.Repository, peerID)
	if err != nil {
		return nil, membership.Record{}, err
	}
	connection, err := dialTrustedMember(ctx, repo, target)
	return connection, target, err
}

func dialTrustedMember(ctx context.Context, repo *repository.Repository, target membership.Record) (*quic.Conn, error) {
	started := time.Now()
	private, _, err := membership.EnsureKey(repo.Config.Repository)
	if err != nil {
		return nil, err
	}
	clientCertificate, err := certificate(private, repo.Config.PeerID)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(target.Payload.QUICEndpoint)
	if err != nil || endpoint.Host == "" {
		return nil, errors.New("invalid member QUIC endpoint")
	}
	address := endpoint.Host
	resolveStarted := time.Now()
	if ipv4, ok := localIPv4(ctx, endpoint.Hostname()); ok {
		address = net.JoinHostPort(ipv4, endpoint.Port())
	}
	repo.LogContentRead("content peer address resolved", "peer_id", target.Payload.PeerID,
		"address", address, "duration", time.Since(resolveStarted))
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{clientCertificate}, MinVersion: tls.VersionTLS13,
		NextProtos: []string{ALPN}, ServerName: target.Payload.Hostname, InsecureSkipVerify: true}
	tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) != 1 {
			return errors.New("DFS managed transport server certificate is missing")
		}
		serverCertificate, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		public, ok := serverCertificate.PublicKey.(ed25519.PublicKey)
		if !ok || base64Public(public) != target.Payload.SigningPublicKey {
			return errors.New("DFS managed transport server key does not match signed membership")
		}
		return nil
	}
	dialContext, cancel := context.WithTimeout(ctx, managedDialTimeout)
	defer cancel()
	connection, err := quic.DialAddr(dialContext, address, tlsConfig, &quic.Config{
		HandshakeIdleTimeout: managedDialTimeout, MaxIdleTimeout: 2 * time.Minute, KeepAlivePeriod: 20 * time.Second,
	})
	repo.LogContentRead("content peer dial completed", "peer_id", target.Payload.PeerID,
		"duration", time.Since(started), "error", err)
	return connection, err
}

func localIPv4(ctx context.Context, hostname string) (string, bool) {
	return localIPv4WithResolver(ctx, hostname, net.DefaultResolver.LookupIP)
}

func localIPv4WithResolver(ctx context.Context, hostname string, lookup func(context.Context, string, string) ([]net.IP, error)) (string, bool) {
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if !strings.HasSuffix(hostname, ".local") {
		return "", false
	}
	lookupContext, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	addresses, err := lookup(lookupContext, "ip4", hostname)
	if err != nil || len(addresses) == 0 {
		return "", false
	}
	return addresses[0].String(), true
}

func Open(ctx context.Context, repo *repository.Repository, peerID string, request Request) (*quic.Conn, *quic.Stream, *bufio.Reader, Response, error) {
	connection, _, err := Dial(ctx, repo, peerID)
	if err != nil {
		return nil, nil, nil, Response{}, err
	}
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		_ = connection.CloseWithError(1, "open stream")
		return nil, nil, nil, Response{}, err
	}
	data, _ := json.Marshal(request)
	if _, err := stream.Write(append(data, '\n')); err != nil {
		_ = connection.CloseWithError(1, "write request")
		return nil, nil, nil, Response{}, err
	}
	reader := bufio.NewReader(stream)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = connection.CloseWithError(1, "read response")
		return nil, nil, nil, Response{}, err
	}
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		_ = connection.CloseWithError(1, "decode response")
		return nil, nil, nil, Response{}, err
	}
	if !response.OK {
		_ = connection.CloseWithError(1, "remote error")
		return nil, nil, nil, response, errors.New(response.Error)
	}
	return connection, stream, reader, response, nil
}

// GitProxy connects Git's remote-ext protocol to an authenticated QUIC stream.
func GitProxy(ctx context.Context, repo *repository.Repository, peerID, service string, input io.Reader, output, errorOutput io.Writer) (string, error) {
	connection, stream, reader, _, err := Open(ctx, repo, peerID, Request{Operation: "git", Service: service})
	if err != nil {
		return "quic", err
	}
	defer connection.CloseWithError(0, "")
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stream, input)
		closeErr := stream.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		copyDone <- copyErr
	}()
	_, outputErr := io.Copy(output, reader)
	inputErr := <-copyDone
	if outputErr != nil {
		return "quic", outputErr
	}
	return "quic", inputErr
}

func Diagnostic(ctx context.Context, repo *repository.Repository, peerID string) ([]byte, error) {
	connection, stream, reader, response, err := Open(ctx, repo, peerID, Request{Operation: "diagnostic"})
	if err != nil {
		return nil, err
	}
	defer connection.CloseWithError(0, "")
	defer stream.Close()
	data := make([]byte, response.Size)
	_, err = io.ReadFull(reader, data)
	return data, err
}

func FetchContent(ctx context.Context, repo *repository.Repository, peerID, key string, output io.Writer) (int64, error) {
	stream, reader, response, err := openContentStream(ctx, repo, peerID, Request{Operation: "annex-get", Key: key})
	if err != nil {
		if isUnavailableContent(err) {
			unavailableContent.mark(repo.Config.Repository, peerID, key)
		} else {
			peerAvailability.markFailure(repo.Config.Repository, peerID)
		}
		return 0, err
	}
	defer stream.Close()
	written, err := io.CopyN(output, reader, response.Size)
	if err != nil {
		contentSessions.invalidate(repo.Config.Repository, peerID)
		peerAvailability.markFailure(repo.Config.Repository, peerID)
	} else {
		peerAvailability.markSuccess(repo.Config.Repository, peerID)
		unavailableContent.clear(repo.Config.Repository, peerID, key)
	}
	return written, err
}

func FetchRange(ctx context.Context, repo *repository.Repository, key string, offset, length int64, output io.Writer) (int64, error) {
	started := time.Now()
	planningCtx, cancel := context.WithTimeout(ctx, contentAvailabilityBudget)
	defer cancel()
	allPeerIDs, livePeerIDs, allHints, livenessKnown, holdersKnown := localContentPlan(repo.Config.Repository, key)
	if !livenessKnown {
		var err error
		allPeerIDs, err = optimizedPeerIDs(ctx, repo, "interactive")
		if err != nil {
			return 0, err
		}
	}
	if len(allPeerIDs) == 0 {
		return 0, &repository.ContentUnavailableError{Reason: repository.AvailabilityNoTrustedPeers,
			Detail: "no accepted remote peer can serve content"}
	}
	if !holdersKnown {
		allHints = repo.PeerContentHints(planningCtx, key, allPeerIDs)
	}
	if knownHoldersOffline(livenessKnown, livePeerIDs, allHints) {
		return 0, &repository.ContentUnavailableError{Reason: repository.AvailabilityKnownHoldersOffline,
			Detail: "every peer recorded as holding this object is currently offline"}
	}
	peerIDs := allPeerIDs
	if livenessKnown {
		peerIDs = livePeerIDs
	}
	peerIDs = contentCandidates(repo.Config.Repository, key, peerIDs)
	if len(peerIDs) == 0 {
		reason := repository.AvailabilityAcceptedPeersOffline
		if len(allHints) > 0 {
			reason = repository.AvailabilityKnownHoldersOffline
		}
		detail := "every candidate peer is in transfer backoff"
		if livenessKnown {
			detail = "the local liveness monitor reports every accepted peer offline"
		}
		return 0, &repository.ContentUnavailableError{Reason: reason, Detail: detail}
	}
	hints := orderedIntersection(peerIDs, allHints)
	candidates := hints
	discovery := "annex-location"
	if len(candidates) == 0 {
		discovery = "parallel-has-content"
		candidates = discoverContentHolders(planningCtx, repo, key, peerIDs)
	}
	repo.LogContentRead("content sources planned", "key", key, "accepted_peers", len(peerIDs),
		"holder_hints", len(hints), "candidates", len(candidates), "discovery", discovery,
		"liveness_known", livenessKnown, "duration", time.Since(started))
	repo.RecordContentPlan(discovery, "")
	total, payload, sourcePeer, failures, allUnreachable := fetchRangeCandidates(planningCtx, repo, key, offset, length, candidates)
	if payload != nil {
		repo.RecordContentPlan("", sourcePeer)
		_, copyErr := output.Write(payload)
		if copyErr != nil {
			return 0, copyErr
		}
		return total, nil
	}

	// Location logs are advisory. If all hinted holders failed, ask the
	// remaining online accepted peers before concluding that content is absent.
	if len(hints) > 0 && planningCtx.Err() == nil {
		discovered := discoverContentHolders(planningCtx, repo, key, peerIDs)
		discovered = withoutPeerIDs(discovered, hints)
		fallbackTotal, fallbackPayload, fallbackSource, fallbackFailures, fallbackUnreachable := fetchRangeCandidates(planningCtx, repo, key, offset, length, discovered)
		if len(fallbackFailures) > 0 {
			allUnreachable = allUnreachable && fallbackUnreachable
		}
		failures = append(failures, fallbackFailures...)
		if fallbackPayload != nil {
			repo.RecordContentPlan("parallel-has-content", fallbackSource)
			_, copyErr := output.Write(fallbackPayload)
			if copyErr != nil {
				return 0, copyErr
			}
			return fallbackTotal, nil
		}
	}
	if errors.Is(planningCtx.Err(), context.DeadlineExceeded) {
		failures = append(failures, "availability budget exceeded")
		return 0, &repository.ContentUnavailableError{Reason: repository.AvailabilityTimeout,
			Detail: strings.Join(failures, "; ")}
	}
	if len(failures) == 0 {
		return 0, &repository.ContentUnavailableError{Reason: repository.AvailabilityNoOnlineCopy,
			Detail: "online accepted peers do not report this object"}
	}
	if allUnreachable {
		reason := repository.AvailabilityAcceptedPeersOffline
		if len(allHints) > 0 {
			reason = repository.AvailabilityKnownHoldersOffline
		}
		return 0, &repository.ContentUnavailableError{Reason: reason, Detail: strings.Join(failures, "; ")}
	}
	return 0, &repository.ContentUnavailableError{Reason: repository.AvailabilityTransferFailed,
		Detail: "managed range fetch failed: " + strings.Join(failures, "; ")}
}

func knownHoldersOffline(livenessKnown bool, livePeerIDs, holderHints []string) bool {
	return livenessKnown && len(holderHints) > 0 && len(orderedIntersection(livePeerIDs, holderHints)) == 0
}

func orderedIntersection(order, included []string) []string {
	wanted := make(map[string]bool, len(included))
	for _, peerID := range included {
		wanted[peerID] = true
	}
	return orderedSubset(order, wanted)
}

type rangeCandidateResult struct {
	peerID      string
	total       int64
	data        []byte
	unreachable bool
	err         error
}

func fetchRangeCandidates(ctx context.Context, repo *repository.Repository, key string, offset, length int64, peerIDs []string) (int64, []byte, string, []string, bool) {
	var failures []string
	allUnreachable := true
	for start := 0; start < len(peerIDs); start += 2 {
		end := min(start+2, len(peerIDs))
		batch := peerIDs[start:end]
		batchCtx, cancel := context.WithCancel(ctx)
		results := make(chan rangeCandidateResult, len(batch))
		launch := func(peerID string) {
			go func() { results <- fetchRangeCandidate(batchCtx, repo, peerID, key, offset, length) }()
		}
		launch(batch[0])
		launched, received := 1, 0
		var hedge <-chan time.Time
		if len(batch) > 1 {
			timer := time.NewTimer(contentHedgeDelay)
			defer timer.Stop()
			hedge = timer.C
		}
		for received < launched || launched < len(batch) {
			select {
			case <-ctx.Done():
				cancel()
				return 0, nil, "", append(failures, ctx.Err().Error()), allUnreachable
			case <-hedge:
				if launched < len(batch) {
					launch(batch[launched])
					launched++
				}
				hedge = nil
			case result := <-results:
				received++
				if result.err == nil {
					cancel()
					return result.total, result.data, result.peerID, failures, false
				}
				allUnreachable = allUnreachable && result.unreachable
				failures = append(failures, result.peerID+": "+result.err.Error())
				if launched < len(batch) {
					launch(batch[launched])
					launched++
					hedge = nil
				}
			}
		}
		cancel()
	}
	return 0, nil, "", failures, allUnreachable
}

func fetchRangeCandidate(ctx context.Context, repo *repository.Repository, peerID, key string, offset, length int64) rangeCandidateResult {
	result := rangeCandidateResult{peerID: peerID}
	started := time.Now()
	stream, reader, response, err := openContentStream(ctx, repo, peerID, Request{
		Operation: "annex-range", Key: key, Offset: offset, Length: length,
	})
	if err != nil {
		if isUnavailableContent(err) {
			unavailableContent.mark(repo.Config.Repository, peerID, key)
		} else {
			peerAvailability.markFailure(repo.Config.Repository, peerID)
			result.unreachable = true
		}
		result.err = err
		return result
	}
	defer stream.Close()
	if response.Size != length || response.TotalSize <= 0 {
		peerAvailability.markFailure(repo.Config.Repository, peerID)
		result.err = errors.New("invalid annex range response")
		return result
	}
	result.data = make([]byte, response.Size)
	if len(result.data) > 0 {
		result.data[0], result.err = reader.ReadByte()
		if result.err == nil {
			repo.LogContentRead("content first byte received", "peer_id", peerID, "offset", offset,
				"requested_bytes", length, "duration", time.Since(started))
			_, result.err = io.ReadFull(reader, result.data[1:])
		}
	}
	if result.err != nil {
		contentSessions.invalidate(repo.Config.Repository, peerID)
		peerAvailability.markFailure(repo.Config.Repository, peerID)
		return result
	}
	result.total = response.TotalSize
	repo.LogContentRead("content range received", "peer_id", peerID, "offset", offset, "bytes", length,
		"duration", time.Since(started))
	unavailableContent.clear(repo.Config.Repository, peerID, key)
	peerAvailability.markSuccess(repo.Config.Repository, peerID)
	return result
}

func discoverContentHolders(ctx context.Context, repo *repository.Repository, key string, peerIDs []string) []string {
	type result struct {
		peerID    string
		has       bool
		annexUUID string
		duration  time.Duration
		err       error
	}
	results := make(chan result, len(peerIDs))
	count := 0
	for _, peerID := range peerIDs {
		if peerAvailability.isOpen(repo.Config.Repository, peerID) || unavailableContent.isKnown(repo.Config.Repository, peerID, key) {
			continue
		}
		count++
		go func(peerID string) {
			started := time.Now()
			stream, _, response, err := openContentStream(ctx, repo, peerID, Request{Operation: "annex-has", Key: key})
			if stream != nil {
				_ = stream.Close()
			}
			if err == nil && response.TotalSize > 0 {
				peerAvailability.markSuccess(repo.Config.Repository, peerID)
				unavailableContent.clear(repo.Config.Repository, peerID, key)
				results <- result{peerID: peerID, has: true, annexUUID: response.AnnexUUID, duration: time.Since(started)}
				return
			}
			if isUnavailableContent(err) {
				unavailableContent.mark(repo.Config.Repository, peerID, key)
			} else if err != nil {
				peerAvailability.markFailure(repo.Config.Repository, peerID)
			}
			results <- result{peerID: peerID, duration: time.Since(started), err: err}
		}(peerID)
	}
	found := make(map[string]bool)
	for received := 0; received < count; received++ {
		select {
		case <-ctx.Done():
			return orderedSubset(peerIDs, found)
		case value := <-results:
			if value.err != nil {
				repo.LogContentRead("content availability check failed", "peer_id", value.peerID,
					"duration", value.duration, "error", value.err)
			}
			if value.has {
				if value.annexUUID != "" {
					go func(peerID, annexUUID string) {
						persistCtx, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						_ = repo.RecordPeerAnnexUUID(persistCtx, peerID, annexUUID)
					}(value.peerID, value.annexUUID)
				}
				// One verified online holder is enough to begin the demand read.
				// Do not let an unrelated offline probe consume the interactive
				// availability budget after content has already been located.
				return []string{value.peerID}
			}
		}
	}
	return orderedSubset(peerIDs, found)
}

func orderedSubset(peerIDs []string, included map[string]bool) []string {
	result := make([]string, 0, len(included))
	for _, peerID := range peerIDs {
		if included[peerID] {
			result = append(result, peerID)
		}
	}
	return result
}

func withoutPeerIDs(peerIDs, excluded []string) []string {
	blocked := make(map[string]bool, len(excluded))
	for _, peerID := range excluded {
		blocked[peerID] = true
	}
	var result []string
	for _, peerID := range peerIDs {
		if !blocked[peerID] {
			result = append(result, peerID)
		}
	}
	return result
}

func FetchPath(ctx context.Context, repo *repository.Repository, path, from string) error {
	key, err := repo.LookupKey(ctx, path)
	if err != nil {
		return err
	}
	wantedPrefix := strings.TrimPrefix(from, "dfs-peer-")
	peerIDs, err := optimizedPeerIDs(ctx, repo, "bulk")
	if err != nil {
		return err
	}
	if wantedPrefix != "" {
		filtered := peerIDs[:0]
		for _, peerID := range peerIDs {
			if strings.HasPrefix(peerID, wantedPrefix) {
				filtered = append(filtered, peerID)
			}
		}
		peerIDs = filtered
	} else {
		peerIDs = contentCandidates(repo.Config.Repository, key, peerIDs)
		if hinted := repo.PeerContentHints(ctx, key, peerIDs); len(hinted) > 0 {
			peerIDs = hinted
		} else {
			peerIDs = discoverContentHolders(ctx, repo, key, peerIDs)
		}
	}
	if len(peerIDs) == 0 {
		return &repository.ContentUnavailableError{Reason: repository.AvailabilityNoOnlineCopy,
			Detail: "no trusted managed content source is available"}
	}
	stateDirectory := filepath.Join(repo.Config.Repository, ".git", "dfs")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return err
	}
	var failures []string
	for _, peerID := range peerIDs {
		temporary, err := os.CreateTemp(stateDirectory, "managed-content-*")
		if err != nil {
			return err
		}
		temporaryPath := temporary.Name()
		_, fetchErr := FetchContent(ctx, repo, peerID, key, temporary)
		if isUnavailableContent(fetchErr) {
			unavailableContent.mark(repo.Config.Repository, peerID, key)
		}
		closeErr := temporary.Close()
		if fetchErr == nil {
			fetchErr = closeErr
		}
		if fetchErr == nil {
			fetchErr = repo.ReinjectContent(ctx, temporaryPath, path)
		}
		_ = os.Remove(temporaryPath)
		if fetchErr == nil {
			unavailableContent.clear(repo.Config.Repository, peerID, key)
			return nil
		}
		failures = append(failures, peerID+": "+fetchErr.Error())
	}
	return &repository.ContentUnavailableError{Reason: repository.AvailabilityTransferFailed,
		Detail: "managed content fetch failed: " + strings.Join(failures, "; ")}
}

func contentCandidates(repositoryPath, key string, peerIDs []string) []string {
	online := make([]string, 0, len(peerIDs))
	available := make([]string, 0, len(peerIDs))
	for _, peerID := range peerIDs {
		if peerAvailability.isOpen(repositoryPath, peerID) {
			continue
		}
		online = append(online, peerID)
		if !unavailableContent.isKnown(repositoryPath, peerID, key) {
			available = append(available, peerID)
		}
	}
	if len(available) == 0 {
		// Availability observations race with newly published annex objects.
		// Never let advisory negative caching eliminate every source.
		return online
	}
	return available
}

func optimizedPeerIDs(ctx context.Context, repo *repository.Repository, profile string) ([]string, error) {
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return nil, err
	}
	members, err := optimization.CurrentMembers(repo.Config.Repository, filesystemID, repo.Config.PeerID)
	if err != nil {
		return nil, err
	}
	state, err := optimization.LoadCurrent(repo.Config.Repository, filesystemID, repo.Config.PeerID)
	if err != nil {
		// Optimization is advisory: missing, corrupt, or peer-mismatched state
		// must never make content unavailable.
		state = optimization.State{}
	}
	return optimization.OrderedPeerIDs(state, profile, members, repo.Config.PeerID), nil
}

const unavailableContentTTL = 5 * time.Minute

var unavailableContent = &contentAvailability{entries: make(map[string]time.Time)}
var peerAvailability = &peerCircuit{entries: make(map[string]peerCircuitEntry)}
var contentSessions = &contentSessionPool{entries: make(map[string]*contentSession)}

type contentAvailability struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

type peerCircuitEntry struct {
	failures int
	until    time.Time
}

type peerCircuit struct {
	mu      sync.Mutex
	entries map[string]peerCircuitEntry
}

type ContentPeerState struct {
	PeerID     string    `json:"peer_id"`
	Status     string    `json:"status"`
	Failures   int       `json:"failures"`
	RetryAfter time.Time `json:"retry_after,omitempty"`
}

func ContentPeerStates(repositoryPath string) []ContentPeerState {
	peerAvailability.mu.Lock()
	defer peerAvailability.mu.Unlock()
	now := time.Now()
	var result []ContentPeerState
	prefix := repositoryPath + "\x00"
	for key, entry := range peerAvailability.entries {
		if !strings.HasPrefix(key, prefix) || !now.Before(entry.until) {
			continue
		}
		result = append(result, ContentPeerState{PeerID: strings.TrimPrefix(key, prefix), Status: "backoff",
			Failures: entry.failures, RetryAfter: entry.until.UTC()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PeerID < result[j].PeerID })
	return result
}

func peerStateKey(repositoryPath, peerID string) string {
	return repositoryPath + "\x00" + peerID
}

func (circuit *peerCircuit) isOpen(repositoryPath, peerID string) bool {
	circuit.mu.Lock()
	defer circuit.mu.Unlock()
	entry, found := circuit.entries[peerStateKey(repositoryPath, peerID)]
	if !found {
		return false
	}
	if time.Now().After(entry.until) {
		delete(circuit.entries, peerStateKey(repositoryPath, peerID))
		return false
	}
	return true
}

func (circuit *peerCircuit) markFailure(repositoryPath, peerID string) {
	circuit.mu.Lock()
	defer circuit.mu.Unlock()
	key := peerStateKey(repositoryPath, peerID)
	entry := circuit.entries[key]
	entry.failures++
	backoff := peerBackoffInitial << min(entry.failures-1, 5)
	if backoff > peerBackoffMaximum {
		backoff = peerBackoffMaximum
	}
	entry.until = time.Now().Add(backoff)
	circuit.entries[key] = entry
}

func (circuit *peerCircuit) markSuccess(repositoryPath, peerID string) {
	circuit.mu.Lock()
	defer circuit.mu.Unlock()
	delete(circuit.entries, peerStateKey(repositoryPath, peerID))
}

type contentSession struct {
	mu           sync.Mutex
	connection   *quic.Conn
	fingerprint  string
	target       membership.Record
	trustVersion string
}

type contentSessionPool struct {
	mu      sync.Mutex
	entries map[string]*contentSession
}

func CloseContentSessions(repositoryPath string) {
	contentSessions.closeRepository(repositoryPath)
}

func (pool *contentSessionPool) closeRepository(repositoryPath string) {
	pool.mu.Lock()
	prefix := repositoryPath + "\x00"
	var sessions []*contentSession
	for key, session := range pool.entries {
		if strings.HasPrefix(key, prefix) {
			sessions = append(sessions, session)
			delete(pool.entries, key)
		}
	}
	pool.mu.Unlock()
	for _, session := range sessions {
		session.mu.Lock()
		if session.connection != nil {
			_ = session.connection.CloseWithError(0, "repository closed")
			session.connection = nil
			session.fingerprint = ""
		}
		session.mu.Unlock()
	}
}

func (pool *contentSessionPool) session(repositoryPath, peerID string) *contentSession {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	key := peerStateKey(repositoryPath, peerID)
	session := pool.entries[key]
	if session == nil {
		session = &contentSession{}
		pool.entries[key] = session
	}
	return session
}

func (pool *contentSessionPool) connection(ctx context.Context, repo *repository.Repository, peerID string) (*quic.Conn, bool, error) {
	started := time.Now()
	trustVersion, versionErr := membership.TrustStateVersion(repo.Config.Repository)
	session := pool.session(repo.Config.Repository, peerID)
	session.mu.Lock()
	defer session.mu.Unlock()
	if versionErr == nil && session.connection != nil && session.connection.Context().Err() == nil && session.trustVersion == trustVersion {
		return session.connection, true, nil
	}
	target := session.target
	trustCached := versionErr == nil && target.Payload.PeerID == peerID && session.trustVersion == trustVersion
	var err error
	if !trustCached {
		target, err = trustedMember(repo.Config.Repository, peerID)
		if err == nil {
			session.target = target
			session.trustVersion = trustVersion
		}
	}
	repo.LogContentRead("content peer trust resolved", "peer_id", peerID, "cached", trustCached,
		"duration", time.Since(started), "error", err)
	if err != nil {
		if session.connection != nil {
			_ = session.connection.CloseWithError(1, "membership invalid")
			session.connection = nil
			session.fingerprint = ""
		}
		return nil, false, err
	}
	fingerprint := fmt.Sprintf("%s\x00%s\x00%d", target.Payload.QUICEndpoint, target.Payload.SigningPublicKey, target.Payload.Generation)
	if session.connection != nil {
		_ = session.connection.CloseWithError(0, "membership changed")
		session.connection = nil
	}
	connection, err := dialTrustedMember(ctx, repo, target)
	if err != nil {
		return nil, false, err
	}
	session.connection = connection
	session.fingerprint = fingerprint
	return connection, false, nil
}

func (pool *contentSessionPool) invalidate(repositoryPath, peerID string) {
	session := pool.session(repositoryPath, peerID)
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.connection != nil {
		_ = session.connection.CloseWithError(1, "content session failed")
		session.connection = nil
		session.fingerprint = ""
	}
}

func openContentStream(ctx context.Context, repo *repository.Repository, peerID string, request Request) (*quic.Stream, *bufio.Reader, Response, error) {
	started := time.Now()
	connection, reused, err := contentSessions.connection(ctx, repo, peerID)
	if err != nil {
		repo.LogContentRead("content connection failed", "peer_id", peerID, "operation", request.Operation,
			"duration", time.Since(started), "error", err)
		return nil, nil, Response{}, err
	}
	repo.LogContentRead("content stream created", "peer_id", peerID, "operation", request.Operation,
		"duration", time.Since(started))
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		contentSessions.invalidate(repo.Config.Repository, peerID)
		return nil, nil, Response{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	context.AfterFunc(ctx, func() {
		stream.CancelRead(1)
		stream.CancelWrite(1)
	})
	data, _ := json.Marshal(request)
	if _, err := stream.Write(append(data, '\n')); err != nil {
		stream.CancelRead(1)
		_ = stream.Close()
		contentSessions.invalidate(repo.Config.Repository, peerID)
		return nil, nil, Response{}, err
	}
	repo.LogContentRead("content request written", "peer_id", peerID, "operation", request.Operation,
		"duration", time.Since(started))
	reader := bufio.NewReader(stream)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		stream.CancelRead(1)
		_ = stream.Close()
		contentSessions.invalidate(repo.Config.Repository, peerID)
		return nil, nil, Response{}, err
	}
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		_ = stream.Close()
		contentSessions.invalidate(repo.Config.Repository, peerID)
		return nil, nil, Response{}, err
	}
	if !response.OK {
		_ = stream.Close()
		return nil, nil, response, errors.New(response.Error)
	}
	repo.LogContentReadDebug("content stream opened", "peer_id", peerID, "operation", request.Operation,
		"connection_reused", reused, "duration", time.Since(started))
	return stream, reader, response, nil
}

func (availability *contentAvailability) key(repositoryPath, peerID, key string) string {
	return repositoryPath + "\x00" + peerID + "\x00" + key
}

func (availability *contentAvailability) isKnown(repositoryPath, peerID, key string) bool {
	availability.mu.Lock()
	defer availability.mu.Unlock()
	cacheKey := availability.key(repositoryPath, peerID, key)
	observed, found := availability.entries[cacheKey]
	if found && time.Since(observed) >= unavailableContentTTL {
		delete(availability.entries, cacheKey)
		return false
	}
	return found
}

func (availability *contentAvailability) mark(repositoryPath, peerID, key string) {
	availability.mu.Lock()
	defer availability.mu.Unlock()
	availability.entries[availability.key(repositoryPath, peerID, key)] = time.Now()
}

func (availability *contentAvailability) clear(repositoryPath, peerID, key string) {
	availability.mu.Lock()
	defer availability.mu.Unlock()
	delete(availability.entries, availability.key(repositoryPath, peerID, key))
}

func isUnavailableContent(err error) bool {
	return err != nil && strings.Contains(err.Error(), "annex content is unavailable")
}

func Probe(ctx context.Context, repo *repository.Repository, peerID string) error {
	connection, stream, _, _, err := Open(ctx, repo, peerID, Request{Operation: "ping"})
	if err != nil {
		peerAvailability.markFailure(repo.Config.Repository, peerID)
		return err
	}
	_ = stream.Close()
	err = connection.CloseWithError(0, "")
	if err == nil {
		peerAvailability.markSuccess(repo.Config.Repository, peerID)
	}
	return err
}

func writeResponse(writer io.Writer, response Response) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}

func certificate(private ed25519.PrivateKey, peerID string) (tls.Certificate, error) {
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: peerID},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, private.Public(), private)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}, nil
}

func trustedMember(repositoryPath, peerID string) (membership.Record, error) {
	trusted, err := membership.LoadTrusted(repositoryPath)
	if err != nil {
		return membership.Record{}, err
	}
	records, err := membership.LoadAll(repositoryPath)
	if err != nil {
		return membership.Record{}, fmt.Errorf("load DFS membership: %w", err)
	}
	var target membership.Record
	found := false
	for _, record := range records {
		if record.Payload.PeerID == peerID && trusted[peerID] == record.Payload.SigningPublicKey {
			target = record
			found = true
			break
		}
	}
	if !found || target.Payload.Revoked {
		return membership.Record{}, fmt.Errorf("peer %s is not in trusted DFS membership", peerID)
	}
	revoked, err := membership.AcceptedRevocations(repositoryPath, target.Payload.FileSystemID)
	if err != nil {
		return membership.Record{}, fmt.Errorf("load DFS membership revocations: %w", err)
	}
	if !revoked[peerID] {
		return target, nil
	}
	return membership.Record{}, fmt.Errorf("peer %s is not in trusted DFS membership", peerID)
}

func verifyTrustedPublicKey(repositoryPath string, public ed25519.PublicKey) error {
	trusted, err := membership.LoadTrusted(repositoryPath)
	if err != nil {
		return err
	}
	wanted := base64Public(public)
	for peerID, key := range trusted {
		if key != wanted {
			continue
		}
		revoked, err := membership.IsRevoked(repositoryPath, peerID)
		if err != nil {
			return err
		}
		if !revoked {
			return nil
		}
	}
	return errors.New("client certificate is not in trusted DFS membership")
}

func base64Public(public ed25519.PublicKey) string {
	return base64.RawStdEncoding.EncodeToString(public)
}
