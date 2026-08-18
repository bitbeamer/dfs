package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/optimization"
	"github.com/bitbeamer/dfs/internal/repository"
)

type RemoteDiagnostic struct {
	Name        string `json:"name"`
	PeerID      string `json:"peer_id,omitempty"`
	PeerName    string `json:"peer_name,omitempty"`
	Reachable   bool   `json:"reachable"`
	Error       string `json:"error,omitempty"`
	ManagedQUIC bool   `json:"managed_quic"`
	Transport   string `json:"transport,omitempty"`
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
	Optimization         *optimization.State    `json:"optimization,omitempty"`
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
	Online   bool   `json:"online"`
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

type SetupAcknowledgement struct {
	PeerID   string `json:"peer_id"`
	PeerName string `json:"peer_name"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
}

// EvaluateSetupAcknowledgements treats a diagnostic response as an explicit
// acknowledgement from that peer. Every directed edge between responding
// peers must be healthy. Members without a response are retained as PENDING so
// an offline peer does not block setup and can reconcile when it returns.
func EvaluateSetupAcknowledgements(report MeshReport) ([]SetupAcknowledgement, bool) {
	reported := make(map[string]DiagnosticReport, len(report.Reports))
	for _, node := range report.Reports {
		reported[node.PeerID] = node
	}
	acknowledgements := make([]SetupAcknowledgement, 0, len(report.Peers))
	byID := make(map[string]int, len(report.Peers))
	online := make(map[string]bool, len(report.Peers))
	ready := true
	for _, member := range report.Peers {
		acknowledgement := SetupAcknowledgement{PeerID: member.PeerID, PeerName: member.PeerName, Status: "PENDING", Detail: "peer is offline or unavailable"}
		node, acknowledged := reported[member.PeerID]
		online[member.PeerID] = member.Online || acknowledged
		if acknowledged {
			acknowledgement.Status = "READY"
			acknowledgement.Detail = ""
			if node.ReconciliationStatus != "ready" {
				acknowledgement.Status = "INCOMPLETE"
				acknowledgement.Detail = "membership reconciliation has not completed"
				ready = false
			}
		} else if member.Online {
			acknowledgement.Status = "INCOMPLETE"
			acknowledgement.Detail = "online peer did not acknowledge the cluster topology"
			ready = false
		}
		acknowledgements = append(acknowledgements, acknowledgement)
		byID[member.PeerID] = len(acknowledgements) - 1
	}
	for _, connection := range report.Connections {
		if !online[connection.FromPeerID] || !online[connection.ToPeerID] || connection.Status == "OK" {
			continue
		}
		ready = false
		if index, found := byID[connection.FromPeerID]; found {
			acknowledgements[index].Status = "INCOMPLETE"
			acknowledgements[index].Detail = fmt.Sprintf("directed connection to %s is %s", setupPeerName(report.Peers, connection.ToPeerID), connection.Status)
		}
	}
	return acknowledgements, ready
}

func setupPeerName(peers []MeshPeer, peerID string) string {
	for _, member := range peers {
		if member.PeerID == peerID && member.PeerName != "" {
			return member.PeerName
		}
	}
	return peerID
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
	if state, stateErr := optimization.LoadCurrent(repo.Config.Repository, filesystemID, repo.Config.PeerID); stateErr == nil {
		report.Optimization = &state
	}
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
	for _, pin := range stats.Pinned {
		if pin.Status == "capacity-constrained" {
			report.Issues = append(report.Issues, HealthIssue{Code: "PIN_CAPACITY_CONSTRAINED", Severity: "error",
				Detail: fmt.Sprintf("%s pin %q needs %d additional byte(s), exceeding available disk space", pin.Scope, pin.Path, pin.MissingBytes),
				Action: "free disk space on this peer; cluster pin hydration will retry automatically"})
		}
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
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			managedErr := managed.Probe(probeCtx, repo, peerIDForRemote(repo.Config.Repository, remote.Name))
			cancel()
			if managedErr == nil {
				check.ManagedQUIC = true
				check.Transport = "quic"
			} else {
				check.Reachable = false
				check.Error = conciseError(managedErr)
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
		repo.Config.PeerID: {PeerID: repo.Config.PeerID, PeerName: repo.Config.Name, Online: true},
	}
	if records, membershipErr := acceptedMembership(ctx, repo, filesystemID); membershipErr == nil {
		for _, record := range records {
			peers[record.Payload.PeerID] = MeshPeer{PeerID: record.Payload.PeerID, PeerName: record.Payload.Name}
		}
	}
	if offers, discoverErr := Discover(ctx, discoveryTimeout); discoverErr == nil {
		for _, offer := range offers {
			if offer.FileSystemID == filesystemID && offer.PeerID != repo.Config.PeerID {
				peers[offer.PeerID] = MeshPeer{PeerID: offer.PeerID, PeerName: offer.PeerName, Online: true}
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
			peers[report.PeerID] = MeshPeer{PeerID: report.PeerID, PeerName: report.PeerName, Online: true}
		} else {
			peers[peerID] = MeshPeer{PeerID: report.PeerID, PeerName: report.PeerName, Online: true}
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
			} else {
				connection.Status = "OK"
			}
			if connection.Status != "OK" {
				result.Complete = false
			}
			result.Connections = append(result.Connections, connection)
		}
	}
	seenReports := make(map[string]bool)
	trees := make(map[string]bool)
	clusterPinPolicies := make(map[string]bool)
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
			for _, issue := range report.Issues {
				if issue.Severity == "error" {
					result.Complete = false
				}
			}
			var clusterPins []string
			for _, pin := range report.Stats.Pinned {
				if pin.Scope == "cluster" {
					clusterPins = append(clusterPins, pin.Path)
				}
			}
			sort.Strings(clusterPins)
			clusterPinPolicies[strings.Join(clusterPins, "\x00")] = true
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
	if len(clusterPinPolicies) > 1 {
		result.Complete = false
		result.Issues = append(result.Issues, HealthIssue{Code: "CLUSTER_PIN_POLICY_DIVERGED", Severity: "error",
			Detail: "online peers report different replicated cluster pin policies", Action: "keep peers online until reconciliation completes, then repeat dfs health --cluster"})
	}
	return result
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
	if peerID == "" {
		return DiagnosticReport{}, fmt.Errorf("membership for %s is unavailable", remote.Name)
	}
	data, err := managed.Diagnostic(ctx, repo, peerID)
	if err != nil {
		return DiagnosticReport{}, err
	}
	var report DiagnosticReport
	if err := json.Unmarshal(data, &report); err != nil {
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
