package peer

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/repository"
)

func ensureLocalMembership(ctx context.Context, repo *repository.Repository, filesystemID string, identity transportIdentity, port int) (ed25519.PrivateKey, membership.Record, error) {
	private, public, err := membership.EnsureKey(repo.Config.Repository)
	if err != nil {
		return nil, membership.Record{}, err
	}
	account, err := user.Current()
	if err != nil {
		return nil, membership.Record{}, err
	}
	hostname := repo.Config.Hostname
	endpointHost := hostname
	if !strings.Contains(endpointHost, ".") {
		endpointHost += ".local"
	}
	quicEndpoint := fmt.Sprintf("quic://%s:%d", endpointHost, port)
	endpoint, err := sshURL(account.Username, endpointHost, repo.Config.Repository)
	if err != nil {
		return nil, membership.Record{}, err
	}
	for _, existing := range mustLoadMembership(repo.Config.Repository) {
		payload := existing.Payload
		if payload.PeerID != repo.Config.PeerID || payload.SigningPublicKey != public {
			continue
		}
		if payload.FileSystemID == filesystemID && payload.Name == repo.Config.Name && payload.Hostname == hostname &&
			payload.SSH.Endpoint == endpoint && payload.SSH.PublicKey == identity.PublicKey &&
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
		SSH: membership.SSHTransport{Endpoint: endpoint, PublicKey: identity.PublicKey, HostKeys: localSSHHostKeys()}, QUICEndpoint: quicEndpoint,
		Generation: 1, UpdatedAt: time.Now().UTC()}, private)
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
	if err := membership.Trust(repo.Config.Repository, record.Payload.PeerID, record.Payload.SigningPublicKey); err != nil {
		return nil, membership.Record{}, err
	}
	return private, record, nil
}

func newMembershipDraft(keyRepositoryPath, endpointRepositoryPath, filesystemID, peerID, name, sshPublicKey string, sshHostKeys []string, quicPort int) (ed25519.PrivateKey, membership.Record, error) {
	private, public, err := membership.EnsureKey(keyRepositoryPath)
	if err != nil {
		return nil, membership.Record{}, err
	}
	account, err := user.Current()
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
	endpoint, err := sshURL(account.Username, endpointHost, endpointRepositoryPath)
	if err != nil {
		return nil, membership.Record{}, err
	}
	record, err := membership.Sign(membership.Payload{Version: membership.Version, FileSystemID: filesystemID,
		PeerID: peerID, Name: name, Hostname: strings.TrimSuffix(hostname, ".local"), Role: "member", SigningPublicKey: public,
		SSH: membership.SSHTransport{Endpoint: endpoint, PublicKey: sshPublicKey, HostKeys: sshHostKeys}, QUICEndpoint: fmt.Sprintf("quic://%s:%d", endpointHost, quicPort),
		Generation: 1, UpdatedAt: time.Now().UTC()}, private)
	return private, record, err
}

func validatePairingMembership(record membership.Record, filesystemID, peerID, name, sshPublicKey, reversePath string) error {
	if err := membership.VerifySelf(record); err != nil {
		return err
	}
	payload := record.Payload
	if payload.FileSystemID != filesystemID || payload.PeerID != peerID || payload.Name != strings.TrimSpace(name) || payload.SSH.PublicKey != sshPublicKey {
		return errors.New("pairing membership does not match the authenticated peer")
	}
	endpoint, err := url.Parse(payload.SSH.Endpoint)
	if err != nil || endpoint.Scheme != "ssh" || endpoint.User == nil || endpoint.Host == "" || (reversePath != "" && filepath.Clean(endpoint.Path) != filepath.Clean(reversePath)) {
		return errors.New("pairing membership has an invalid SSH endpoint")
	}
	return nil
}

func ReconcileMembership(ctx context.Context, repo *repository.Repository) error {
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
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
	accepted, err := acceptedMembership(ctx, repo, filesystemID)
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
		if err := RevokePeerAuthorization(name); err != nil {
			return err
		}
	}
	knownHosts := filepath.Join(repo.Config.Repository, filepath.FromSlash(config.Directory), "known_hosts")
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	for _, record := range accepted {
		if record.Payload.PeerID == repo.Config.PeerID {
			continue
		}
		if err := installKnownHosts(knownHosts, record.Payload.SSH.Endpoint, record.Payload.SSH.HostKeys); err != nil {
			return fmt.Errorf("pin membership host keys for %s: %w", record.Payload.Name, err)
		}
		if _, err := authorizePeer(record.Payload.SSH.PublicKey, repo.Config.Repository, filesystemID, record.Payload.PeerID); err != nil {
			return fmt.Errorf("authorize member %s: %w", record.Payload.Name, err)
		}
		if _, err := repo.AddManagedRemote(ctx, record.Payload.PeerID, executable, record.Payload.SSH.Endpoint); err != nil {
			return fmt.Errorf("configure member %s: %w", record.Payload.Name, err)
		}
	}
	configuredKnownHosts := knownHosts
	if _, err := os.Stat(knownHosts); errors.Is(err, os.ErrNotExist) {
		configuredKnownHosts = ""
	} else if err != nil {
		return err
	}
	if err := repo.ConfigureSSHCommand(ctx, transportSSHCommand(filepath.Join(repo.Config.Repository, filepath.FromSlash(config.Directory), transportKeyFile), configuredKnownHosts)); err != nil {
		return err
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
	var local, target membership.Record
	for _, record := range records {
		if record.Payload.PeerID == repo.Config.PeerID {
			local = record
		}
		if strings.HasPrefix(record.Payload.PeerID, peerPrefix) {
			target = record
		}
	}
	if target.Payload.PeerID == "" {
		return fmt.Errorf("membership for %s was not found", remote)
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
	remotes, err := repo.Remotes(ctx)
	if err != nil {
		return nil, err
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
	return membership.Accepted(repo.Config.Repository, filesystemID, append(legacy, repo.Config.PeerID)...)
}

func mustLoadMembership(repositoryPath string) []membership.Record {
	records, _ := membership.LoadAll(repositoryPath)
	return records
}
