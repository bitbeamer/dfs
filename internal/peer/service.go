package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	membershipKey    ed25519.PrivateKey
	membershipRecord membership.Record
	members          []membership.Record
	endorsements     []membership.Endorsement
	managed          *managed.Server
	mdnsServers      []mdnsAdvertiser
	statePath        string
	mu               sync.Mutex
	attempts         map[string]attemptWindow
	cleanupStop      chan struct{}
	cleanupDone      chan struct{}
	reconcile        func(context.Context, *repository.Repository) error
	runBackground    func(func())
	backgroundMu     sync.Mutex
	background       sync.WaitGroup
	closing          bool
}

type attemptWindow struct {
	Started time.Time
	Count   int
}

func Start(repo *repository.Repository, logger *slog.Logger, port int, changed func(string, []string)) (*Service, error) {
	return StartWithDiscovery(repo, logger, port, true, changed)
}

func StartWithDiscovery(repo *repository.Repository, logger *slog.Logger, port int, advertise bool, changed func(string, []string)) (*Service, error) {
	if port == 0 {
		port = DefaultPairingPort
	} else if port < 0 {
		port = 0
	}
	return startServiceAddress(repo, logger, net.JoinHostPort("", strconv.Itoa(port)), advertise, changed)
}

func startService(repo *repository.Repository, logger *slog.Logger, listener net.Listener, advertise bool, changed func(string, []string)) (*Service, error) {
	if listener == nil {
		return nil, errors.New("test transport listener is required")
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return nil, err
	}
	return startServiceAddress(repo, logger, address, advertise, changed)
}

