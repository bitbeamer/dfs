package peer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/membership"
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
	PairingPort    int
	GitName        string
	GitEmail       string
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
	_ = configureReverse // Reciprocal registration is intrinsic to QUIC pairing.
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
	pairingPort := options.PairingPort
	if pairingPort == 0 {
		pairingPort = DefaultPairingPort
	}
	_, membershipDraft, err := newMembershipDraft(temporary, invitation.FileSystemID, peerID, name, pairingPort)
	if err != nil {
		return nil, fmt.Errorf("create signed DFS membership: %w", err)
	}
	request.Membership = membershipDraft

	var quicEndpoints []string
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
			quicEndpoints = append(quicEndpoints, offer.Endpoint)
		}
	}
	if invitation.QUICEndpoint != "" {
		quicEndpoints = append(quicEndpoints, invitation.QUICEndpoint)
	}
	quicEndpoints = uniqueStrings(quicEndpoints)
	if len(quicEndpoints) == 0 {
		if discoverErr != nil {
			return nil, discoverErr
		}
		return nil, errors.New("the invited DFS network was not discovered; ensure an existing peer is mounted and multicast is allowed")
	}

	var (
		start    PairStartResponse
		endpoint string
		startErr error
	)
	for _, candidate := range quicEndpoints {
		startErr = postPair(ctx, candidate, invitation.CertificateSHA256, "pair-start", request, &start)
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
	if start.Approver.Payload.PeerID != start.PeerID || start.Membership.Payload.PeerID != peerID {
		return nil, errors.New("pairing peer returned mismatched membership identities")
	}
	if err := verifyMembershipApprovalChain(start.Approver, start.Members, invitation.FileSystemID); err != nil {
		return nil, fmt.Errorf("verify approving DFS membership: %w", err)
	}
	if err := membership.VerifyApproval(start.Membership, start.Approver); err != nil {
		return nil, fmt.Errorf("verify approved DFS membership: %w", err)
	}
	if !strings.HasPrefix(endpoint, "quic://") {
		return nil, errors.New("pairing peer did not provide a QUIC bootstrap endpoint")
	}
	bundlePath := filepath.Join(temporary, "repository.bundle")
	if err := managed.PairClone(ctx, endpoint, invitation.CertificateSHA256, start.SessionID, start.CompletionSecret, bundlePath); err != nil {
		return nil, fmt.Errorf("clone paired DFS repository over QUIC: %w", err)
	}
	repo, err := repository.JoinWithIdentity(ctx, bundlePath, destination, name, cacheLimit,
		repository.GitIdentity{Name: options.GitName, Email: options.GitEmail}, invitation.FileSystemID)
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
	if err := os.Rename(membership.KeyPath(temporary), membership.KeyPath(repo.Config.Repository)); err != nil {
		return nil, fmt.Errorf("install DFS membership private key: %w", err)
	}
	if err := membership.MigrateLegacySharedState(repo.Config.Repository); err != nil {
		return nil, fmt.Errorf("migrate DFS membership metadata: %w", err)
	}
	if err := membership.Save(repo.Config.Repository, start.Approver); err != nil {
		return nil, fmt.Errorf("save approving DFS membership: %w", err)
	}
	if err := membership.Save(repo.Config.Repository, start.Membership); err != nil {
		return nil, fmt.Errorf("save local DFS membership: %w", err)
	}
	memberKeys := make(map[string]string, len(start.Endorsements))
	for _, endorsement := range start.Endorsements {
		if err := membership.VerifyEndorsement(endorsement, start.Approver); err != nil {
			return nil, fmt.Errorf("verify DFS membership endorsement: %w", err)
		}
		memberKeys[endorsement.PeerID] = endorsement.SigningPublicKey
	}
	for _, member := range start.Members {
		key, endorsed := memberKeys[member.Payload.PeerID]
		if !endorsed || key != member.Payload.SigningPublicKey {
			return nil, fmt.Errorf("DFS member %s is not endorsed by the approving peer", member.Payload.PeerID)
		}
		if err := membership.VerifySelf(member); err != nil {
			return nil, fmt.Errorf("verify endorsed DFS member %s: %w", member.Payload.PeerID, err)
		}
		if member.Payload.FileSystemID != invitation.FileSystemID {
			return nil, fmt.Errorf("endorsed DFS member %s belongs to another filesystem", member.Payload.PeerID)
		}
		if err := membership.Save(repo.Config.Repository, member); err != nil {
			return nil, fmt.Errorf("save endorsed DFS member %s: %w", member.Payload.PeerID, err)
		}
	}
	if err := membership.Trust(repo.Config.Repository, start.PeerID, start.Approver.Payload.SigningPublicKey); err != nil {
		return nil, fmt.Errorf("trust approving DFS member: %w", err)
	}
	if err := membership.Trust(repo.Config.Repository, peerID, start.Membership.Payload.SigningPublicKey); err != nil {
		return nil, fmt.Errorf("trust local DFS membership: %w", err)
	}
	for _, endorsement := range start.Endorsements {
		if err := membership.Trust(repo.Config.Repository, endorsement.PeerID, endorsement.SigningPublicKey); err != nil {
			return nil, fmt.Errorf("trust endorsed DFS member: %w", err)
		}
	}
	if err := repo.RemoveRemote(ctx, "origin"); err != nil {
		return nil, fmt.Errorf("remove temporary bundle remote: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate DFS executable: %w", err)
	}
	if _, err := repo.AddManagedRemote(ctx, start.PeerID, executable); err != nil {
		return nil, fmt.Errorf("configure approving QUIC peer: %w", err)
	}
	resume := pairingResume{
		Version: ProtocolVersion, Endpoint: endpoint, CertificateSHA256: invitation.CertificateSHA256,
		SessionID: start.SessionID, CompletionSecret: start.CompletionSecret,
	}
	if err := savePairingResume(repo.Config.Repository, resume); err != nil {
		return nil, err
	}
	var complete PairCompleteResponse
	if err := postPair(ctx, endpoint, invitation.CertificateSHA256, "pair-complete", PairCompleteRequest{
		SessionID: start.SessionID, CompletionSecret: start.CompletionSecret,
	}, &complete); err != nil {
		return nil, fmt.Errorf("repository joined but reciprocal pairing is incomplete: %w; retry with dfs --repo %s internal pairing complete", err, repo.Config.Repository)
	}
	if err := removePairingResume(repo.Config.Repository); err != nil {
		return nil, err
	}
	if err := ReconcileMembership(ctx, repo); err != nil {
		return nil, fmt.Errorf("reconcile DFS membership: %w", err)
	}
	keepOpen = true
	return &JoinResult{
		Repository: repo, NetworkName: start.NetworkName, OfferingPeer: start.PeerName,
		ReverseRemoteName: complete.RemoteName,
	}, nil
}

