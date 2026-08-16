package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/repository"
)

const diagnosticCommand = "dfs-peer-diagnose-v2"

type RemoteDiagnostic struct {
	Name                 string `json:"name"`
	PeerID               string `json:"peer_id,omitempty"`
	PeerName             string `json:"peer_name,omitempty"`
	Reachable            bool   `json:"reachable"`
	Error                string `json:"error,omitempty"`
	PasswordlessSSH      bool   `json:"passwordless_ssh"`
	PasswordlessSSHError string `json:"passwordless_ssh_error,omitempty"`
	ManagedQUIC          bool   `json:"managed_quic"`
	ManagedQUICError     string `json:"managed_quic_error,omitempty"`
	Transport            string `json:"transport,omitempty"`
}

type DiagnosticReport struct {
	Version              int                    `json:"version"`
	ObservedAt           time.Time              `json:"observed_at"`
	FileSystemID         string                 `json:"filesystem_id"`
	NetworkName          string                 `json:"network_name"`
	PeerID               string                 `json:"peer_id"`
	PeerName             string                 `json:"peer_name"`
	Role                 string                 `json:"role"`
	Endpoint             string                 `json:"endpoint,omitempty"`
	InstancePort         int                    `json:"instance_port,omitempty"`
	TreeID               string                 `json:"tree_id,omitempty"`
	MembershipMembers    int                    `json:"membership_members"`
	ConfiguredPeers      int                    `json:"configured_peers"`
	ReconciliationStatus string                 `json:"reconciliation_status"`
	Stats                repository.HealthStats `json:"stats"`
	Issues               []HealthIssue          `json:"issues,omitempty"`
	Remotes              []RemoteDiagnostic     `json:"remotes"`
}

type HealthIssue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
	Action   string `json:"action"`
}

type MeshPeer struct {
	PeerID   string `json:"peer_id"`
	PeerName string `json:"peer_name"`
}

