package peer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/membership"
)

const (
	ServiceType        = "_dfs._tcp"
	ProtocolVersion    = 2
	DefaultPairingPort = 7843
	invitationPrefix   = "dfs2_"
)

type Invitation struct {
	Version           int    `json:"v"`
	FileSystemID      string `json:"fs"`
	InvitationID      string `json:"id"`
	Secret            string `json:"secret"`
	CertificateSHA256 string `json:"cert"`
	Endpoint          string `json:"endpoint,omitempty"`
	QUICEndpoint      string `json:"quic_endpoint,omitempty"`
}

type PairStartRequest struct {
	InvitationID string            `json:"invitation_id"`
	Secret       string            `json:"secret"`
	PeerID       string            `json:"peer_id"`
	PeerName     string            `json:"peer_name"`
	SSHPublicKey string            `json:"ssh_public_key"`
	SSHHostKeys  []string          `json:"ssh_host_keys,omitempty"`
	ReverseUser  string            `json:"reverse_user,omitempty"`
	ReversePath  string            `json:"reverse_path,omitempty"`
	Membership   membership.Record `json:"membership"`
}

type PairStartResponse struct {
	Version          int                      `json:"version"`
	FileSystemID     string                   `json:"filesystem_id"`
	NetworkName      string                   `json:"network_name"`
	PeerName         string                   `json:"peer_name"`
	PeerID           string                   `json:"peer_id"`
	CloneURL         string                   `json:"clone_url"`
	SSHPublicKey     string                   `json:"ssh_public_key"`
	SSHHostKeys      []string                 `json:"ssh_host_keys,omitempty"`
	SessionID        string                   `json:"session_id"`
	CompletionSecret string                   `json:"completion_secret"`
	ExpiresAt        time.Time                `json:"expires_at"`
	Membership       membership.Record        `json:"membership"`
	Approver         membership.Record        `json:"approver"`
	Members          []membership.Record      `json:"members,omitempty"`
	Endorsements     []membership.Endorsement `json:"endorsements,omitempty"`
}

type PairCompleteRequest struct {
	SessionID        string `json:"session_id"`
	CompletionSecret string `json:"completion_secret"`
}

type PairCompleteResponse struct {
	RemoteName string `json:"remote_name,omitempty"`
}

type JoinRequest struct {
	Version      int               `json:"version"`
	RequestID    string            `json:"request_id"`
	Secret       string            `json:"secret"`
	FileSystemID string            `json:"filesystem_id"`
	PeerID       string            `json:"peer_id"`
	PeerName     string            `json:"peer_name"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Membership   membership.Record `json:"membership"`
}

type JoinRequestResponse struct {
	RequestID string    `json:"request_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type JoinStatusRequest struct {
	RequestID string `json:"request_id"`
	Secret    string `json:"secret"`
}

type JoinStatusResponse struct {
	Status     string      `json:"status"`
	Invitation *Invitation `json:"invitation,omitempty"`
}

type protocolError struct {
	Error string `json:"error"`
}

func (i Invitation) Encode() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(i)
	if err != nil {
		return "", err
	}
	return invitationPrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func DecodeInvitation(value string) (Invitation, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, invitationPrefix) {
		return Invitation{}, errors.New("invalid DFS invitation prefix")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, invitationPrefix))
	if err != nil {
		return Invitation{}, fmt.Errorf("decode DFS invitation: %w", err)
	}
	var invitation Invitation
	if err := json.Unmarshal(data, &invitation); err != nil {
		return Invitation{}, fmt.Errorf("decode DFS invitation: %w", err)
	}
	if err := invitation.Validate(); err != nil {
		return Invitation{}, err
	}
	return invitation, nil
}

func (i Invitation) Validate() error {
	if i.Version != ProtocolVersion {
		return fmt.Errorf("unsupported DFS pairing protocol version %d", i.Version)
	}
	if len(i.FileSystemID) < 16 || i.InvitationID == "" || i.Secret == "" {
		return errors.New("incomplete DFS invitation")
	}
	if len(i.CertificateSHA256) != sha256.Size*2 {
		return errors.New("invalid DFS invitation certificate fingerprint")
	}
	if _, err := hex.DecodeString(i.CertificateSHA256); err != nil {
		return errors.New("invalid DFS invitation certificate fingerprint")
	}
	if i.Endpoint != "" && !strings.HasPrefix(i.Endpoint, "https://") {
		return errors.New("DFS invitation endpoint must use HTTPS")
	}
	if i.QUICEndpoint != "" && !strings.HasPrefix(i.QUICEndpoint, "quic://") {
		return errors.New("DFS invitation QUIC endpoint must use quic://")
	}
	return nil
}

func secretHash(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}
