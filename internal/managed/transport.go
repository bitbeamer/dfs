package managed

import (
	"bufio"
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
	"strings"
	"sync"
	"time"

	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/repository"
	quic "github.com/quic-go/quic-go"
)

const ALPN = "dfs-managed-v1"
const PairALPN = "dfs-pair-v2"

type Request struct {
	Operation string          `json:"operation"`
	Service   string          `json:"service,omitempty"`
	Key       string          `json:"key,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type Response struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Size    int64           `json:"size,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Server struct {
	repo       *repository.Repository
	listener   *quic.Listener
	diagnostic func(context.Context) ([]byte, error)
	pair       func(context.Context, string, net.Addr, json.RawMessage) (json.RawMessage, error)
	stop       context.CancelFunc
	done       chan struct{}
	once       sync.Once
}

func Start(repo *repository.Repository, address string, diagnostic func(context.Context) ([]byte, error), pairingCertificate *tls.Certificate, pair func(context.Context, string, net.Addr, json.RawMessage) (json.RawMessage, error)) (*Server, error) {
	private, _, err := membership.EnsureKey(repo.Config.Repository)
	if err != nil {
		return nil, err
	}
	certificate, err := certificate(private, repo.Config.PeerID)
	if err != nil {
		return nil, err
	}
	filesystemID, err := repo.FileSystemID(context.Background())
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
		return verifyTrustedPublicKey(repo.Config.Repository, filesystemID, public)
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
	server := &Server{repo: repo, listener: listener, diagnostic: diagnostic, pair: pair, stop: cancel, done: make(chan struct{})}
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
	reader := bufio.NewReaderSize(stream, 64<<10)
	header, err := reader.ReadBytes('\n')
	if err != nil || len(header) > 64<<10 {
		writeResponse(stream, Response{Error: "read managed transport request"})
		return
	}
	var request Request
	if err := json.Unmarshal(header, &request); err != nil {
		writeResponse(stream, Response{Error: "decode managed transport request"})
		return
	}
	if protocol == PairALPN {
		if s.pair == nil || (request.Operation != "pair-start" && request.Operation != "pair-complete") {
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
	switch request.Operation {
	case "ping":
		writeResponse(stream, Response{OK: true})
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
	case "git":
		s.serveGit(stream, reader, request.Service)
	case "annex-get":
		s.serveContent(stream, request.Key)
	default:
		writeResponse(stream, Response{Error: "unsupported managed transport operation"})
	}
}

func PairCall(ctx context.Context, address, certificateSHA256, operation string, input, output any) error {
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

func (s *Server) serveGit(stream *quic.Stream, input io.Reader, service string) {
	if service != "git-upload-pack" && service != "git-receive-pack" && service != "git-upload-archive" {
		writeResponse(stream, Response{Error: "unsupported Git service"})
		return
	}
	command := exec.CommandContext(stream.Context(), service, s.repo.Config.Repository)
	command.Stdin, command.Stdout, command.Stderr = input, stream, io.Discard
	if err := command.Start(); err != nil {
		writeResponse(stream, Response{Error: err.Error()})
		return
	}
	if err := writeResponse(stream, Response{OK: true}); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return
	}
	_ = command.Wait()
}

func (s *Server) serveContent(stream *quic.Stream, key string) {
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
	if err := writeResponse(stream, Response{OK: true, Size: info.Size()}); err != nil {
		return
	}
	_, _ = io.CopyN(stream, file, info.Size())
}

func Dial(ctx context.Context, repo *repository.Repository, peerID string) (*quic.Conn, membership.Record, error) {
	target, err := trustedMember(repo.Config.Repository, peerID)
	if err != nil {
		return nil, membership.Record{}, err
	}
	private, _, err := membership.EnsureKey(repo.Config.Repository)
	if err != nil {
		return nil, membership.Record{}, err
	}
	clientCertificate, err := certificate(private, repo.Config.PeerID)
	if err != nil {
		return nil, membership.Record{}, err
	}
	endpoint, err := url.Parse(target.Payload.QUICEndpoint)
	if err != nil || endpoint.Host == "" {
		return nil, membership.Record{}, errors.New("invalid member QUIC endpoint")
	}
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
	dialContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	connection, err := quic.DialAddr(dialContext, endpoint.Host, tlsConfig, &quic.Config{
		HandshakeIdleTimeout: 3 * time.Second, MaxIdleTimeout: 2 * time.Minute, KeepAlivePeriod: 20 * time.Second,
	})
	return connection, target, err
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

// GitProxy connects Git's remote-ext protocol to a managed QUIC stream. If
// QUIC cannot be established before any Git input is consumed, the same
// repository-restricted operation is retried over SSH.
func GitProxy(ctx context.Context, repo *repository.Repository, peerID, service string, input io.Reader, output, errorOutput io.Writer) (string, error) {
	connection, stream, reader, _, err := Open(ctx, repo, peerID, Request{Operation: "git", Service: service})
	if err != nil {
		if fallbackErr := sshGitProxy(ctx, repo, peerID, service, input, output, errorOutput); fallbackErr != nil {
			return "", fmt.Errorf("managed QUIC transport failed: %v; SSH fallback failed: %w", err, fallbackErr)
		}
		return "ssh", nil
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
	connection, stream, reader, response, err := Open(ctx, repo, peerID, Request{Operation: "annex-get", Key: key})
	if err != nil {
		return 0, err
	}
	defer connection.CloseWithError(0, "")
	defer stream.Close()
	written, err := io.CopyN(output, reader, response.Size)
	return written, err
}

func FetchPath(ctx context.Context, repo *repository.Repository, path, from string) error {
	key, err := repo.LookupKey(ctx, path)
	if err != nil {
		return err
	}
	trusted, err := membership.LoadTrusted(repo.Config.Repository)
	if err != nil {
		return err
	}
	wantedPrefix := strings.TrimPrefix(from, "dfs-peer-")
	var peerIDs []string
	records, err := membership.LoadAll(repo.Config.Repository)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Payload.PeerID == repo.Config.PeerID || trusted[record.Payload.PeerID] != record.Payload.SigningPublicKey {
			continue
		}
		if wantedPrefix == "" || strings.HasPrefix(record.Payload.PeerID, wantedPrefix) {
			peerIDs = append(peerIDs, record.Payload.PeerID)
		}
	}
	if len(peerIDs) == 0 {
		return errors.New("no trusted managed content source is available")
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
		closeErr := temporary.Close()
		if fetchErr == nil {
			fetchErr = closeErr
		}
		if fetchErr == nil {
			fetchErr = repo.ReinjectContent(ctx, temporaryPath, path)
		}
		_ = os.Remove(temporaryPath)
		if fetchErr == nil {
			return nil
		}
		failures = append(failures, peerID+": "+fetchErr.Error())
	}
	return fmt.Errorf("managed content fetch failed: %s", strings.Join(failures, "; "))
}

func Probe(ctx context.Context, repo *repository.Repository, peerID string) error {
	connection, stream, _, _, err := Open(ctx, repo, peerID, Request{Operation: "ping"})
	if err != nil {
		return err
	}
	_ = stream.Close()
	return connection.CloseWithError(0, "")
}

func sshGitProxy(ctx context.Context, repo *repository.Repository, peerID, service string, input io.Reader, output, errorOutput io.Writer) error {
	target, err := trustedMember(repo.Config.Repository, peerID)
	if err != nil {
		return err
	}
	endpoint, err := url.Parse(target.Payload.SSH.Endpoint)
	if err != nil || endpoint.User == nil || endpoint.Hostname() == "" || endpoint.Path == "" {
		return errors.New("invalid SSH fallback endpoint")
	}
	if service != "git-upload-pack" && service != "git-receive-pack" && service != "git-upload-archive" {
		return errors.New("unsupported Git service")
	}
	stateDirectory := filepath.Join(repo.Config.Repository, ".git", "dfs")
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectTimeout=10", "-i", filepath.Join(stateDirectory, "peer-ssh-key"), "-o", "UserKnownHostsFile=" + filepath.Join(stateDirectory, "known_hosts")}
	if endpoint.Port() != "" {
		args = append(args, "-p", endpoint.Port())
	}
	destination := endpoint.User.Username() + "@" + endpoint.Hostname()
	args = append(args, destination, service+" "+shellQuote(endpoint.Path))
	command := exec.CommandContext(ctx, "ssh", args...)
	command.Stdin, command.Stdout, command.Stderr = input, output, errorOutput
	return command.Run()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
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
	for _, record := range records {
		revoked, err := membership.AcceptedRevocations(repositoryPath, record.Payload.FileSystemID)
		if err != nil {
			return membership.Record{}, fmt.Errorf("load DFS membership revocations: %w", err)
		}
		if record.Payload.PeerID == peerID && trusted[peerID] == record.Payload.SigningPublicKey && !record.Payload.Revoked && !revoked[peerID] {
			return record, nil
		}
	}
	return membership.Record{}, fmt.Errorf("peer %s is not in trusted DFS membership", peerID)
}

func verifyTrustedPublicKey(repositoryPath, filesystemID string, public ed25519.PublicKey) error {
	trusted, err := membership.LoadTrusted(repositoryPath)
	if err != nil {
		return err
	}
	wanted := base64Public(public)
	revoked, err := membership.AcceptedRevocations(repositoryPath, filesystemID)
	if err != nil {
		return err
	}
	records, err := membership.LoadAll(repositoryPath)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Payload.FileSystemID == filesystemID && !revoked[record.Payload.PeerID] && trusted[record.Payload.PeerID] == wanted && record.Payload.SigningPublicKey == wanted {
			return nil
		}
	}
	return errors.New("client certificate is not in trusted DFS membership")
}

func base64Public(public ed25519.PublicKey) string {
	return base64.RawStdEncoding.EncodeToString(public)
}