type MeshConnection struct {
	FromPeerID string `json:"from_peer_id"`
	ToPeerID   string `json:"to_peer_id"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

type MeshReport struct {
	ObservedAt      time.Time          `json:"observed_at"`
	Peers           []MeshPeer         `json:"peers"`
	Connections     []MeshConnection   `json:"connections"`
	Reports         []DiagnosticReport `json:"reports"`
	NamespaceStatus string             `json:"namespace_status"`
	Issues          []HealthIssue      `json:"issues,omitempty"`
	Complete        bool               `json:"complete"`
}

// Diagnose probes all paired DFS remotes from this peer without changing any
// refs or annex state.
func Diagnose(ctx context.Context, repo *repository.Repository, timeout time.Duration) (DiagnosticReport, error) {
	if err := config.ValidateHostname(repo.Config); err != nil {
		return DiagnosticReport{}, err
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return DiagnosticReport{}, err
	}
	remotes, err := repo.Remotes(ctx)
	if err != nil {
		return DiagnosticReport{}, err
	}
	stats, err := repo.HealthStats(ctx)
	if err != nil {
		return DiagnosticReport{}, fmt.Errorf("collect repository health: %w", err)
	}
	treeID, err := repo.TreeID(ctx)
	if err != nil {
		return DiagnosticReport{}, fmt.Errorf("inspect namespace tree: %w", err)
	}
	report := DiagnosticReport{Version: 2, ObservedAt: time.Now().UTC(), FileSystemID: filesystemID,
		NetworkName: repo.Config.NetworkName, PeerID: repo.Config.PeerID, PeerName: repo.Config.Name,
		TreeID: treeID, ConfiguredPeers: len(remotes), Stats: stats, ReconciliationStatus: "ready"}
	if state, stateErr := readRuntimeState(repo.Config.Repository); stateErr == nil {
		report.Endpoint = state.Endpoint
		if endpoint, parseErr := url.Parse(state.Endpoint); parseErr == nil {
			report.InstancePort, _ = strconv.Atoi(endpoint.Port())
		}
	}
	membersByRemote := make(map[string]MeshPeer)
	if records, membershipErr := acceptedMembership(ctx, repo, filesystemID); membershipErr == nil {
		report.MembershipMembers = len(records)
		for _, record := range records {
			membersByRemote[pairedRemoteName(record.Payload.PeerID)] = MeshPeer{
				PeerID: record.Payload.PeerID, PeerName: record.Payload.Name,
			}
			if record.Payload.PeerID == repo.Config.PeerID {
				report.Role = record.Payload.Role
			}
		}
		if expected := len(records) - 1; expected > len(remotes) {
			report.ReconciliationStatus = "pending"
			report.Issues = append(report.Issues, HealthIssue{Code: "RECONCILIATION_PENDING", Severity: "warning",
				Detail: fmt.Sprintf("%d accepted member(s) are not configured as local peers", expected-len(remotes)),
				Action: "keep peers online and run dfs sync; use dfs health --cluster to identify incomplete edges"})
		}
	}
	if report.Role == "" {
		report.Role = "unknown"
	}
	if stats.MissingPinnedFiles > 0 {
		report.Issues = append(report.Issues, HealthIssue{Code: "PINNED_CONTENT_MISSING", Severity: "error",
			Detail: fmt.Sprintf("%d pinned file(s) are not held locally", stats.MissingPinnedFiles),
			Action: "run dfs sync and verify that another peer or durable remote still holds the content"})
	}
	if stats.CacheLimitBytes > 0 && stats.CacheBytes > stats.CacheLimitBytes {
		report.Issues = append(report.Issues, HealthIssue{Code: "CACHE_OVER_LIMIT", Severity: "warning",
			Detail: "local annex object storage exceeds the configured cache limit", Action: "run dfs cache prune"})
	}
	if stats.DiskTotalBytes > 0 && stats.DiskAvailableBytes < 1<<30 && stats.DiskAvailableBytes < stats.DiskTotalBytes/20 {
		report.Issues = append(report.Issues, HealthIssue{Code: "DISK_SPACE_LOW", Severity: "warning",
			Detail: "less than 1 GiB and 5% of the repository filesystem remains available", Action: "free disk space or reduce the local cache limit"})
	}
	checks := make([]RemoteDiagnostic, len(remotes))
	var checksWait sync.WaitGroup
	for index, remote := range remotes {
		if !strings.HasPrefix(remote.Name, "dfs-peer-") {
			continue
		}
		checksWait.Add(1)
		go func(index int, remote repository.Remote) {
			defer checksWait.Done()
			member := membersByRemote[remote.Name]
			check := RemoteDiagnostic{Name: remote.Name, PeerID: member.PeerID, PeerName: member.PeerName, Reachable: true}
			managedResult := make(chan error, 1)
			fallbackResult := make(chan error, 1)
			sshResult := make(chan error, 1)
			go func() {
				probeCtx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				managedResult <- managed.Probe(probeCtx, repo, peerIDForRemote(repo.Config.Repository, remote.Name))
			}()
			go func() {
				probeCtx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				fallbackResult <- repo.ProbeSSHFallback(probeCtx, remote.Name)
			}()
			go func() {
				sshCtx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				sshURL := remote.URL
				if fallback, fallbackErr := repo.PeerSSHFallback(sshCtx, remote.Name); fallbackErr == nil {
					sshURL = fallback
				}
				sshResult <- probePasswordlessSSH(sshCtx, sshURL, timeout)
			}()
			managedErr := <-managedResult
			fallbackErr := <-fallbackResult
			if managedErr == nil {
				check.ManagedQUIC = true
				check.Transport = "quic"
			} else {
				check.ManagedQUICError = conciseError(managedErr)
				if fallbackErr == nil {
					check.Transport = "ssh-fallback"
				} else {
					check.Reachable = false
					check.Error = "QUIC: " + conciseError(managedErr) + "; fallback: " + conciseError(fallbackErr)
				}
			}
			if err := <-sshResult; err != nil {
				check.PasswordlessSSHError = conciseError(err)
			} else {
				check.PasswordlessSSH = true
			}
			checks[index] = check
		}(index, remote)
	}
	checksWait.Wait()
	for _, check := range checks {
		if check.Name != "" {
			report.Remotes = append(report.Remotes, check)
			if !check.Reachable {
				report.Issues = append(report.Issues, HealthIssue{Code: "PEER_UNREACHABLE", Severity: "warning",
					Detail: check.Name + ": " + check.Error, Action: "check the peer daemon and firewall, then run dfs health --cluster"})
			} else if check.Transport == "ssh-fallback" {
				report.Issues = append(report.Issues, HealthIssue{Code: "SSH_FALLBACK", Severity: "warning",
					Detail: check.Name + " is reachable only through SSH fallback", Action: "check UDP reachability for the peer's managed DFS port"})
			}
			if check.Reachable && !check.PasswordlessSSH {
				report.Issues = append(report.Issues, HealthIssue{Code: "PASSWORDLESS_SSH_FAILED", Severity: "error",
					Detail: check.Name + ": " + check.PasswordlessSSHError, Action: "configure ordinary non-interactive SSH for this directed peer path"})
			}
		}
	}
	sort.Slice(report.Remotes, func(i, j int) bool { return report.Remotes[i].Name < report.Remotes[j].Name })
	return report, nil
}

// CheckMesh asks every configured or discovered peer in this filesystem to run
// Diagnose and evaluates every directed edge between those peers and the local
// peer. Configured remotes are authoritative mesh members: discovery is only a
// supplement, so a broken or blocked mDNS advertisement cannot make doctor
// silently omit a peer.
func CheckMesh(ctx context.Context, repo *repository.Repository, discoveryTimeout, probeTimeout time.Duration) (MeshReport, error) {
	if discoveryTimeout <= 0 {
		discoveryTimeout = 2 * time.Second
	}
	if probeTimeout <= 0 {
		probeTimeout = 10 * time.Second
	}
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return MeshReport{}, err
	}
	peers := map[string]MeshPeer{
		repo.Config.PeerID: {PeerID: repo.Config.PeerID, PeerName: repo.Config.Name},
	}
	if records, membershipErr := acceptedMembership(ctx, repo, filesystemID); membershipErr == nil {
		for _, record := range records {
			peers[record.Payload.PeerID] = MeshPeer{PeerID: record.Payload.PeerID, PeerName: record.Payload.Name}
		}
	}
	if offers, discoverErr := Discover(ctx, discoveryTimeout); discoverErr == nil {
		for _, offer := range offers {
			if offer.FileSystemID == filesystemID && offer.PeerID != repo.Config.PeerID {
				peers[offer.PeerID] = MeshPeer{PeerID: offer.PeerID, PeerName: offer.PeerName}
			}
		}
	}
	remotes, err := repo.Remotes(ctx)
	if err != nil {
		return MeshReport{}, err
	}

	reports := make(map[string]DiagnosticReport)
	reportErrors := make(map[string]string)
	local, err := Diagnose(ctx, repo, probeTimeout)
	if err != nil {
		return MeshReport{}, err
	}
	reports[local.PeerID] = local

	type reportResult struct {
		peerID     string
		remoteName string
		report     DiagnosticReport
		err        error
	}
	results := make(chan reportResult, len(remotes))
	requests := 0
	for _, remote := range remotes {
		if !strings.HasPrefix(remote.Name, "dfs-peer-") {
			continue
		}
		remote := remote
		peerID := meshPeerIDForRemote(peers, remote.Name)
		if peerID == "" {
			peerID = strings.TrimPrefix(remote.Name, "dfs-peer-")
			peers[peerID] = MeshPeer{PeerID: peerID, PeerName: remote.Name}
		}
		requests++
		go func(peerID string) {
			requestCtx, cancel := context.WithTimeout(ctx, probeTimeout+time.Second)
			defer cancel()
			report, requestErr := requestDiagnostic(requestCtx, repo, remote, probeTimeout)
			results <- reportResult{peerID: peerID, remoteName: remote.Name, report: report, err: requestErr}
		}(peerID)
	}
	for range requests {
		result := <-results
		peerID, report, requestErr := result.peerID, result.report, result.err
		if requestErr != nil {
			reportErrors[peerID] = conciseError(requestErr)
			continue
		}
		if report.FileSystemID != filesystemID || pairedRemoteName(report.PeerID) != result.remoteName {
			reportErrors[peerID] = "remote diagnostic returned a different filesystem or peer identity"
			continue
		}
		if report.PeerID != peerID {
			delete(peers, peerID)
			delete(reportErrors, peerID)
			peers[report.PeerID] = MeshPeer{PeerID: report.PeerID, PeerName: report.PeerName}
		} else {
			peers[peerID] = MeshPeer{PeerID: report.PeerID, PeerName: report.PeerName}
		}
		reports[peerID] = report
		reports[report.PeerID] = report
	}
	return evaluateMesh(peers, reports, reportErrors), nil
}

func meshPeerIDForRemote(peers map[string]MeshPeer, remoteName string) string {
	for peerID := range peers {
		if pairedRemoteName(peerID) == remoteName {
			return peerID
		}
	}
	return ""
}

func evaluateMesh(peers map[string]MeshPeer, reports map[string]DiagnosticReport, reportErrors map[string]string) MeshReport {
	result := MeshReport{ObservedAt: time.Now().UTC(), Complete: true, NamespaceStatus: "converged"}
	for _, participant := range peers {
		result.Peers = append(result.Peers, participant)
	}
	sort.Slice(result.Peers, func(i, j int) bool {
		if result.Peers[i].PeerName != result.Peers[j].PeerName {
			return result.Peers[i].PeerName < result.Peers[j].PeerName
		}
		return result.Peers[i].PeerID < result.Peers[j].PeerID
	})
	for _, from := range result.Peers {
		for _, to := range result.Peers {
			if from.PeerID == to.PeerID {
				continue
			}
			connection := MeshConnection{FromPeerID: from.PeerID, ToPeerID: to.PeerID}
			report, reported := reports[from.PeerID]
			if !reported {
				connection.Status = "UNREPORTED"
				connection.Error = reportErrors[from.PeerID]
			} else if check, found := diagnosticFor(report, pairedRemoteName(to.PeerID)); !found {
				connection.Status = "NOT_CONFIGURED"
				connection.Error = "paired remote is missing"
			} else if !check.Reachable {
				connection.Status = "FAILED"
				connection.Error = check.Error
			} else if !check.PasswordlessSSH {
				connection.Status = "PASSWORDLESS_SSH_FAILED"
				connection.Error = check.PasswordlessSSHError
			} else if check.Transport == "ssh-fallback" {
				connection.Status = "SSH_FALLBACK"
				connection.Error = check.ManagedQUICError
			} else {
				connection.Status = "OK"
			}
			if connection.Status != "OK" && connection.Status != "SSH_FALLBACK" {
				result.Complete = false
			}
			result.Connections = append(result.Connections, connection)
		}
	}
	seenReports := make(map[string]bool)
	trees := make(map[string]bool)
	missingTree := false
	for _, participant := range result.Peers {
		if report, found := reports[participant.PeerID]; found && !seenReports[report.PeerID] {
			seenReports[report.PeerID] = true
			result.Reports = append(result.Reports, report)
			if report.TreeID != "" {
				trees[report.TreeID] = true
			} else {
				missingTree = true
			}
		}
	}
	sort.Slice(result.Reports, func(i, j int) bool { return result.Reports[i].PeerName < result.Reports[j].PeerName })
	if len(trees) > 1 {
		result.NamespaceStatus = "inconsistent"
		result.Complete = false
		result.Issues = append(result.Issues, HealthIssue{Code: "NAMESPACE_DIVERGED", Severity: "error",
			Detail: "online peers report different logical namespace trees", Action: "run dfs sync on the affected peers and repeat dfs health --cluster"})
	} else if len(result.Reports) < len(result.Peers) || missingTree {
		result.NamespaceStatus = "unknown"
		result.Complete = false
	}
	return result
}

func probePasswordlessSSH(ctx context.Context, remoteURL string, timeout time.Duration) error {
	target, port, err := sshRemote(remoteURL)
	if err != nil {
		return err
	}
	seconds := int((timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectTimeout=" + strconv.Itoa(seconds),
	}
	if port != "" && port != "22" {
		args = append(args, "-p", port)
	}
	args = append(args, "--", target, "true")
	command := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("passwordless SSH to %s failed: %s", target, message)
	}
	return nil
}

func pairedRemoteName(peerID string) string {
	if len(peerID) > 12 {
		peerID = peerID[:12]
	}
	return "dfs-peer-" + strings.ToLower(peerID)
}

func diagnosticFor(report DiagnosticReport, name string) (RemoteDiagnostic, bool) {
	for _, check := range report.Remotes {
		if check.Name == name {
			return check, true
		}
	}
	return RemoteDiagnostic{}, false
}

func requestDiagnostic(ctx context.Context, repo *repository.Repository, remote repository.Remote, timeout time.Duration) (DiagnosticReport, error) {
	peerID := peerIDForRemote(repo.Config.Repository, remote.Name)
	if peerID != "" {
		if data, managedErr := managed.Diagnostic(ctx, repo, peerID); managedErr == nil {
			var report DiagnosticReport
			if err := json.Unmarshal(data, &report); err == nil && report.Version == 2 && report.PeerID != "" && report.FileSystemID != "" {
				return report, nil
			}
		}
	}
	sshURL := remote.URL
	if fallback, fallbackErr := repo.PeerSSHFallback(ctx, remote.Name); fallbackErr == nil {
		sshURL = fallback
	}
	target, port, err := sshRemote(sshURL)
	if err != nil {
		return DiagnosticReport{}, fmt.Errorf("%s: %w", remote.Name, err)
	}
	stateDirectory := filepath.Join(repo.Config.Repository, filepath.FromSlash(config.Directory))
	privateKey := filepath.Join(stateDirectory, transportKeyFile)
	if _, statErr := os.Stat(privateKey); statErr != nil {
		return DiagnosticReport{}, fmt.Errorf("paired SSH identity unavailable: %w", statErr)
	}
	knownHosts := filepath.Join(stateDirectory, "known_hosts")
	if _, statErr := os.Stat(knownHosts); statErr != nil {
		return DiagnosticReport{}, fmt.Errorf("pinned peer host keys unavailable: %w", statErr)
	}
	args := []string{
		"-i", privateKey, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + knownHosts, "-o", "StrictHostKeyChecking=yes",
	}
	seconds := int((timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	args = append(args, "-o", "ConnectTimeout="+strconv.Itoa(seconds))
	if port != "" && port != "22" {
		args = append(args, "-p", port)
	}
	args = append(args, "--", target, diagnosticCommand)
	command := exec.CommandContext(ctx, "ssh", args...)
	command.Dir = repo.Config.Repository
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return DiagnosticReport{}, errors.New(message)
	}
	var report DiagnosticReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return DiagnosticReport{}, fmt.Errorf("decode diagnostic response: %w", err)
	}
	if report.Version != 2 || report.PeerID == "" || report.FileSystemID == "" {
		return DiagnosticReport{}, errors.New("remote returned an invalid diagnostic response")
	}
	return report, nil
}

func peerIDForRemote(repositoryPath, remoteName string) string {
	prefix := strings.TrimPrefix(remoteName, "dfs-peer-")
	for _, record := range mustLoadMembership(repositoryPath) {
		if strings.HasPrefix(record.Payload.PeerID, prefix) {
			return record.Payload.PeerID
		}
	}
	return prefix
}

func sshRemote(value string) (target, port string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "ssh" || parsed.Hostname() == "" {
		return "", "", errors.New("paired remote is not an ssh:// URL")
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if parsed.User != nil && parsed.User.Username() != "" {
		host = parsed.User.Username() + "@" + host
	}
	return host, parsed.Port(), nil
}

func conciseError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 500 {
		message = message[:497] + "..."
	}
	return message
}

func serveDiagnostic(repositoryPath string, output io.Writer) error {
	repo, err := repository.Open(repositoryPath)
	if err != nil {
		return err
	}
	defer repo.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := Diagnose(ctx, repo, 10*time.Second)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}
