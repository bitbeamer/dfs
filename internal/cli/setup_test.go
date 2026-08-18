package cli

import (
	"bufio"
	"bytes"
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/peer"
	dfssetup "github.com/bitbeamer/dfs/internal/setup"
	"github.com/spf13/cobra"
)

func TestDiscoverSetupNetworksReportsProgress(t *testing.T) {
	var output bytes.Buffer
	offers, err := discoverSetupNetworks(context.Background(), &output, 10*time.Second, 5*time.Millisecond,
		func(context.Context, time.Duration) ([]peer.Offer, error) {
			time.Sleep(20 * time.Millisecond)
			return []peer.Offer{{PeerName: "ares"}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 1 {
		t.Fatalf("offers = %d, want 1", len(offers))
	}
	for _, wanted := range []string{"Searching for DFS filesystems", "up to 10s", "Still searching for DFS filesystems", "Discovery finished: 1 network offer(s) found"} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("discovery output does not contain %q:\n%s", wanted, output.String())
		}
	}
}

func TestDiscoverSetupNetworksHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := discoverSetupNetworks(ctx, &bytes.Buffer{}, time.Minute, time.Millisecond,
		func(ctx context.Context, _ time.Duration) ([]peer.Offer, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	if err == nil {
		t.Fatal("cancelled setup discovery unexpectedly succeeded")
	}
}

func TestEnsureGitIdentityCollectsAndValidatesMissingValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	var output bytes.Buffer
	name, email, err := ensureGitIdentity(context.Background(), bufio.NewReader(strings.NewReader("Otto\notto@example.com\n")), &output, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Otto" || email != "otto@example.com" {
		t.Fatalf("identity = %q <%s>", name, email)
	}
	for _, wanted := range []string{"Git author name", "Git author email", "Git author identity: Otto <otto@example.com>"} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("identity output does not contain %q:\n%s", wanted, output.String())
		}
	}
}

func TestEnsureGitIdentityRejectsMissingNonInteractiveValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	_, _, err := ensureGitIdentity(context.Background(), bufio.NewReader(strings.NewReader("")), &bytes.Buffer{}, "", "", true)
	if err == nil || !strings.Contains(err.Error(), "--git-name") {
		t.Fatalf("non-interactive identity error = %v", err)
	}
}

func TestSetupFilesystemNamePrefersDisplayName(t *testing.T) {
	state := &dfssetup.State{NetworkName: "Home Files", FileSystemID: strings.Repeat("a", 40)}
	if got := setupFilesystemName(state); got != "Home Files" {
		t.Fatalf("setup filesystem name = %q", got)
	}
}

func TestConsolidatedServiceCommandsAreExposed(t *testing.T) {
	root := New()
	for _, path := range [][]string{{"service", "list"}, {"service", "show"}, {"service", "start"}, {"service", "stop"}, {"service", "restart"}, {"service", "repair"}, {"service", "uninstall"}, {"upgrade"}} {
		command, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if command.Name() != path[len(path)-1] {
			t.Fatalf("command %v resolved to %s", path, command.Name())
		}
	}
}

func TestPublicCommandTreeUsesConsolidatedRoots(t *testing.T) {
	root := New()
	wanted := map[string]bool{"setup": true, "filesystem": true, "service": true, "upgrade": true, "peer": true, "content": true,
		"cache": true, "storage": true, "sync": true, "health": true, "history": true}
	for _, command := range root.Commands() {
		if command.Hidden || command.Name() == "completion" || command.Name() == "help" {
			continue
		}
		if !wanted[command.Name()] {
			t.Errorf("unexpected public root %q", command.Name())
		}
		delete(wanted, command.Name())
	}
	for missing := range wanted {
		t.Errorf("missing public root %q", missing)
	}
	for _, legacy := range []string{"init", "join", "instance", "network", "pair", "relay", "optimize", "fetch", "pin", "unpin", "evict", "status", "doctor", "restore", "conflicts"} {
		if _, _, err := root.Find([]string{legacy}); err == nil {
			t.Errorf("legacy root %q remains public", legacy)
		}
	}
	for _, legacy := range []string{"transport", "daemon", "mount", "unmount"} {
		command, _, err := root.Find([]string{legacy})
		if err != nil || !command.Hidden {
			t.Errorf("runtime compatibility root %q is not hidden", legacy)
		}
	}
}

func TestConsolidatedPublicLeafCommands(t *testing.T) {
	root := New()
	wanted := map[string][]string{
		"setup":      {"abort", "create", "join", "resume"},
		"filesystem": {"rename", "show"},
		"service":    {"list", "repair", "restart", "show", "start", "stop", "uninstall"},
		"peer":       {"approve", "check", "invite", "list", "optimize", "reject", "relay", "remove", "requests"},
		"content":    {"evict", "fetch", "pin", "unpin"},
		"cache":      {"limit", "prune", "show"},
		"storage":    {"add", "copy", "enable", "list", "remove", "show"},
		"history":    {"conflicts", "list", "restore"},
	}
	for parentName, expected := range wanted {
		parent, _, err := root.Find([]string{parentName})
		if err != nil {
			t.Fatal(err)
		}
		var actual []string
		for _, child := range parent.Commands() {
			if !child.Hidden && child.Name() != "help" {
				actual = append(actual, child.Name())
			}
		}
		sort.Strings(actual)
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s children = %#v, want %#v", parentName, actual, expected)
		}
	}
}

