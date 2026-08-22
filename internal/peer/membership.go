package peer

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/repository"
)

func ensureLocalMembership(ctx context.Context, repo *repository.Repository, filesystemID string, port int) (ed25519.PrivateKey, membership.Record, error) {
	private, public, err := membership.EnsureKey(repo.Config.Repository)
	if err != nil {
		return nil, membership.Record{}, err
	}
	hostname := repo.Config.Hostname
	endpointHost := hostname
	if !strings.Contains(endpointHost, ".") {
		endpointHost += ".local"
	}
	quicEndpoint := fmt.Sprintf("quic://%s:%d", endpointHost, port)
	for _, existing := range mustLoadMembership(repo.Config.Repository) {
		payload := existing.Payload
		if payload.PeerID != repo.Config.PeerID || payload.SigningPublicKey != public {
			continue
		}
		if payload.FileSystemID == filesystemID && payload.Name == repo.Config.Name && payload.Hostname == hostname &&
			payload.QUICEndpoint == quicEndpoint && membership.VerifySelf(existing) == nil {
			if err := membership.Trust(repo.Config.Repository, payload.PeerID, payload.SigningPublicKey); err != nil {
				return nil, membership.Record{}, err
			}
			return private, existing, nil
		}
		return nil, membership.Record{}, errors.New("published DFS membership no longer matches this peer; revoke the old member and pair this machine again")
	}
	record, err := membership.Sign(membership.Payload{Version: membership.Version, FileSystemID: filesystemID,
		PeerID: repo.Config.PeerID, Name: repo.Config.Name, Hostname: hostname, Role: "admin", SigningPublicKey: public,
		QUICEndpoint: quicEndpoint,
		Generation:   1, UpdatedAt: time.Now().UTC()}, private)
	if err != nil {
		return nil, membership.Record{}, err
	}
	record, err = membership.Approve(record, repo.Config.PeerID, private)
	if err != nil {
		return nil, membership.Record{}, err
	}
	if err := membership.Save(repo.Config.Repository, record); err != nil {
		return nil, membership.Record{}, err
	}
	if err := membership.TrustBootstrapAdmin(repo.Config.Repository, record); err != nil {
		return nil, membership.Record{}, err
	}
	return private, record, nil
}

