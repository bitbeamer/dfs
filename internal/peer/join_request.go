package peer

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/repository"
)

const joinRequestDirectory = "join-requests"

type joinRequestRecord struct {
	Version      int               `json:"version"`
	RequestID    string            `json:"request_id"`
	SecretHash   string            `json:"secret_hash"`
	FileSystemID string            `json:"filesystem_id"`
	PeerID       string            `json:"peer_id"`
	PeerName     string            `json:"peer_name"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Membership   membership.Record `json:"membership"`
	Invitation   *Invitation       `json:"invitation,omitempty"`
}

type JoinRequestInfo struct {
	ID        string
	PeerID    string
	PeerName  string
	ExpiresAt time.Time
	Approved  bool
}

type JoinRequestCredentials struct {
	RequestID      string    `json:"request_id"`
	Secret         string    `json:"secret"`
	ExpiresAt      time.Time `json:"expires_at"`
	ApprovingPeers []string  `json:"approving_peers,omitempty"`
}

func SubmitJoinRequest(ctx context.Context, network Network, peerID, peerName, stateDirectory string, pairingPort int, lifetime time.Duration) (JoinRequestCredentials, error) {
	if lifetime <= 0 || lifetime > 30*time.Minute {
		return JoinRequestCredentials{}, errors.New("join request lifetime must be greater than zero and at most 30 minutes")
	}
	if !validPeerID(peerID) || strings.TrimSpace(peerName) == "" || len(network.FileSystemID) < 16 {
		return JoinRequestCredentials{}, errors.New("incomplete DFS join identity")
	}
	_, draft, err := newMembershipDraft(stateDirectory, network.FileSystemID, peerID, peerName, pairingPort)
	if err != nil {
		return JoinRequestCredentials{}, err
	}
	id, err := randomString(12)
	if err != nil {
		return JoinRequestCredentials{}, err
	}
	secret, err := randomString(32)
	if err != nil {
		return JoinRequestCredentials{}, err
	}
	expires := time.Now().UTC().Add(lifetime)
	request := JoinRequest{Version: ProtocolVersion, RequestID: id, Secret: secret, FileSystemID: network.FileSystemID,
		PeerID: peerID, PeerName: strings.TrimSpace(peerName), ExpiresAt: expires, Membership: draft}
	var failures []string
	accepted := 0
	var approvingPeers []string
	for _, offer := range network.Offers {
		if offer.ProtocolVersion != ProtocolVersion || offer.CertificateSHA256 == "" {
			continue
		}
		endpoint := offer.Endpoint
		var response JoinRequestResponse
		if err := managed.PairCall(ctx, endpoint, offer.CertificateSHA256, "join-request", request, &response); err != nil {
			failures = append(failures, offer.PeerName+": "+err.Error())
			continue
		}
		if response.RequestID != id || !response.ExpiresAt.Equal(expires) {
			failures = append(failures, offer.PeerName+": invalid acknowledgement")
			continue
		}
		accepted++
		approvingPeers = append(approvingPeers, offer.PeerName)
	}
	if accepted == 0 {
		if len(failures) == 0 {
			return JoinRequestCredentials{}, errors.New("no compatible DFS peer accepted the join request")
		}
		return JoinRequestCredentials{}, fmt.Errorf("no DFS peer accepted the join request: %s", strings.Join(failures, "; "))
	}
	sort.Strings(approvingPeers)
	approvingPeers = slices.Compact(approvingPeers)
	return JoinRequestCredentials{RequestID: id, Secret: secret, ExpiresAt: expires, ApprovingPeers: approvingPeers}, nil
}

func PollJoinApproval(ctx context.Context, network Network, credentials JoinRequestCredentials) (Invitation, bool, error) {
	if credentials.RequestID == "" || credentials.Secret == "" || !credentials.ExpiresAt.After(time.Now()) {
		return Invitation{}, false, errors.New("DFS join request has expired")
	}
	request := JoinStatusRequest{RequestID: credentials.RequestID, Secret: credentials.Secret}
	var failures []string
	for _, offer := range network.Offers {
		if offer.ProtocolVersion != ProtocolVersion || offer.CertificateSHA256 == "" {
			continue
		}
		endpoint := offer.Endpoint
		var response JoinStatusResponse
		if err := managed.PairCall(ctx, endpoint, offer.CertificateSHA256, "join-status", request, &response); err != nil {
			failures = append(failures, offer.PeerName+": "+err.Error())
			continue
		}
		if response.Status == "approved" && response.Invitation != nil {
			if err := response.Invitation.Validate(); err != nil {
				return Invitation{}, false, fmt.Errorf("approved DFS invitation is invalid: %w", err)
			}
			return *response.Invitation, true, nil
		}
	}
	if len(failures) == len(network.Offers) && len(failures) > 0 {
		return Invitation{}, false, fmt.Errorf("cannot check DFS join approval: %s", strings.Join(failures, "; "))
	}
	return Invitation{}, false, nil
}

func ListJoinRequests(repositoryPath string, now time.Time) ([]JoinRequestInfo, error) {
	directory := joinRequestsPath(repositoryPath)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []JoinRequestInfo
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, err := loadJoinRequestPath(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		}
		if !record.ExpiresAt.After(now) {
			_ = os.Remove(filepath.Join(directory, entry.Name()))
			continue
		}
		result = append(result, JoinRequestInfo{ID: record.RequestID, PeerID: record.PeerID, PeerName: record.PeerName,
			ExpiresAt: record.ExpiresAt, Approved: record.Invitation != nil})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ExpiresAt.Before(result[j].ExpiresAt) })
	return result, nil
}

func ApproveJoinRequest(repo *repository.Repository, id string, lifetime time.Duration) (Invitation, error) {
	record, err := loadJoinRequest(repo.Config.Repository, id)
	if errors.Is(err, os.ErrNotExist) {
		return Invitation{}, fmt.Errorf("DFS join request %s is not pending on this peer; run the approval command on a peer named by the joining setup", id)
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("load DFS join request: %w", err)
	}
	if !record.ExpiresAt.After(time.Now()) {
		return Invitation{}, errors.New("DFS join request has expired")
	}
	if record.Invitation != nil {
		return *record.Invitation, nil
	}
	if lifetime <= 0 || time.Now().Add(lifetime).After(record.ExpiresAt) {
		lifetime = time.Until(record.ExpiresAt)
	}
	invitation, err := createInvitation(repo, lifetime, record.PeerID)
	if err != nil {
		return Invitation{}, err
	}
	record.Invitation = &invitation
	if err := saveJoinRequest(repo.Config.Repository, record); err != nil {
		_ = RevokeInvitation(repo.Config.Repository, invitation.InvitationID)
		return Invitation{}, err
	}
	return invitation, nil
}

func RejectJoinRequest(repositoryPath, id string) error {
	record, err := loadJoinRequest(repositoryPath, id)
	if err != nil {
		return err
	}
	if record.Invitation != nil {
		_ = RevokeInvitation(repositoryPath, record.Invitation.InvitationID)
	}
	return os.Remove(joinRequestPath(repositoryPath, id))
}

func (s *Service) handleJoinRequest(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeProtocolError(response, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var input JoinRequest
	if err := decodeRequest(response, request, &input); err != nil {
		return
	}
	if input.Version != ProtocolVersion || input.FileSystemID != s.filesystemID || !validRequestID(input.RequestID) ||
		input.Secret == "" || !input.ExpiresAt.After(time.Now()) || input.ExpiresAt.After(time.Now().Add(30*time.Minute)) ||
		validatePairingMembership(input.Membership, s.filesystemID, input.PeerID, input.PeerName) != nil {
		writeProtocolError(response, http.StatusBadRequest, "invalid DFS join request")
		return
	}
	record := joinRequestRecord{Version: ProtocolVersion, RequestID: input.RequestID, SecretHash: secretHash(input.Secret),
		FileSystemID: input.FileSystemID, PeerID: input.PeerID, PeerName: strings.TrimSpace(input.PeerName),
		ExpiresAt: input.ExpiresAt.UTC(), Membership: input.Membership}
	if existing, err := loadJoinRequest(s.repo.Config.Repository, input.RequestID); err == nil {
		if existing.SecretHash != record.SecretHash || existing.PeerID != record.PeerID || existing.FileSystemID != record.FileSystemID {
			writeProtocolError(response, http.StatusConflict, "DFS join request ID is already in use")
			return
		}
		record = existing
	}
	if err := saveJoinRequest(s.repo.Config.Repository, record); err != nil {
		writeProtocolError(response, http.StatusInternalServerError, "cannot persist DFS join request")
		return
	}
	writeJSON(response, http.StatusOK, JoinRequestResponse{RequestID: record.RequestID, ExpiresAt: record.ExpiresAt})
}

func (s *Service) handleJoinStatus(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeProtocolError(response, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var input JoinStatusRequest
	if err := decodeRequest(response, request, &input); err != nil {
		return
	}
	record, err := loadJoinRequest(s.repo.Config.Repository, input.RequestID)
	if err != nil || !record.ExpiresAt.After(time.Now()) ||
		subtle.ConstantTimeCompare([]byte(record.SecretHash), []byte(secretHash(input.Secret))) != 1 {
		writeProtocolError(response, http.StatusUnauthorized, "invalid or expired DFS join request")
		return
	}
	status := JoinStatusResponse{Status: "pending"}
	if record.Invitation != nil {
		status.Status, status.Invitation = "approved", record.Invitation
	}
	writeJSON(response, http.StatusOK, status)
}

func joinRequestsPath(repositoryPath string) string {
	return filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), joinRequestDirectory)
}

func joinRequestPath(repositoryPath, id string) string {
	return filepath.Join(joinRequestsPath(repositoryPath), id+".json")
}

func validRequestID(id string) bool { return id != "" && !strings.ContainsAny(id, `/\\`) }

func saveJoinRequest(repositoryPath string, record joinRequestRecord) error {
	if !validRequestID(record.RequestID) {
		return errors.New("invalid DFS join request ID")
	}
	if err := os.MkdirAll(joinRequestsPath(repositoryPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := joinRequestPath(repositoryPath, record.RequestID) + ".new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, joinRequestPath(repositoryPath, record.RequestID))
}

func loadJoinRequest(repositoryPath, id string) (joinRequestRecord, error) {
	if !validRequestID(id) {
		return joinRequestRecord{}, errors.New("invalid DFS join request ID")
	}
	return loadJoinRequestPath(joinRequestPath(repositoryPath, id))
}

func loadJoinRequestPath(path string) (joinRequestRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return joinRequestRecord{}, err
	}
	var record joinRequestRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return joinRequestRecord{}, err
	}
	return record, nil
}
