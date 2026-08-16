package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/repository"
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
	repo             *repository.Repository
	logger           *slog.Logger
	filesystemID     string
	fingerprint      string
	identity         transportIdentity
	membershipKey    ed25519.PrivateKey
	membershipRecord membership.Record
	endorsements     []membership.Endorsement
	listener         net.Listener
	managed          *managed.Server
	httpServer       *http.Server
	mdnsServers      []mdnsAdvertiser
	statePath        string
	mu               sync.Mutex
	attempts         map[string]attemptWindow
	done             chan struct{}
	cleanupStop      chan struct{}
	cleanupDone      chan struct{}
}

type attemptWindow struct {
	Started time.Time
	Count   int
}

func Start(repo *repository.Repository, logger *slog.Logger, port int, changed func(string, []string)) (*Service, error) {
	if port == 0 {
		port = DefaultPairingPort
	} else if port < 0 {
		port = 0
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen for DFS pairing on TCP port %d: %w", port, err)
	}
	service, err := startService(repo, logger, listener, true, changed)
	if err != nil {
		_ = listener.Close()
	}
	return service, err
}

func startService(repo *repository.Repository, logger *slog.Logger, listener net.Listener, advertise bool, changed func(string, []string)) (*Service, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return nil, err
	}
	if err := membership.MigrateLegacySharedState(repo.Config.Repository); err != nil {
		return nil, fmt.Errorf("migrate DFS membership metadata: %w", err)
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
	port := listener.Addr().(*net.TCPAddr).Port
	membershipKey, membershipRecord, err := ensureLocalMembership(ctx, repo, filesystemID, identity, port)
	if err != nil {
		return nil, fmt.Errorf("prepare DFS membership: %w", err)
	}
	accepted, err := acceptedMembership(ctx, repo, filesystemID)
	if err != nil {
		return nil, fmt.Errorf("load accepted DFS membership: %w", err)
	}
	var endorsements []membership.Endorsement
	for _, record := range accepted {
		endorsement, endorseErr := membership.Endorse(record, repo.Config.PeerID, membershipKey)
		if endorseErr != nil {
			return nil, fmt.Errorf("endorse DFS membership: %w", endorseErr)
		}
		endorsements = append(endorsements, endorsement)
	}
	service := &Service{
		repo: repo, logger: logger.With("component", "peer-network"), filesystemID: filesystemID,
		fingerprint: fingerprint, identity: identity, membershipKey: membershipKey, membershipRecord: membershipRecord, endorsements: endorsements, listener: tls.NewListener(listener, &tls.Config{
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
	pairHandler := func(ctx context.Context, operation string, remote net.Addr, payload json.RawMessage) (json.RawMessage, error) {
		path := "/v1/pair/start"
		handler := service.handlePairStart
		if operation == "pair-complete" {
			path = "/v1/pair/complete"
			handler = service.handlePairComplete
		}
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(payload))
		request.RemoteAddr = remote.String()
		response := httptest.NewRecorder()
		handler(response, request)
		if response.Code < 200 || response.Code >= 300 {
			var failure protocolError
			if json.Unmarshal(response.Body.Bytes(), &failure) == nil && failure.Error != "" {
				return nil, errors.New(failure.Error)
			}
			return nil, fmt.Errorf("pairing returned HTTP %d", response.Code)
		}
		return append(json.RawMessage(nil), response.Body.Bytes()...), nil
	}
	managedService, err := managed.Start(repo, listener.Addr().String(), func(ctx context.Context) ([]byte, error) {
		report, err := Diagnose(ctx, repo, 10*time.Second)
		if err != nil {
			return nil, err
		}
		return json.Marshal(report)
	}, &certificate, pairHandler, changed)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("start DFS managed QUIC transport: %w", err)
	}
	service.managed = managedService
	if advertise {
		interfaces := interfaceProvider()
		if len(interfaces) == 0 {
			_ = service.managed.Close()
			_ = listener.Close()
			return nil, errors.New("advertise DFS network: no multicast-capable interface")
		}
		txt := []string{
			"v=" + strconv.Itoa(ProtocolVersion), "fs=" + filesystemID,
			"network=" + encodeTXT(repo.Config.NetworkName), "peer=" + repo.Config.PeerID,
			"name=" + encodeTXT(repo.Config.Name), "cert=" + fingerprint, "pair=invite",
		}
		service.mdnsServers, err = startMDNSAdvertisers(
			serviceInstance(repo.Config.NetworkName, repo.Config.Name, repo.Config.PeerID), port, txt, interfaces,
		)
		if err != nil {
			_ = service.managed.Close()
			_ = listener.Close()
			return nil, fmt.Errorf("advertise DFS network: %w", err)
		}
	}
	endpoint := runtimeEndpoint(port)
	if err := writeRuntimeState(service.statePath, runtimeState{
		Version: ProtocolVersion, FileSystemID: filesystemID, Endpoint: endpoint,
		CertificateSHA: fingerprint, PID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		_ = service.managed.Close()
		for _, server := range service.mdnsServers {
			server.Shutdown()
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
		server.Shutdown()
	}
	close(s.cleanupStop)
	<-s.cleanupDone
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.httpServer.Shutdown(ctx)
	if s.managed != nil {
		if managedErr := s.managed.Close(); err == nil {
			err = managedErr
		}
	}
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
	if err := validatePairingMembership(input.Membership, s.filesystemID, input.PeerID, input.PeerName, input.SSHPublicKey, input.ReversePath); err != nil {
		writeProtocolError(response, http.StatusBadRequest, "valid signed peer membership is required")
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
	if err := s.refreshEndorsements(request.Context()); err != nil {
		writeProtocolError(response, http.StatusInternalServerError, "cannot load approved membership")
		return
	}
	if record.Pending != nil && !record.Pending.ExpiresAt.After(time.Now()) {
		if err := removeAuthorizedMarker(record.Pending.AuthorizedMarker); err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot release expired pairing session")
			return
		}
		record.Pending = nil
	}
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
		record.Pending.ExpiresAt = minTime(record.ExpiresAt, time.Now().UTC().Add(pairingLease))
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
	approvedMembership, err := membership.Approve(input.Membership, s.repo.Config.PeerID, s.membershipKey)
	if err != nil {
		writeProtocolError(response, http.StatusInternalServerError, "cannot approve peer membership")
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
		ExpiresAt:        minTime(record.ExpiresAt, time.Now().UTC().Add(pairingLease)),
		Membership:       approvedMembership,
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
		Membership: pending.Membership, Approver: s.membershipRecord, Endorsements: s.endorsements,
	}
}

func (s *Service) refreshEndorsements(ctx context.Context) error {
	accepted, err := acceptedMembership(ctx, s.repo, s.filesystemID)
	if err != nil {
		return err
	}
	endorsements := make([]membership.Endorsement, 0, len(accepted))
	for _, record := range accepted {
		endorsement, err := membership.Endorse(record, s.repo.Config.PeerID, s.membershipKey)
		if err != nil {
			return err
		}
		endorsements = append(endorsements, endorsement)
	}
	s.endorsements = endorsements
	return nil
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
		if err := membership.VerifyApproval(pending.Membership, s.membershipRecord); err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "approved membership is invalid")
			return
		}
		if err := membership.Save(s.repo.Config.Repository, pending.Membership); err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot publish peer membership")
			return
		}
		if err := membership.Trust(s.repo.Config.Repository, pending.Membership.Payload.PeerID, pending.Membership.Payload.SigningPublicKey); err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot trust approved peer membership")
			return
		}
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
	return strings.TrimSuffix(mdnsHostname(host), ".")
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

func interfaceIPStrings(networkInterface *net.Interface) []string {
	addresses, _ := networkInterface.Addrs()
	var result []string
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
		result = append(result, ip.String())
	}
	return result
}

func localMDNSHostname() string {
	host, _ := os.Hostname()
	return mdnsHostname(host)
}

func mdnsHostname(host string) string {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if strings.HasSuffix(strings.ToLower(host), ".local") {
		return host + "."
	}
	return host + ".local."
}

func runtimeEndpoint(port int) string {
	return "https://" + net.JoinHostPort(strings.TrimSuffix(localMDNSHostname(), "."), strconv.Itoa(port))
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
