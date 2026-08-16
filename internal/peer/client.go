package peer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
)

type JoinResult struct {
	Repository        *repository.Repository
	NetworkName       string
	OfferingPeer      string
	ReverseRemoteName string
}

// PairOptions supplies durable client identity for a resumable pairing. When
// StateDirectory is empty PairAndJoin retains its historical, ephemeral
// behavior.
type PairOptions struct {
	PeerID         string
	StateDirectory string
}

const pairingResumeFile = "pairing-resume.json"

type pairingResume struct {
	Version           int    `json:"version"`
	Endpoint          string `json:"endpoint"`
	CertificateSHA256 string `json:"certificate_sha256"`
	SessionID         string `json:"session_id"`
	CompletionSecret  string `json:"completion_secret"`
}

func PairAndJoin(ctx context.Context, encodedInvitation, destination, name string, cacheLimit int64, discoveryTimeout time.Duration, configureReverse bool) (*JoinResult, error) {
	return PairAndJoinWithOptions(ctx, encodedInvitation, destination, name, cacheLimit, discoveryTimeout, configureReverse, PairOptions{})
}

func PairAndJoinWithOptions(ctx context.Context, encodedInvitation, destination, name string, cacheLimit int64, discoveryTimeout time.Duration, configureReverse bool, options PairOptions) (*JoinResult, error) {
	invitation, err := DecodeInvitation(encodedInvitation)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	peerID := strings.TrimSpace(options.PeerID)
	if peerID == "" {
		peerID, err = newPeerID()
		if err != nil {
			return nil, err
		}
	} else if !validPeerID(peerID) {
		return nil, errors.New("invalid persisted pairing peer ID")
	}
	request := PairStartRequest{
		InvitationID: invitation.InvitationID, Secret: invitation.Secret,
		PeerID: peerID, PeerName: name,
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create paired repository parent: %w", err)
	}
	temporary := strings.TrimSpace(options.StateDirectory)
	removeTemporary := temporary == ""
	if removeTemporary {
		temporary, err = os.MkdirTemp(parent, ".dfs-pair-")
	} else {
		temporary, err = filepath.Abs(temporary)
		if err == nil {
			err = os.MkdirAll(temporary, 0o700)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create pairing state: %w", err)
	}
	if removeTemporary {
		defer os.RemoveAll(temporary)
	}
	identity, err := ensureTransportIdentity(ctx, temporary, peerID)
	if err != nil {
		return nil, err
	}
	request.SSHPublicKey = identity.PublicKey
	request.SSHHostKeys = localSSHHostKeys()
	if configureReverse {
		account, userErr := user.Current()
		if userErr != nil {
			return nil, fmt.Errorf("determine local account for reverse peer: %w", userErr)
		}
		request.ReverseUser = account.Username
		request.ReversePath = destination
	}

	endpoints := []string{}
	discoveryCtx, cancel := context.WithTimeout(ctx, discoveryTimeout+time.Second)
	offers, discoverErr := Discover(discoveryCtx, discoveryTimeout)
	cancel()
	if discoverErr == nil {
		for _, offer := range offers {
			if offer.FileSystemID != invitation.FileSystemID || offer.ProtocolVersion != ProtocolVersion {
				continue
			}
			if offer.CertificateSHA256 != "" && !strings.EqualFold(offer.CertificateSHA256, invitation.CertificateSHA256) {
				continue
			}
			endpoints = append(endpoints, offer.Endpoint)
		}
	}
	if invitation.Endpoint != "" {
		endpoints = append(endpoints, invitation.Endpoint)
	}
	endpoints = uniqueStrings(endpoints)
	if len(endpoints) == 0 {
		if discoverErr != nil {
			return nil, discoverErr
		}
		return nil, errors.New("the invited DFS network was not discovered; ensure an existing peer is mounted and multicast is allowed")
	}

	client := pinnedHTTPClient(invitation.CertificateSHA256)
	defer client.CloseIdleConnections()
	var (
		start    PairStartResponse
		endpoint string
		startErr error
	)
	for _, candidate := range endpoints {
		startErr = postJSON(ctx, client, candidate+"/v1/pair/start", request, &start)
		if startErr == nil {
			endpoint = candidate
			break
		}
	}
	if startErr != nil {
		return nil, fmt.Errorf("start DFS pairing: %w", startErr)
	}
	if start.Version != ProtocolVersion || start.FileSystemID != invitation.FileSystemID {
		return nil, errors.New("pairing peer returned a different DFS filesystem identity")
	}
	if !start.ExpiresAt.After(time.Now()) {
		return nil, errors.New("pairing session expired before repository initialization")
	}
	knownHosts := filepath.Join(temporary, "known_hosts")
	if err := installKnownHosts(knownHosts, start.CloneURL, start.SSHHostKeys); err != nil {
		return nil, fmt.Errorf("configure paired SSH host verification: %w", err)
	}
	if _, err := os.Stat(knownHosts); errors.Is(err, os.ErrNotExist) {
		knownHosts = ""
	}
	localAuthorization, err := authorizePeer(start.SSHPublicKey, destination, invitation.FileSystemID, start.PeerID)
	if err != nil {
		return nil, fmt.Errorf("authorize offering DFS peer: %w", err)
	}
	pairCompleted := false
	defer func() {
		if !pairCompleted {
			_ = removeAuthorizedMarker(localAuthorization)
		}
	}()

	repo, err := repository.JoinWithSSH(ctx, start.CloneURL, destination, name, cacheLimit, transportSSHCommand(identity.PrivateKey, knownHosts))
	if err != nil {
		return nil, fmt.Errorf("clone paired DFS repository: %w", err)
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			_ = repo.Close()
		}
	}()
	joinedID, err := repo.FileSystemID(ctx)
	if err != nil {
		return nil, err
	}
	if joinedID != invitation.FileSystemID {
		return nil, errors.New("cloned repository does not match the invited DFS filesystem")
	}
	repo.Config.PeerID = peerID
	repo.Config.NetworkName = start.NetworkName
	if err := repo.SaveConfig(); err != nil {
		return nil, fmt.Errorf("save paired DFS identity: %w", err)
	}
	stateDirectory := filepath.Join(repo.Config.Repository, filepath.FromSlash(config.Directory))
	installedPrivate := filepath.Join(stateDirectory, transportKeyFile)
	installedPublic := installedPrivate + ".pub"
	if err := os.Rename(identity.PrivateKey, installedPrivate); err != nil {
		return nil, fmt.Errorf("install paired SSH private key: %w", err)
	}
	if err := os.Rename(identity.PrivateKey+".pub", installedPublic); err != nil {
		return nil, fmt.Errorf("install paired SSH public key: %w", err)
	}
	installedKnownHosts := ""
	if knownHosts != "" {
		installedKnownHosts = filepath.Join(stateDirectory, "known_hosts")
		if err := os.Rename(knownHosts, installedKnownHosts); err != nil {
			return nil, fmt.Errorf("install paired SSH host keys: %w", err)
		}
	}
	if err := repo.ConfigureSSHCommand(ctx, transportSSHCommand(installedPrivate, installedKnownHosts)); err != nil {
		return nil, fmt.Errorf("activate paired SSH transport: %w", err)
	}
	if _, err := repo.AdoptClonedPeer(ctx, start.PeerID); err != nil {
		return nil, fmt.Errorf("name paired source remote: %w", err)
	}
	resume := pairingResume{
		Version: ProtocolVersion, Endpoint: endpoint, CertificateSHA256: invitation.CertificateSHA256,
		SessionID: start.SessionID, CompletionSecret: start.CompletionSecret,
	}
	if err := savePairingResume(repo.Config.Repository, resume); err != nil {
		return nil, err
	}
	pairCompleted = true
	var complete PairCompleteResponse
	if err := postJSON(ctx, client, endpoint+"/v1/pair/complete", PairCompleteRequest{
		SessionID: start.SessionID, CompletionSecret: start.CompletionSecret,
	}, &complete); err != nil {
		return nil, fmt.Errorf("repository joined but reciprocal pairing is incomplete: %w; retry with dfs --repo %s network complete", err, repo.Config.Repository)
	}
	if err := removePairingResume(repo.Config.Repository); err != nil {
		return nil, err
	}
	keepOpen = true
	return &JoinResult{
		Repository: repo, NetworkName: start.NetworkName, OfferingPeer: start.PeerName,
		ReverseRemoteName: complete.RemoteName,
	}, nil
}