func verifyMembershipApprovalChain(record membership.Record, members []membership.Record, filesystemID string) error {
	byID := make(map[string]membership.Record, len(members)+1)
	for _, member := range append(append([]membership.Record(nil), members...), record) {
		if member.Payload.FileSystemID != filesystemID {
			continue
		}
		if existing, found := byID[member.Payload.PeerID]; found && existing.Payload.SigningPublicKey != member.Payload.SigningPublicKey {
			return fmt.Errorf("membership chain contains conflicting records for peer %s", member.Payload.PeerID)
		}
		byID[member.Payload.PeerID] = member
	}
	current := record
	visited := make(map[string]bool, len(byID))
	for {
		peerID := current.Payload.PeerID
		if current.Payload.FileSystemID != filesystemID {
			return errors.New("approving membership belongs to another filesystem")
		}
		if visited[peerID] {
			return errors.New("membership approval chain contains a cycle")
		}
		visited[peerID] = true
		approver, found := byID[current.ApprovedBy]
		if !found {
			return fmt.Errorf("membership approval chain is missing peer %s", current.ApprovedBy)
		}
		if err := membership.VerifyApproval(current, approver); err != nil {
			return err
		}
		if current.ApprovedBy == peerID {
			return nil
		}
		current = approver
	}
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
	var complete PairCompleteResponse
	if err := postPair(ctx, resume.Endpoint, resume.CertificateSHA256, "pair-complete", PairCompleteRequest{
		SessionID: resume.SessionID, CompletionSecret: resume.CompletionSecret,
	}, &complete); err != nil {
		return PairCompleteResponse{}, err
	}
	if err := removePairingResume(repositoryPath); err != nil {
		return PairCompleteResponse{}, err
	}
	return complete, nil
}

func postPair(ctx context.Context, endpoint, certificateSHA256, operation string, input, output any) error {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if !strings.HasPrefix(endpoint, "quic://") {
		return errors.New("DFS pairing requires a QUIC endpoint")
	}
	return managed.PairCall(ctx, endpoint, certificateSHA256, operation, input, output)
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
