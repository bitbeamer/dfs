package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dfscore "github.com/bitbeamer/dfs/internal/daemon"
	"github.com/bitbeamer/dfs/internal/optimization"
	"github.com/bitbeamer/dfs/internal/peer"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/spf13/cobra"
)

func TestDegradedJSONRetainsHealthResult(t *testing.T) {
	app := &App{output: "json", filesystemID: strings.Repeat("a", 40)}
	app.capture.WriteString(`{"cluster":{"complete":false,"namespace_status":"unknown"}}`)
	envelope := app.jsonErrorEnvelope("dfs health", "cluster", errors.New("DFS cluster health is degraded"))
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("captured result = %#v", envelope["result"])
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"complete":false`) {
		t.Fatalf("degraded result was discarded: %s", encoded)
	}
	errorValue, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope = %#v", envelope["error"])
	}
	if code := errorValue["code"]; code != "HEALTH_DEGRADED" {
		t.Fatalf("degraded health error code = %q", code)
	}
	if envelope["filesystem_id"] != strings.Repeat("a", 40) {
		t.Fatalf("filesystem ID missing from degraded envelope: %#v", envelope)
	}
}

func TestPrintNodeHealthIncludesOperationalDetailsAndActions(t *testing.T) {
	report := peer.DiagnosticReport{
		ObservedAt: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), FileSystemID: strings.Repeat("a", 40),
		NetworkName: "Home", PeerName: "iris", Role: "member", InstancePort: 7843,
		MembershipMembers: 3, ConfiguredPeers: 2, ReconciliationStatus: "ready",
		Stats: repository.HealthStats{LogicalFiles: 4, LogicalBytes: 1024, ContentFiles: 2, ContentBytes: 512,
			CacheBytes: 600, AnnexCacheBytes: 500, RangeCacheBytes: 100, CacheLimitBytes: 2048, RepositoryBytes: 4096, MetadataBytes: 3000,
			PrivateStateBytes: 1000, DiskAvailableBytes: 1 << 30, DiskTotalBytes: 2 << 30,
			Pinned: []repository.PinnedPathHealth{{Path: "Media", Scope: "cluster", Status: "hydrating", Kind: "directory", LogicalFiles: 2, LogicalBytes: 768}}},
		ContentRead: repository.ContentReadDiagnostics{LastPath: "Media/movie.mov", LastPlan: "annex-location", LastSourcePeer: "zeus", LastOutcome: "ready", DurationMS: 42},
		Issues:      []peer.HealthIssue{{Code: "PEER_UNREACHABLE", Severity: "warning", Detail: "offline", Action: "open UDP"}},
	}
	var output bytes.Buffer
	printNodeHealth(&output, report)
	for _, wanted := range []string{"Status: DEGRADED", "Namespace: 4 files", "Content: 2 locally available paths", "Storage: repo 4.0 KiB", "annex 500 B, ranges 100 B", "Last content read: READY", "Media/movie.mov", "zeus", "42 ms", "Pinned content", "Media", "CLUSTER", "HYDRATING", "768 B", "Action: open UDP"} {
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

func TestPrintClusterHealthIsCompactAndUsesPeerNames(t *testing.T) {
	observed := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	report := peer.MeshReport{
		ObservedAt: observed, NamespaceStatus: "unknown", Complete: false,
		Peers: []peer.MeshPeer{{PeerID: "a", PeerName: "cachyos"}, {PeerID: "b", PeerName: "iris"}},
		Reports: []peer.DiagnosticReport{{PeerID: "a", PeerName: "cachyos", Role: "admin", ObservedAt: observed,
			Stats: repository.HealthStats{LogicalFiles: 3, LogicalBytes: 8192,
				Pinned: []repository.PinnedPathHealth{{Path: "Archive/file", Scope: "cluster", Status: "ready", Kind: "file", LogicalFiles: 1, LogicalBytes: 4096}}}, ReconciliationStatus: "ready"}},
		Connections: []peer.MeshConnection{
			{FromPeerID: "a", ToPeerID: "b", Status: "FAILED", Error: "QUIC: context deadline exceeded; fallback: a very long internal command failed"},
			{FromPeerID: "b", ToPeerID: "a", Status: "UNREPORTED", Error: "signal: killed"},
		},
	}
	var output bytes.Buffer
	printMeshHealth(&output, report)
	text := output.String()
	for _, wanted := range []string{"DFS CLUSTER HEALTH", "Status: DEGRADED", "Responding: 1/2", "cachyos", "iris", "UNREPORTED", "Pinned content", "Archive/file", "CLUSTER", "READY", "4.0 KiB", "connection timed out"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("cluster health output does not contain %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "dfs-peer-") || strings.Contains(text, "git ls-remote") {
		t.Fatalf("cluster health exposes internal diagnostic details:\n%s", text)
	}
}

func TestHealthUsesExplicitScopeFlag(t *testing.T) {
	root := New()
	command, _, err := root.Find([]string{"health"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Flags().Lookup("scope") == nil {
		t.Error("health command has no --scope flag")
	}
	if flag := command.Flags().Lookup("cluster"); flag == nil || !flag.Hidden {
		t.Error("health command does not hide the legacy --cluster flag")
	}
}

func TestLegacyHealthClusterFlagIsNotOverriddenByDefaultScope(t *testing.T) {
	var cluster bool
	command := &cobra.Command{Use: "health", RunE: func(*cobra.Command, []string) error { return nil }}
	command.Flags().BoolVar(&cluster, "cluster", false, "")
	addScopeFlag(command)
	command.SetArgs([]string{"--cluster"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !cluster {
		t.Fatal("legacy --cluster flag was silently reset by the default local scope")
	}
}

func TestRefreshOperationalHealthReplacesCachedObservation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git-annex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo, err := repository.InitWithIdentity(context.Background(), filepath.Join(home, "repository"), "iris", 1<<20,
		repository.GitIdentity{Name: "Health Test", Email: "health@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	stale := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	report := dfscore.HealthReport{Operational: &peer.DiagnosticReport{PeerName: "cached", ObservedAt: stale}, OperationalError: "cached error"}
	if err := refreshOperationalHealth(context.Background(), repo.Config.Repository, repo, &report, time.Second); err != nil {
		t.Fatal(err)
	}
	if report.Operational == nil || report.Operational.PeerName != "iris" || !report.Operational.ObservedAt.After(stale) || report.OperationalError != "" {
		t.Fatalf("fresh operational health did not replace cached observation: %+v", report)
	}
}

func TestOptimizeExposesLocalAndClusterScopes(t *testing.T) {
	root := New()
	command, _, err := root.Find([]string{"peer", "optimize"})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"scope", "dry-run"} {
		if command.Flags().Lookup(flag) == nil {
			t.Errorf("optimize command has no --%s flag", flag)
		}
	}
}

func TestHealthDisplaysStableSourceProfilesAndOfflineFallback(t *testing.T) {
	state := optimization.State{OptimizedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), Stale: true,
		Interactive: []optimization.RankedSource{{PeerID: "fast", PeerName: "zeus", Status: "MEASURED"}, {PeerID: "away", PeerName: "iris", Status: "OFFLINE"}},
		Bulk:        []optimization.RankedSource{{PeerID: "away", PeerName: "iris", Status: "OFFLINE"}, {PeerID: "fast", PeerName: "zeus", Status: "MEASURED"}}}
	var output bytes.Buffer
	printOptimizationState(&output, "cachyos", state)
	for _, wanted := range []string{"cachyos source priorities", "STALE", "interactive", "1. zeus", "2. iris [offline]", "bulk"} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("optimization output does not contain %q:\n%s", wanted, output.String())
		}
	}
}

func TestHealthIncludesMissingDependencyDiagnostics(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	report, err := checkEnvironment("plan9")
	if err == nil || report.Healthy {
		t.Fatalf("checkEnvironment() = %+v, %v; want degraded error", report, err)
	}
	if len(report.Checks) != 2 {
		t.Fatalf("dependency checks = %d, want 2", len(report.Checks))
	}
	var output bytes.Buffer
	printEnvironmentHealth(&output, report)
	if !strings.Contains(output.String(), "Environment: DEGRADED") || !strings.Contains(output.String(), "git") || !strings.Contains(output.String(), "MISSING") {
		t.Fatalf("environment health output:\n%s", output.String())
	}
}

func TestDoctorIsRemovedFromPublicTree(t *testing.T) {
	root := New()
	if _, _, err := root.Find([]string{"doctor"}); err == nil {
		t.Fatal("doctor remains in the public command tree")
	}
}

func TestPinCommandsExposeClusterScope(t *testing.T) {
	root := New()
	for _, name := range []string{"pin", "unpin"} {
		command, _, err := root.Find([]string{"content", name})
		if err != nil {
			t.Fatal(err)
		}
		if command.Flags().Lookup("scope") == nil {
			t.Errorf("%s command has no --scope flag", name)
		}
	}
}