func TestMaterialMutationsExposeDryRun(t *testing.T) {
	root := New()
	paths := [][]string{
		{"setup", "abort"}, {"setup", "create"}, {"setup", "join"}, {"setup", "resume"},
		{"filesystem", "rename"}, {"service", "start"}, {"service", "stop"}, {"service", "restart"}, {"service", "repair"}, {"service", "uninstall"},
		{"upgrade"}, {"peer", "approve"}, {"peer", "reject"}, {"peer", "remove"}, {"peer", "invite", "create"}, {"peer", "invite", "revoke"},
		{"peer", "optimize"}, {"peer", "relay", "set"}, {"peer", "relay", "clear"}, {"content", "fetch"}, {"content", "pin"}, {"content", "unpin"},
		{"content", "evict"}, {"cache", "limit"}, {"cache", "prune"}, {"storage", "add", "s3"}, {"storage", "enable"}, {"storage", "copy"},
		{"storage", "remove"}, {"sync"}, {"history", "restore"},
	}
	for _, path := range paths {
		command, _, err := root.Find(path)
		if err != nil {
			t.Errorf("find %v: %v", path, err)
			continue
		}
		if command.Flags().Lookup("dry-run") == nil {
			t.Errorf("%s has no --dry-run", command.CommandPath())
		}
	}
}

func TestServiceUninstallPurgeRequiresExplicitApproval(t *testing.T) {
	command := New()
	command.SetArgs([]string{"service", "uninstall", "--purge"})
	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires --yes") {
		t.Fatalf("unapproved purge error = %v", err)
	}
}

func TestLegacySetupAbortSkipsCreateOrJoinPrompt(t *testing.T) {
	command := New()
	var stderr bytes.Buffer
	command.SetErr(&stderr)
	command.SetArgs([]string{"setup", "--abort", "--repository", t.TempDir()})
	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no DFS setup transaction is recorded") {
		t.Fatalf("legacy setup abort error = %v", err)
	}
	if strings.Contains(stderr.String(), "Create or join") {
		t.Fatalf("legacy setup abort prompted for setup mode: %q", stderr.String())
	}
}

func TestSetupAbortYesSkipsConfirmation(t *testing.T) {
	command := New()
	var stderr bytes.Buffer
	command.SetErr(&stderr)
	command.SetArgs([]string{"setup", "abort", "--data-dir", t.TempDir(), "--yes"})
	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no DFS setup transaction is recorded") {
		t.Fatalf("setup abort --yes error = %v", err)
	}
	if strings.Contains(stderr.String(), "Continue?") {
		t.Fatalf("setup abort --yes prompted for confirmation: %q", stderr.String())
	}
}

func TestPublicCommandHelpMatchesConsolidatedTree(t *testing.T) {
	root := New()
	wanted := map[string]string{
		"dfs setup":             "Provision or recover a DFS filesystem",
		"dfs setup create":      "Create, install, mount, and verify a new filesystem",
		"dfs setup join":        "Discover, join, install, mount, and verify a filesystem",
		"dfs filesystem":        "Inspect and manage the selected DFS filesystem",
		"dfs cache show":        "Show cache use, limit, and pins",
		"dfs history list":      "Show namespace history",
		"dfs peer relay show":   "Show relay configuration",
		"dfs service uninstall": "Remove local DFS services and optionally their repositories",
	}
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		if !command.Hidden && command.Name() != "help" && command.Name() != "completion" {
			if strings.TrimSpace(command.Short) == "" {
				t.Errorf("%s has no short help description", command.CommandPath())
			}
			if expected, found := wanted[command.CommandPath()]; found {
				if command.Short != expected {
					t.Errorf("%s short help = %q, want %q", command.CommandPath(), command.Short, expected)
				}
				delete(wanted, command.CommandPath())
			}
		}
		for _, child := range command.Commands() {
			visit(child)
		}
	}
	visit(root)
	for path := range wanted {
		t.Errorf("help audit did not find %s", path)
	}
	uninstall, _, err := root.Find([]string{"service", "uninstall"})
	if err != nil {
		t.Fatal(err)
	}
	if uninstall.Flags().Lookup("purge") == nil || !strings.Contains(uninstall.Long, "Membership records held by other peers are not revoked") {
		t.Fatalf("service uninstall purge help is incomplete: %q", uninstall.Long)
	}
}

func TestPublicCommandHelpDoesNotReferenceLegacyCommands(t *testing.T) {
	root := New()
	legacy := []string{
		"dfs setup --resume", "dfs setup --abort", "dfs pair ", "dfs network ",
		"dfs unmount ", "dfs status", "dfs doctor", "dfs fetch ", "dfs pin ",
		"dfs unpin ", "dfs evict ", "dfs restore ", "dfs conflicts",
	}
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		if command.Hidden {
			return
		}
		help := strings.Join([]string{command.Short, command.Long, command.Example}, "\n")
		for _, stale := range legacy {
			if strings.Contains(help, stale) {
				t.Errorf("%s help references legacy command %q", command.CommandPath(), stale)
			}
		}
		for _, child := range command.Commands() {
			visit(child)
		}
	}
	visit(root)
}
