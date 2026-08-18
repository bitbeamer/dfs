package cli

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/peer"
	dfssetup "github.com/bitbeamer/dfs/internal/setup"
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
