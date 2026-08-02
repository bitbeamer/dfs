package peer

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/hashicorp/mdns"
)

const runtimeStateFile = "network.json"

type runtimeState struct {
	Version        int       `json:"version"`
	FileSystemID   string    `json:"filesystem_id"`
	Endpoint       string    `json:"endpoint"`
	CertificateSHA string    `json:"certificate_sha256"`
	PID            int       `json:"pid"`
	StartedAt      time.Time `json:"started_at"`
}

type Service struct {
	repo         *repository.Repository
	logger       *slog.Logger
	filesystemID string
	fingerprint  string
	identity     transportIdentity
	listener     net.Listener
	httpServer   *http.Server
	mdnsServers  []*mdns.Server
	statePath    string
	mu           sync.Mutex
	attempts     map[string]attemptWindow
	done         chan struct{}
	cleanupStop  chan struct{}
	cleanupDone  chan struct{}
}

type attemptWindow struct {
	Started time.Time
	Count   int
}

func Start(repo *repository.Repository, logger *slog.Logger, port int) (*Service, error) {
	if port == 0 {
		port = DefaultPairingPort
	} else if port < 0 {
		port = 0
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen for DFS pairing on TCP port %d: %w", port, err)
	}
	service, err := startService(repo, logger, listener, true)
	if err != nil {
		_ = listener.Close()
	}
	return service, err
}

func startService(repo *repository.Repository, logger *slog.Logger, listener net.Listener, advertise bool) (*Service, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return nil, err
	}
	certificate, fingerprint, err := loadOrCreateCertificate(repo.Config.Repository)
	if err != nil {
		return nil, err
	}
	identity, err := ensureRepositoryTransport(ctx, repo)
	if err != nil {
		return nil, err
	}
	if listener == nil {
		listener, err = net.Listen("tcp", ":0")
		if err != nil {
			return nil, fmt.Errorf("listen for DFS pairing: %w", err)
		}
	}
	service := &Service{
		repo: repo, logger: logger.With("component", "peer-network"), filesystemID: filesystemID,
		fingerprint: fingerprint, identity: identity, listener: tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13,
		}),
		statePath: filepath.Join(repo.Config.Repository, filepath.FromSlash(config.Directory), runtimeStateFile),
		attempts:  make(map[string]attemptWindow), done: make(chan struct{}),
		cleanupStop: make(chan struct{}), cleanupDone: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pair/start", service.handlePairStart)
	mux.HandleFunc("/v1/pair/complete", service.handlePairComplete)
	service.httpServer = &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if advertise {
		interfaces := interfaceProvider()
		if len(interfaces) == 0 {
			_ = listener.Close()
			return nil, errors.New("advertise DFS network: no multicast-capable interface")
		}
		for _, networkInterface := range interfaces {
			zone, zoneErr := mdns.NewMDNSService(
				serviceInstance(repo.Config.NetworkName, repo.Config.Name, repo.Config.PeerID), ServiceType, "", "", port,
				interfaceIPs(networkInterface), []string{
					"v=" + strconv.Itoa(ProtocolVersion), "fs=" + filesystemID,
					"network=" + encodeTXT(repo.Config.NetworkName), "peer=" + repo.Config.PeerID,
					"name=" + encodeTXT(repo.Config.Name), "cert=" + fingerprint, "pair=invite",
				},
			)
			if zoneErr != nil {
				continue
			}
			server, serverErr := mdns.NewServer(&mdns.Config{Zone: zone, Iface: networkInterface, Logger: log.New(io.Discard, "", 0)})
			if serverErr == nil {
				service.mdnsServers = append(service.mdnsServers, server)
			}
		}
		if len(service.mdnsServers) == 0 {
			_ = listener.Close()
			return nil, errors.New("advertise DFS network: no multicast listener could be started")
		}
	}
	endpoint := runtimeEndpoint(port)
	if err := writeRuntimeState(service.statePath, runtimeState{
		Version: ProtocolVersion, FileSystemID: filesystemID, Endpoint: endpoint,
		CertificateSHA: fingerprint, PID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		for _, server := range service.mdnsServers {
			_ = server.Shutdown()
		}
		_ = listener.Close()
		return nil, err
	}
	go func() {
		err := service.httpServer.Serve(service.listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			service.logger.Error("pairing service stopped unexpectedly", "error", err)
		}
		close(service.done)
	}()
	go service.cleanupInvitations()
	service.logger.Info("peer discovery ready", "filesystem_id", filesystemID, "endpoint", endpoint)
	return service, nil
}

func (s *Service) Close() error {
	for _, server := range s.mdnsServers {
		_ = server.Shutdown()
	}
	close(s.cleanupStop)
	<-s.cleanupDone
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.httpServer.Shutdown(ctx)
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	if state, stateErr := readRuntimeState(s.repo.Config.Repository); stateErr == nil && state.PID == os.Getpid() {
		_ = os.Remove(s.statePath)
	}
	return err
}

