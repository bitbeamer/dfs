package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/peer"
	"github.com/bitbeamer/dfs/internal/repository"
)

func TestPrintNodeHealthIncludesOperationalDetailsAndActions(t *testing.T) {
	report := peer.DiagnosticReport{
		ObservedAt: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), FileSystemID: strings.Repeat("a", 40),
		NetworkName: "Home", PeerName: "iris", Role: "member", InstancePort: 7843,
		MembershipMembers: 3, ConfiguredPeers: 2, ReconciliationStatus: "ready",
		Stats: repository.HealthStats{LogicalFiles: 4, LogicalBytes: 1024, ContentFiles: 2, ContentBytes: 512,
			CacheBytes: 600, CacheLimitBytes: 2048, RepositoryBytes: 4096, MetadataBytes: 3000,
			PrivateStateBytes: 1000, DiskAvailableBytes: 1 << 30, DiskTotalBytes: 2 << 30},
		Issues: []peer.HealthIssue{{Code: "SSH_FALLBACK", Severity: "warning", Detail: "fallback", Action: "open UDP"}},
	}
	var output bytes.Buffer
	printNodeHealth(&output, report)
	for _, wanted := range []string{"Status: DEGRADED", "Namespace: 4 files", "Content: 2 local files", "Storage: repo 4.0 KiB", "Action: open UDP"} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("health output does not contain %q:\n%s", wanted, output.String())
		}
	}
}

func TestPrintNodeHealthMarksLegacyObservationUnknown(t *testing.T) {
	var output bytes.Buffer
	printNodeHealth(&output, peer.DiagnosticReport{PeerName: "old"})
	if !strings.Contains(output.String(), "Status: UNKNOWN") || !strings.Contains(output.String(), "update this peer's DFS daemon") {
		t.Fatalf("legacy health output:\n%s", output.String())
	}
}

func TestPrintMeshHealthIsCompactAndUsesPeerNames(t *testing.T) {
	observed := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	report := peer.MeshReport{
		ObservedAt: observed, NamespaceStatus: "unknown", Complete: false,
		Peers: []peer.MeshPeer{{PeerID: "a", PeerName: "cachyos"}, {PeerID: "b", PeerName: "iris"}},
		Reports: []peer.DiagnosticReport{{PeerID: "a", PeerName: "cachyos", Role: "admin", ObservedAt: observed,
			Stats: repository.HealthStats{LogicalFiles: 3, LogicalBytes: 8192}, ReconciliationStatus: "ready"}},
		Connections: []peer.MeshConnection{
			{FromPeerID: "a", ToPeerID: "b", Status: "FAILED", Error: "QUIC: context deadline exceeded; fallback: a very long internal command failed"},
			{FromPeerID: "b", ToPeerID: "a", Status: "UNREPORTED", Error: "signal: killed"},
		},
	}
	var output bytes.Buffer
	printMeshHealth(&output, report)
	text := output.String()
	for _, wanted := range []string{"Status: DEGRADED", "Responding: 1/2", "cachyos", "iris", "UNREPORTED", "connection timed out"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("mesh health output does not contain %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "dfs-peer-") || strings.Contains(text, "git ls-remote") {
		t.Fatalf("mesh health exposes internal diagnostic details:\n%s", text)
	}
}
