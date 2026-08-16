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
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
)

const diagnosticCommand = "dfs-peer-diagnose-v1"

type RemoteDiagnostic struct {
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

type DiagnosticReport struct {
	Version      int                `json:"version"`
	FileSystemID string             `json:"filesystem_id"`
	PeerID       string             `json:"peer_id"`
	PeerName     string             `json:"peer_name"`
	Remotes      []RemoteDiagnostic `json:"remotes"`
}

type MeshPeer struct {
	PeerID   string
	PeerName string
}

type MeshConnection struct {
	FromPeerID string
	ToPeerID   string
	Status     string
	Error      string
}

type MeshReport struct {
	Peers       []MeshPeer
	Connections []MeshConnection
	Complete    bool
}

// Diagnose probes all paired DFS remotes from this peer without changing any
// refs or annex state.
func Diagnose(ctx context.Context, repo *repository.Repository, timeout time.Duration) (DiagnosticReport, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return DiagnosticReport{}, err
	}
	remotes, err := repo.Remotes(ctx)
	if err != nil {
		return DiagnosticReport{}, err
	}
	report := DiagnosticReport{
		Version: 1, FileSystemID: filesystemID, PeerID: repo.Config.PeerID, PeerName: repo.Config.Name,
	}
	for _, remote := range remotes {
		if !strings.HasPrefix(remote.Name, "dfs-peer-") {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		check := RemoteDiagnostic{Name: remote.Name, Reachable: true}
		if err := repo.ProbeRemote(probeCtx, remote.Name); err != nil {
			check.Reachable = false
			check.Error = conciseError(err)
		}
		cancel()
		report.Remotes = append(report.Remotes, check)
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
		probeTimeout = 5 * time.Second
	}
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return MeshReport{}, err
	}
	peers := map[string]MeshPeer{
		repo.Config.PeerID: {PeerID: repo.Config.PeerID, PeerName: repo.Config.Name},
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
		requestCtx, cancel := context.WithTimeout(ctx, probeTimeout+time.Second)
		report, requestErr := requestDiagnostic(requestCtx, repo, remote, probeTimeout)
		cancel()
		if requestErr != nil {
			reportErrors[peerID] = conciseError(requestErr)
			continue
		}
		if report.FileSystemID != filesystemID || pairedRemoteName(report.PeerID) != remote.Name {
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
	result := MeshReport{Complete: true}
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
	target, port, err := sshRemote(remote.URL)
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
	if report.Version != 1 || report.PeerID == "" || report.FileSystemID == "" {
		return DiagnosticReport{}, errors.New("remote returned an invalid diagnostic response")
	}
	return report, nil
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
	report, err := Diagnose(ctx, repo, 5*time.Second)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}
