package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/peer"
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
