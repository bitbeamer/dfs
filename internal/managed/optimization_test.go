package managed

import (
	"bufio"
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/optimization"
)

func TestRankSourcesUsesProfileAndKeepsOfflinePeersLast(t *testing.T) {
	measurements := []optimization.Measurement{
		{PeerID: "offline", PeerName: "Offline", Status: "OFFLINE"},
		{PeerID: "latency", PeerName: "Latency", Status: "MEASURED", TTFBMedianMS: 2, InteractiveMedianMbps: 20, BulkMedianMbps: 50},
		{PeerID: "bulk", PeerName: "Bulk", Status: "MEASURED", TTFBMedianMS: 10, InteractiveMedianMbps: 40, BulkMedianMbps: 900},
		{PeerID: "degraded", PeerName: "Degraded", Status: "DEGRADED", TTFBMedianMS: 1, BulkMedianMbps: 1000},
	}
	ids := func(values []optimization.RankedSource) []string {
		result := make([]string, 0, len(values))
		for _, value := range values {
			result = append(result, value.PeerID)
		}
		return result
	}
	if got, want := ids(rankSources(measurements, false)), []string{"latency", "bulk", "degraded", "offline"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("interactive ranking = %v, want %v", got, want)
	}
	if got, want := ids(rankSources(measurements, true)), []string{"bulk", "latency", "degraded", "offline"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bulk ranking = %v, want %v", got, want)
	}
}

func TestKnownNonHolderCacheDoesNotRewriteRankings(t *testing.T) {
	availability := &contentAvailability{entries: make(map[string]time.Time)}
	availability.mark("repo", "peer", "key")
	if !availability.isKnown("repo", "peer", "key") {
		t.Fatal("known non-holder was not cached")
	}
	if availability.isKnown("repo", "other", "key") {
		t.Fatal("non-holder state leaked to another peer")
	}
	availability.clear("repo", "peer", "key")
	if availability.isKnown("repo", "peer", "key") {
		t.Fatal("successful source did not clear non-holder state")
	}
}

func TestContentCandidatesRetryStableOrderWhenEverySourceWasUnavailable(t *testing.T) {
	original := unavailableContent
	originalPeers := peerAvailability
	unavailableContent = &contentAvailability{entries: make(map[string]time.Time)}
	peerAvailability = &peerCircuit{entries: make(map[string]peerCircuitEntry)}
	t.Cleanup(func() {
		unavailableContent = original
		peerAvailability = originalPeers
	})
	peerIDs := []string{"first", "second"}
	unavailableContent.mark("repo", "first", "key")
	if got, want := contentCandidates("repo", "key", peerIDs), []string{"second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate after one miss = %v, want %v", got, want)
	}
	unavailableContent.mark("repo", "second", "key")
	if got := contentCandidates("repo", "key", peerIDs); !reflect.DeepEqual(got, peerIDs) {
		t.Fatalf("all-missed retry order = %v, want stable %v", got, peerIDs)
	}
}

func TestContentCandidatesSkipCircuitOpenPeer(t *testing.T) {
	original := peerAvailability
	peerAvailability = &peerCircuit{entries: make(map[string]peerCircuitEntry)}
	t.Cleanup(func() { peerAvailability = original })
	peerAvailability.markFailure("repo", "offline")
	if got, want := contentCandidates("repo", "key", []string{"offline", "online"}), []string{"online"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("circuit-filtered candidates = %v, want %v", got, want)
	}
	if got := contentCandidates("repo", "key", []string{"offline"}); len(got) != 0 {
		t.Fatalf("all-offline candidates = %v, want none", got)
	}
}

func TestBenchmarkPayloadIsDeterministic(t *testing.T) {
	const offset = int64(7919)
	payload := make([]byte, 4096)
	for index := range payload {
		payload[index] = benchmarkByte(offset + int64(index))
	}
	if err := validateBenchmark(bufio.NewReader(bytes.NewReader(payload)), offset, int64(len(payload))); err != nil {
		t.Fatalf("validate deterministic benchmark: %v", err)
	}
	payload[len(payload)/2]++
	if err := validateBenchmark(bufio.NewReader(bytes.NewReader(payload)), offset, int64(len(payload))); err == nil {
		t.Fatal("corrupt benchmark payload passed validation")
	}
}