func CompletePairing(ctx context.Context, repositoryPath string) (PairCompleteResponse, error) {
	resume, err := loadPairingResume(repositoryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PairCompleteResponse{}, errors.New("no incomplete DFS pairing is recorded")
		}
		return PairCompleteResponse{}, err
	}
	if resume.Version != ProtocolVersion || resume.Endpoint == "" || resume.CertificateSHA256 == "" || resume.SessionID == "" || resume.CompletionSecret == "" {
		return PairCompleteResponse{}, errors.New("incomplete DFS pairing record is invalid")
	}
	client := pinnedHTTPClient(resume.CertificateSHA256)
	defer client.CloseIdleConnections()
	var complete PairCompleteResponse
	if err := postJSON(ctx, client, strings.TrimRight(resume.Endpoint, "/")+"/v1/pair/complete", PairCompleteRequest{
		SessionID: resume.SessionID, CompletionSecret: resume.CompletionSecret,
	}, &complete); err != nil {
		return PairCompleteResponse{}, err
	}
	if err := removePairingResume(repositoryPath); err != nil {
		return PairCompleteResponse{}, err
	}
	return complete, nil
}

func savePairingResume(repositoryPath string, resume pairingResume) error {
	data, err := json.MarshalIndent(resume, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), pairingResumeFile)
	temporary := path + ".new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("save incomplete DFS pairing: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish incomplete DFS pairing: %w", err)
	}
	return nil
}

