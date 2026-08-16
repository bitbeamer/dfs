package peer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/repository"
)

const (
	invitationDirectory = "invitations"
	pairingLease        = 5 * time.Minute
)

type invitationRecord struct {
	Version      int          `json:"version"`
	ID           string       `json:"id"`
	SecretHash   string       `json:"secret_hash"`
	FileSystemID string       `json:"filesystem_id"`
	ExpiresAt    time.Time    `json:"expires_at"`
	BoundPeerID  string       `json:"bound_peer_id,omitempty"`
	Pending      *pendingPair `json:"pending,omitempty"`
}

type pendingPair struct {
	SessionID        string            `json:"session_id"`
	CompletionHash   string            `json:"completion_hash"`
	PeerID           string            `json:"peer_id"`
	PeerName         string            `json:"peer_name"`
	AuthorizedMarker string            `json:"authorized_marker,omitempty"`
	ExpiresAt        time.Time         `json:"expires_at"`
	Membership       membership.Record `json:"membership"`
}

type InvitationInfo struct {
	ID        string
	ExpiresAt time.Time
	Pending   bool
}

func CreateInvitation(repo *repository.Repository, lifetime time.Duration) (Invitation, error) {
	return createInvitation(repo, lifetime, "")
}

func createInvitation(repo *repository.Repository, lifetime time.Duration, boundPeerID string) (Invitation, error) {
	if lifetime <= 0 || lifetime > 24*time.Hour {
		return Invitation{}, errors.New("invitation lifetime must be greater than zero and at most 24 hours")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return Invitation{}, err
	}
	_, fingerprint, err := loadOrCreateCertificate(repo.Config.Repository)
	if err != nil {
		return Invitation{}, err
	}
	id, err := randomString(12)
	if err != nil {
		return Invitation{}, err
	}
	secret, err := randomString(32)
	if err != nil {
		return Invitation{}, err
	}
	record := invitationRecord{
		Version: ProtocolVersion, ID: id, SecretHash: secretHash(secret),
		FileSystemID: filesystemID, ExpiresAt: time.Now().UTC().Add(lifetime),
		BoundPeerID: strings.TrimSpace(boundPeerID),
	}
	if err := saveInvitation(repo.Config.Repository, record); err != nil {
		return Invitation{}, err
	}
	invitation := Invitation{
		Version: ProtocolVersion, FileSystemID: filesystemID, InvitationID: id,
		Secret: secret, CertificateSHA256: fingerprint,
	}
	if state, err := readRuntimeState(repo.Config.Repository); err == nil && state.FileSystemID == filesystemID {
		invitation.QUICEndpoint = state.Endpoint
	}
	return invitation, nil
}

func ListInvitations(repositoryPath string, now time.Time) ([]InvitationInfo, error) {
	directory := invitationsPath(repositoryPath)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list pairing invitations: %w", err)
	}
	var result []InvitationInfo
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, err := loadInvitationPath(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		}
		if !record.ExpiresAt.After(now) {
			if record.Pending != nil {
				_ = removeAuthorizedMarker(record.Pending.AuthorizedMarker)
			}
			_ = os.Remove(filepath.Join(directory, entry.Name()))
			continue
		}
		if record.Pending != nil && !record.Pending.ExpiresAt.After(now) {
			_ = removeAuthorizedMarker(record.Pending.AuthorizedMarker)
			record.Pending = nil
			if err := saveInvitation(repositoryPath, record); err != nil {
				return nil, err
			}
		}
		result = append(result, InvitationInfo{ID: record.ID, ExpiresAt: record.ExpiresAt, Pending: record.Pending != nil})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ExpiresAt.Before(result[j].ExpiresAt) })
	return result, nil
}

func RevokeInvitation(repositoryPath, id string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, `/\\`) {
		return errors.New("invalid invitation ID")
	}
	record, loadErr := loadInvitation(repositoryPath, id)
	if loadErr == nil && record.Pending != nil {
		if err := removeAuthorizedMarker(record.Pending.AuthorizedMarker); err != nil {
			return fmt.Errorf("remove pending peer authorization: %w", err)
		}
	}
	err := os.Remove(invitationPath(repositoryPath, id))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pairing invitation %s does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("revoke pairing invitation: %w", err)
	}
	return nil
}

func invitationsPath(repositoryPath string) string {
	return filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), invitationDirectory)
}

func invitationPath(repositoryPath, id string) string {
	return filepath.Join(invitationsPath(repositoryPath), id+".json")
}

func saveInvitation(repositoryPath string, record invitationRecord) error {
	directory := invitationsPath(repositoryPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create pairing invitation directory: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := invitationPath(repositoryPath, record.ID) + ".new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write pairing invitation: %w", err)
	}
	if err := os.Rename(temporary, invitationPath(repositoryPath, record.ID)); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish pairing invitation: %w", err)
	}
	return nil
}

func loadInvitation(repositoryPath, id string) (invitationRecord, error) {
	if id == "" || strings.ContainsAny(id, `/\\`) {
		return invitationRecord{}, errors.New("invalid invitation ID")
	}
	return loadInvitationPath(invitationPath(repositoryPath, id))
}

func loadInvitationPath(path string) (invitationRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return invitationRecord{}, err
	}
	var record invitationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return invitationRecord{}, fmt.Errorf("decode pairing invitation: %w", err)
	}
	return record, nil
}

func randomString(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate pairing secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