func newMembershipDraft(keyRepositoryPath, filesystemID, peerID, name string, quicPort int) (ed25519.PrivateKey, membership.Record, error) {
	private, public, err := membership.EnsureKey(keyRepositoryPath)
	if err != nil {
		return nil, membership.Record{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, membership.Record{}, err
	}
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	endpointHost := strings.TrimSuffix(hostname, ".local")
	if !strings.Contains(endpointHost, ".") {
		endpointHost += ".local"
	}
	record, err := membership.Sign(membership.Payload{Version: membership.Version, FileSystemID: filesystemID,
		PeerID: peerID, Name: name, Hostname: strings.TrimSuffix(hostname, ".local"), Role: "member", SigningPublicKey: public,
		QUICEndpoint: fmt.Sprintf("quic://%s:%d", endpointHost, quicPort),
		Generation:   1, UpdatedAt: time.Now().UTC()}, private)
	return private, record, err
}

func validatePairingMembership(record membership.Record, filesystemID, peerID, name string) error {
	if err := membership.VerifySelf(record); err != nil {
		return err
	}
	payload := record.Payload
	if payload.FileSystemID != filesystemID || payload.PeerID != peerID || payload.Name != strings.TrimSpace(name) {
		return errors.New("pairing membership does not match the authenticated peer")
	}
	if payload.Role != "member" {
		return errors.New("new peers must request the member role")
	}
	if !strings.HasPrefix(payload.QUICEndpoint, "quic://") {
		return errors.New("pairing membership has an invalid QUIC endpoint")
	}
	return nil
}

func ReconcileMembership(ctx context.Context, repo *repository.Repository) error {
	if err := ConfigureMembership(ctx, repo); err != nil {
		return err
	}
	remotes, err := repo.Remotes(ctx)
	if err != nil {
		return err
	}
	remoteNames := make([]string, 0, len(remotes))
	for _, remote := range remotes {
		remoteNames = append(remoteNames, remote.Name)
	}
	if err := membership.Sync(ctx, repo.Config.Repository, remoteNames); err != nil {
		return fmt.Errorf("synchronize DFS membership metadata: %w", err)
	}
	return ConfigureMembership(ctx, repo)
}

// ConfigureMembership applies already-available signed membership metadata to
// this repository without contacting peers. Network reconciliation belongs to
// the running core and must not delay pairing or service installation.
func ConfigureMembership(ctx context.Context, repo *repository.Repository) error {
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return err
	}
	remotes, err := repo.Remotes(ctx)
	if err != nil {
		return err
	}
	if shared, err := membership.LoadFilesystemConfig(repo.Config.Repository, filesystemID); err == nil {
		if shared.Name != repo.Config.NetworkName {
			if err := repo.SetNetworkName(shared.Name); err != nil {
				return fmt.Errorf("apply replicated filesystem name: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load replicated filesystem configuration: %w", err)
	}
	accepted, superseded, err := acceptedMembershipState(ctx, repo, filesystemID)
	if err != nil {
		return err
	}
	revoked, err := membership.AcceptedRevocations(repo.Config.Repository, filesystemID)
	if err != nil {
		return err
	}
	for peerID := range revoked {
		if peerID == repo.Config.PeerID {
			return errors.New("this DFS peer membership has been revoked; remove the local setup before joining again")
		}
		name := remoteName(peerID)
		for _, remote := range remotes {
			if remote.Name == name {
				if err := repo.RemovePeer(ctx, name); err != nil {
					return err
				}
			}
		}
	}
	for _, remote := range supersededRemoteNames(remotes, superseded) {
		if err := repo.RemovePeer(ctx, remote); err != nil {
			return fmt.Errorf("remove superseded DFS peer %s: %w", remote, err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	for _, record := range accepted {
		if record.Payload.PeerID == repo.Config.PeerID {
			continue
		}
		if _, err := repo.AddManagedRemote(ctx, record.Payload.PeerID, executable); err != nil {
			return fmt.Errorf("configure member %s: %w", record.Payload.Name, err)
		}
	}
	clusterPins, err := membership.ActivePinPaths(repo.Config.Repository, filesystemID)
	if err != nil {
		return fmt.Errorf("load cluster pin policy: %w", err)
	}
	if err := repo.Store.ReplaceClusterPins(clusterPins); err != nil {
		return fmt.Errorf("apply cluster pin policy: %w", err)
	}
	return nil
}

func RevokeMembership(ctx context.Context, repo *repository.Repository, remote string) error {
	peerPrefix := strings.TrimPrefix(strings.TrimSpace(remote), "dfs-peer-")
	if peerPrefix == "" {
		return errors.New("managed DFS peer name is required")
	}
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return err
	}
	records, err := membership.LoadAll(repo.Config.Repository)
	if err != nil {
		return err
	}
	var target membership.Record
	for _, record := range records {
		if strings.HasPrefix(record.Payload.PeerID, peerPrefix) {
			target = record
		}
	}
	if target.Payload.PeerID == "" {
		return fmt.Errorf("membership for %s was not found", remote)
	}
	accepted, err := membership.Accepted(repo.Config.Repository, filesystemID, repo.Config.PeerID)
	if err != nil {
		return err
	}
	var local membership.Record
	for _, record := range accepted {
		if record.Payload.PeerID == repo.Config.PeerID {
			local = record
			break
		}
	}
	if local.Payload.Role != "admin" {
		return errors.New("only an administrator member can revoke a DFS peer")
	}
	private, public, err := membership.EnsureKey(repo.Config.Repository)
	if err != nil {
		return err
	}
	if public != local.Payload.SigningPublicKey {
		return errors.New("local membership signing key does not match the published record")
	}
	revocation, err := membership.Revoke(filesystemID, target.Payload.PeerID, local.Payload.PeerID, target.Payload.Generation+1, private)
	if err != nil {
		return err
	}
	if err := membership.SaveRevocation(repo.Config.Repository, revocation); err != nil {
		return err
	}
	return nil
}

func remoteName(peerID string) string {
	if len(peerID) > 12 {
		peerID = peerID[:12]
	}
	return "dfs-peer-" + peerID
}

func acceptedMembership(ctx context.Context, repo *repository.Repository, filesystemID string) ([]membership.Record, error) {
	accepted, _, err := acceptedMembershipState(ctx, repo, filesystemID)
	return accepted, err
}

func acceptedMembershipState(ctx context.Context, repo *repository.Repository, filesystemID string) ([]membership.Record, map[string]bool, error) {
	remotes, err := repo.Remotes(ctx)
	if err != nil {
		return nil, nil, err
	}
	var legacy []string
	for _, remote := range remotes {
		if strings.HasPrefix(remote.Name, "dfs-peer-") {
			short := strings.TrimPrefix(remote.Name, "dfs-peer-")
			for _, record := range mustLoadMembership(repo.Config.Repository) {
				if strings.HasPrefix(record.Payload.PeerID, short) {
					legacy = append(legacy, record.Payload.PeerID)
				}
			}
		}
	}
	accepted, err := membership.Accepted(repo.Config.Repository, filesystemID, append(legacy, repo.Config.PeerID)...)
	if err != nil {
		return nil, nil, err
	}
	current, superseded := currentMachineMemberships(accepted)
	return current, superseded, nil
}

func currentMachineMemberships(records []membership.Record) ([]membership.Record, map[string]bool) {
	return membership.CurrentMachines(records)
}

func supersededRemoteNames(remotes []repository.Remote, superseded map[string]bool) []string {
	var names []string
	for _, remote := range remotes {
		if !strings.HasPrefix(remote.Name, "dfs-peer-") {
			continue
		}
		prefix := strings.TrimPrefix(remote.Name, "dfs-peer-")
		for peerID := range superseded {
			if strings.HasPrefix(peerID, prefix) {
				names = append(names, remote.Name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

func mustLoadMembership(repositoryPath string) []membership.Record {
	records, _ := membership.LoadAll(repositoryPath)
	return records
}