func startServiceAddress(repo *repository.Repository, logger *slog.Logger, address string, advertise bool, changed func(string, []string)) (*Service, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return nil, err
	}
	if err := removeLegacyAuthorizations(filesystemID); err != nil {
		return nil, fmt.Errorf("remove legacy DFS peer authorizations: %w", err)
	}
	if err := membership.MigrateLegacySharedState(repo.Config.Repository); err != nil {
		return nil, fmt.Errorf("migrate DFS membership metadata: %w", err)
	}
	certificate, fingerprint, err := loadOrCreateCertificate(repo.Config.Repository)
	if err != nil {
		return nil, err
	}
	udpAddress, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve DFS managed transport address: %w", err)
	}
	port := udpAddress.Port
	membershipKey, membershipRecord, err := ensureLocalMembership(ctx, repo, filesystemID, port)
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
		fingerprint: fingerprint, membershipKey: membershipKey, membershipRecord: membershipRecord, members: accepted, endorsements: endorsements,
		statePath:   filepath.Join(repo.Config.Repository, filepath.FromSlash(config.Directory), runtimeStateFile),
		attempts:    make(map[string]attemptWindow),
		cleanupStop: make(chan struct{}), cleanupDone: make(chan struct{}),
		reconcile: ReconcileMembership,
	}
	service.runBackground = service.startBackground
	pairHandler := func(ctx context.Context, operation string, remote net.Addr, payload json.RawMessage) (json.RawMessage, error) {
		path := "/v1/pair/start"
		handler := service.handlePairStart
		if operation == "pair-complete" {
			path = "/v1/pair/complete"
			handler = service.handlePairComplete
		} else if operation == "join-request" {
			path = "/v1/join/request"
			handler = service.handleJoinRequest
		} else if operation == "join-status" {
			path = "/v1/join/status"
			handler = service.handleJoinStatus
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
	managedService, err := managed.Start(repo, address, func(ctx context.Context) ([]byte, error) {
		report, err := Diagnose(ctx, repo, 10*time.Second)
		if err != nil {
			return nil, err
		}
		return json.Marshal(report)
	}, &certificate, pairHandler, service.authorizePairClone, changed)
	if err != nil {
		return nil, fmt.Errorf("start DFS managed QUIC transport: %w", err)
	}
	service.managed = managedService
	if actual, ok := managedService.Addr().(*net.UDPAddr); ok && port == 0 {
		port = actual.Port
	}
	if advertise {
		interfaces := interfaceProvider()
		if len(interfaces) == 0 {
			_ = service.managed.Close()
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
		return nil, err
	}
	go service.cleanupInvitations()
	service.logger.Info("peer discovery ready", "filesystem_id", filesystemID, "endpoint", endpoint)
	return service, nil
}

func (s *Service) Close() error {
	s.backgroundMu.Lock()
	s.closing = true
	s.backgroundMu.Unlock()
	for _, server := range s.mdnsServers {
		server.Shutdown()
	}
	close(s.cleanupStop)
	<-s.cleanupDone
	var err error
	if s.managed != nil {
		if managedErr := s.managed.Close(); err == nil {
			err = managedErr
		}
	}
	s.background.Wait()
	if state, stateErr := readRuntimeState(s.repo.Config.Repository); stateErr == nil && state.PID == os.Getpid() {
		_ = os.Remove(s.statePath)
	}
	return err
}

func (s *Service) startBackground(work func()) {
	s.backgroundMu.Lock()
	if s.closing {
		s.backgroundMu.Unlock()
		return
	}
	s.background.Add(1)
	s.backgroundMu.Unlock()
	go func() {
		defer s.background.Done()
		work()
	}()
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
	if err := validatePairingMembership(input.Membership, s.filesystemID, input.PeerID, input.PeerName); err != nil {
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
		subtle.ConstantTimeCompare([]byte(record.SecretHash), []byte(secretHash(input.Secret))) != 1 ||
		(record.BoundPeerID != "" && record.BoundPeerID != input.PeerID) {
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
	record.Pending = &pendingPair{
		SessionID: sessionID, CompletionHash: secretHash(completionSecret), PeerID: input.PeerID,
		PeerName:   strings.TrimSpace(input.PeerName),
		ExpiresAt:  minTime(record.ExpiresAt, time.Now().UTC().Add(pairingLease)),
		Membership: approvedMembership,
	}
	if err := saveInvitation(s.repo.Config.Repository, record); err != nil {
		writeProtocolError(response, http.StatusInternalServerError, "cannot persist pairing session")
		return
	}
	writeJSON(response, http.StatusOK, s.startResponse(record.Pending, completionSecret))
}

func (s *Service) startResponse(pending *pendingPair, completionSecret string) PairStartResponse {
	return PairStartResponse{
		Version: ProtocolVersion, FileSystemID: s.filesystemID, NetworkName: s.repo.Config.NetworkName,
		PeerName: s.repo.Config.Name, PeerID: s.repo.Config.PeerID,
		SessionID: pending.SessionID, CompletionSecret: completionSecret, ExpiresAt: pending.ExpiresAt,
		Membership: pending.Membership, Approver: s.membershipRecord, Members: s.members, Endorsements: s.endorsements,
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
	s.members = accepted
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
		executable, executableErr := os.Executable()
		if executableErr != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot locate DFS executable")
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
		remoteName, err = s.repo.AddManagedRemote(ctx, pending.PeerID, executable)
		cancel()
		if err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot configure reverse peer")
			return
		}
		refreshCtx, refreshCancel := context.WithTimeout(request.Context(), 30*time.Second)
		if err := s.refreshEndorsements(refreshCtx); err != nil {
			refreshCancel()
			writeProtocolError(response, http.StatusInternalServerError, "cannot refresh approved membership: "+err.Error())
			return
		}
		refreshCancel()
		var notify []string
		for _, member := range s.members {
			if member.Payload.PeerID == s.repo.Config.PeerID || member.Payload.PeerID == pending.PeerID {
				continue
			}
			notify = append(notify, member.Payload.PeerID)
		}
		if err := os.Remove(invitationPath(s.repo.Config.Repository, record.ID)); err != nil {
			writeProtocolError(response, http.StatusInternalServerError, "cannot finalize pairing invitation")
			return
		}
		s.logger.Info("peer paired", "peer", pending.PeerName, "peer_id", pending.PeerID, "remote", remoteName)
		writeJSON(response, http.StatusOK, PairCompleteResponse{RemoteName: remoteName})
		s.reconcileApprovedMembership(notify)
		return
	}
	writeProtocolError(response, http.StatusUnauthorized, "invalid or expired pairing session")
}

func (s *Service) reconcileApprovedMembership(notify []string) {
	s.runBackground(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.reconcile(ctx, s.repo); err != nil {
			s.logger.Warn("approved membership reconciliation deferred", "error", err)
			return
		}
		for _, peerID := range notify {
			notifyCtx, notifyCancel := context.WithTimeout(ctx, 5*time.Second)
			_ = managed.RequestReconcile(notifyCtx, s.repo, peerID)
			notifyCancel()
		}
	})
}

func (s *Service) authorizePairClone(_ context.Context, sessionID, completionSecret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	infos, err := ListInvitations(s.repo.Config.Repository, time.Now())
	if err != nil {
		return errors.New("cannot inspect pairing sessions")
	}
	for _, info := range infos {
		record, loadErr := loadInvitation(s.repo.Config.Repository, info.ID)
		if loadErr != nil || record.Pending == nil || record.Pending.SessionID != sessionID {
			continue
		}
		pending := record.Pending
		if pending.ExpiresAt.After(time.Now()) && subtle.ConstantTimeCompare([]byte(pending.CompletionHash), []byte(secretHash(completionSecret))) == 1 {
			return nil
		}
	}
	return errors.New("invalid or expired pairing session")
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
	return "quic://" + net.JoinHostPort(strings.TrimSuffix(localMDNSHostname(), "."), strconv.Itoa(port))
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