func loadPairingResume(repositoryPath string) (pairingResume, error) {
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), pairingResumeFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return pairingResume{}, err
	}
	var resume pairingResume
	if err := json.Unmarshal(data, &resume); err != nil {
		return pairingResume{}, fmt.Errorf("decode incomplete DFS pairing: %w", err)
	}
	return resume, nil
}

func removePairingResume(repositoryPath string) error {
	path := filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), pairingResumeFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("clear completed DFS pairing state: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("clear completed DFS pairing state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("clear completed DFS pairing state: %w", err)
	}
	// An unlink failure leaves only an empty, non-sensitive marker. Pairing is
	// already complete and should not be reported as failed for cleanup alone.
	_ = os.Remove(path)
	return nil
}

func pinnedHTTPClient(fingerprint string) *http.Client {
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // The out-of-band invitation pins the exact certificate below.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("pairing peer supplied no TLS certificate")
			}
			digest := sha256.Sum256(state.PeerCertificates[0].Raw)
			if !strings.EqualFold(hex.EncodeToString(digest[:]), fingerprint) {
				return errors.New("pairing peer TLS certificate does not match the invitation")
			}
			return nil
		},
	}}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure protocolError
		if decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&failure); decodeErr == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		return fmt.Errorf("pairing peer returned HTTP %s", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(output); err != nil {
		return fmt.Errorf("decode pairing response: %w", err)
	}
	return nil
}

func newPeerID() (string, error) {
	secret, err := randomString(32)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:16]), nil
}

// NewPeerID creates a stable identity suitable for persisting before a
// transactional pairing begins.
func NewPeerID() (string, error) { return newPeerID() }

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimRight(strings.TrimSpace(value), "/")
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