func (s *Service) handlePairStart(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeProtocolError(response, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var input PairStartRequest
	if err := decodeRequest(response, request, &input); err != nil {
		return
	}
	if !validPeerID(input.PeerID) || strings.TrimSpace(input.PeerName) == "" {
		writeProtocolError(response, http.StatusBadRequest, "valid peer ID and name are required")
		return
	}
	if _, err := normalizePublicKey(input.SSHPublicKey); err != nil {
		writeProtocolError(response, http.StatusBadRequest, "valid peer SSH public key is required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	remote := remoteAddress(request)
	if !s.allowPairingAttempt(remote, time.Now()) {
		writeProtocolError(response, http.StatusTooManyRequests, "too many pairing attempts; try again later")
		return
	}
	record, err := loadInvitation(s.repo.Config.Repository, input.InvitationID)
	if err != nil || record.FileSystemID != s.filesystemID || !record.ExpiresAt.After(time.Now()) ||
		subtle.ConstantTimeCompare([]byte(record.SecretHash), []byte(secretHash(input.Secret))) != 1 {
		s.recordPairingFailure(remote, time.Now())
		s.logger.Warn("pairing request rejected", "remote", remote)
		writeProtocolError(response, http.StatusUnauthorized, "invalid or expired pairing invitation")
		return
	}
	delete(s.attempts, remote)
	if record.Pending != nil {
		if record.Pending.PeerID != input.PeerID {
			writeProtocolError(response, http.StatusConflict, "pairing invitation is already in use")
			return
		}
		completionSecret, secretErr := randomString(32)
		if secretErr != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot refresh pairing session")
			return
		}
		record.Pending.CompletionHash = secretHash(completionSecret)
		if err := saveInvitation(s.repo.Config.Repository, record); err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot refresh pairing session")
			return
		}
		writeJSON(response, http.StatusOK, s.startResponse(record.Pending, completionSecret))
		return
	}
	cloneURL := record.CloneURL
	if cloneURL == "" {
		account, userErr := user.Current()
		if userErr != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot determine DFS account")
			return
		}
		cloneURL, err = sshURL(account.Username, localAddress(request), s.repo.Config.Repository)
		if err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot build peer URL")
			return
		}
	}
	completionSecret, err := randomString(32)
	if err != nil {
		writeProtocolError(response, http.StatusInternalServerError, "cannot create pairing session")
		return
	}
	sessionID, err := randomString(12)
	if err != nil {
		writeProtocolError(response, http.StatusInternalServerError, "cannot create pairing session")
		return
	}
	var reverseURL string
	if input.ReverseUser != "" && input.ReversePath != "" {
		reverseURL, err = sshURL(input.ReverseUser, remoteAddress(request), input.ReversePath)
		if err != nil {
			writeProtocolError(response, http.StatusBadRequest, "invalid reverse peer details")
			return
		}
		knownHosts := filepath.Join(s.repo.Config.Repository, filepath.FromSlash(config.Directory), "known_hosts")
		if err := installKnownHosts(knownHosts, reverseURL, input.SSHHostKeys); err != nil {
			writeProtocolError(response, http.StatusBadRequest, "cannot pin reverse peer SSH host keys")
			return
		}
		if err := s.repo.ConfigureSSHCommand(request.Context(), transportSSHCommand(s.identity.PrivateKey, knownHosts)); err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot configure reverse peer SSH transport")
			return
		}
	}
	authorizedMarker, err := authorizePeer(input.SSHPublicKey, s.repo.Config.Repository, s.filesystemID, input.PeerID)
	if err != nil {
		writeProtocolError(response, http.StatusInternalServerError, "cannot authorize paired peer transport")
		return
	}
	record.Pending = &pendingPair{
		SessionID: sessionID, CompletionHash: secretHash(completionSecret), PeerID: input.PeerID,
		PeerName: strings.TrimSpace(input.PeerName), ReverseURL: reverseURL, CloneURL: cloneURL,
		AuthorizedMarker: authorizedMarker,
		ExpiresAt:        minTime(record.ExpiresAt, time.Now().UTC().Add(30*time.Minute)),
	}
	if err := saveInvitation(s.repo.Config.Repository, record); err != nil {
		_ = removeAuthorizedMarker(authorizedMarker)
		writeProtocolError(response, http.StatusInternalServerError, "cannot persist pairing session")
		return
	}
	writeJSON(response, http.StatusOK, s.startResponse(record.Pending, completionSecret))
}

func (s *Service) startResponse(pending *pendingPair, completionSecret string) PairStartResponse {
	return PairStartResponse{
		Version: ProtocolVersion, FileSystemID: s.filesystemID, NetworkName: s.repo.Config.NetworkName,
		PeerName: s.repo.Config.Name, PeerID: s.repo.Config.PeerID, CloneURL: pending.CloneURL,
		SSHPublicKey: s.identity.PublicKey, SSHHostKeys: localSSHHostKeys(), SessionID: pending.SessionID,
		CompletionSecret: completionSecret, ExpiresAt: pending.ExpiresAt,
	}
}

