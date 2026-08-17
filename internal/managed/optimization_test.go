package managed

import (
	"bufio"
	"bytes"
	"reflect"
	"testing"

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