func (s *Service) handlePairComplete(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeProtocolError(response, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var input PairCompleteRequest
	if err := decodeRequest(response, request, &input); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	infos, err := ListInvitations(s.repo.Config.Repository, time.Now())
	if err != nil {
		writeProtocolError(response, http.StatusInternalServerError, "cannot inspect pairing sessions")
		return
	}
	for _, info := range infos {
		record, loadErr := loadInvitation(s.repo.Config.Repository, info.ID)
		if loadErr != nil || record.Pending == nil || record.Pending.SessionID != input.SessionID {
			continue
		}
		pending := record.Pending
		if !pending.ExpiresAt.After(time.Now()) || subtle.ConstantTimeCompare([]byte(pending.CompletionHash), []byte(secretHash(input.CompletionSecret))) != 1 {
			break
		}
		remoteName := ""
		if pending.ReverseURL != "" {
			ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
			remoteName, err = s.repo.AddPairedRemote(ctx, pending.PeerID, pending.ReverseURL)
			cancel()
			if err != nil {
				writeProtocolError(response, http.StatusInternalServerError, "cannot configure reverse peer")
				return
			}
		}
		if err := os.Remove(invitationPath(s.repo.Config.Repository, record.ID)); err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot finalize pairing invitation")
			return
		}
		s.logger.Info("peer paired", "peer", pending.PeerName, "peer_id", pending.PeerID, "remote", remoteName)
		writeJSON(response, http.StatusOK, PairCompleteResponse{RemoteName: remoteName})
		return
	}
	writeProtocolError(response, http.StatusUnauthorized, "invalid or expired pairing session")
}

func (s *Service) cleanupInvitations() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	defer close(s.cleanupDone)
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			_, err := ListInvitations(s.repo.Config.Repository, time.Now())
			s.mu.Unlock()
			if err != nil {
				s.logger.Warn("cleaning expired pairing invitations failed", "error", err)
			}
		case <-s.cleanupStop:
			return
		}
	}
}

func decodeRequest(response http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeProtocolError(response, http.StatusBadRequest, "invalid JSON request")
		return err
	}
	return nil
}

func writeProtocolError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, protocolError{Error: message})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func sshURL(username, host, path string) (string, error) {
	username = strings.TrimSpace(username)
	host = strings.TrimSpace(host)
	if username == "" || host == "" || !filepath.IsAbs(path) {
		return "", errors.New("SSH user, host, and absolute repository path are required")
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.To4() == nil {
		host = "[" + strings.Trim(host, "[]") + "]"
	}
	return (&url.URL{Scheme: "ssh", User: url.User(username), Host: host, Path: filepath.ToSlash(path)}).String(), nil
}

func localAddress(request *http.Request) string {
	if address, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if host, _, err := net.SplitHostPort(address.String()); err == nil && host != "" && !net.ParseIP(host).IsUnspecified() {
			return host
		}
	}
	host, _ := os.Hostname()
	return host + ".local"
}

func remoteAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}

func validPeerID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, character := range strings.ToLower(value) {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func serviceInstance(network, peer, peerID string) string {
	shortID := peerID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	value := strings.TrimSpace(network + " on " + peer + " " + shortID)
	if len(value) > 60 {
		value = value[:60]
	}
	return value
}

func (s *Service) allowPairingAttempt(remote string, now time.Time) bool {
	window, found := s.attempts[remote]
	if !found || now.Sub(window.Started) >= time.Minute {
		return true
	}
	return window.Count < 10
}

func (s *Service) recordPairingFailure(remote string, now time.Time) {
	window, found := s.attempts[remote]
	if !found || now.Sub(window.Started) >= time.Minute {
		window = attemptWindow{Started: now}
	}
	window.Count++
	s.attempts[remote] = window
}

func interfaceIPs(networkInterface *net.Interface) []net.IP {
	addresses, _ := networkInterface.Addrs()
	var result []net.IP
	for _, address := range addresses {
		var ip net.IP
		switch value := address.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		result = append(result, ip)
	}
	return result
}

func runtimeEndpoint(port int) string {
	host, _ := os.Hostname()
	return "https://" + net.JoinHostPort(host+".local", strconv.Itoa(port))
}

func writeRuntimeState(path string, state runtimeState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write peer network state: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish peer network state: %w", err)
	}
	return nil
}

func readRuntimeState(repositoryPath string) (runtimeState, error) {
	data, err := os.ReadFile(filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), runtimeStateFile))
	if err != nil {
		return runtimeState{}, err
	}
	var state runtimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return runtimeState{}, err
	}
	return state, nil
}
