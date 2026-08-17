package managed

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/optimization"
	"github.com/bitbeamer/dfs/internal/repository"
	quic "github.com/quic-go/quic-go"
)

const (
	optimizationSamples = 3
	interactiveBytes    = 64 << 10
	bulkBytes           = 4 << 20
	benchmarkMaxBytes   = 8 << 20
	benchmarkTimeout    = 10 * time.Second
)

type OptimizationProgress struct {
	PeerID   string
	PeerName string
	Stage    string
	Sample   int
	Samples  int
}

type PeerOptimization struct {
	PeerID   string             `json:"peer_id"`
	PeerName string             `json:"peer_name"`
	State    optimization.State `json:"state,omitempty"`
	Error    string             `json:"error,omitempty"`
}

type ClusterOptimization struct {
	OptimizedAt time.Time          `json:"optimized_at"`
	Peers       []PeerOptimization `json:"peers"`
}

type benchmarkResult struct {
	latencyMS float64
	ttfbMS    float64
	mbps      float64
}

func OptimizeLocal(ctx context.Context, repo *repository.Repository, progress func(OptimizationProgress)) (optimization.State, error) {
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return optimization.State{}, err
	}
	members, err := optimization.CurrentMembers(repo.Config.Repository, filesystemID, repo.Config.PeerID)
	if err != nil {
		return optimization.State{}, err
	}
	var measurements []optimization.Measurement
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return optimization.State{}, err
		}
		if member.PeerID == repo.Config.PeerID {
			continue
		}
		measurement := measurePeer(ctx, repo, member, progress)
		measurements = append(measurements, measurement)
	}
	if err := ctx.Err(); err != nil {
		return optimization.State{}, err
	}
	state := optimization.State{Version: optimization.Version, PeerID: repo.Config.PeerID, OptimizedAt: time.Now().UTC(),
		MembershipFingerprint: optimization.Fingerprint(members), Measurements: measurements}
	state.Interactive = rankSources(measurements, false)
	state.Bulk = rankSources(measurements, true)
	if err := optimization.Save(repo.Config.Repository, state); err != nil {
		return optimization.State{}, fmt.Errorf("persist DFS optimization result: %w", err)
	}
	return state, nil
}

func OptimizeCluster(ctx context.Context, repo *repository.Repository, progress func(OptimizationProgress)) (ClusterOptimization, error) {
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		return ClusterOptimization{}, err
	}
	members, err := optimization.CurrentMembers(repo.Config.Repository, filesystemID, repo.Config.PeerID)
	if err != nil {
		return ClusterOptimization{}, err
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].PeerName != members[j].PeerName {
			return members[i].PeerName < members[j].PeerName
		}
		return members[i].PeerID < members[j].PeerID
	})
	result := ClusterOptimization{OptimizedAt: time.Now().UTC()}
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if progress != nil {
			progress(OptimizationProgress{PeerID: member.PeerID, PeerName: member.PeerName, Stage: "peer"})
		}
		var state optimization.State
		var optimizeErr error
		if member.PeerID == repo.Config.PeerID {
			state, optimizeErr = OptimizeLocal(ctx, repo, progress)
		} else {
			state, optimizeErr = OptimizeRemote(ctx, repo, member.PeerID)
		}
		outcome := PeerOptimization{PeerID: member.PeerID, PeerName: member.PeerName, State: state}
		if optimizeErr != nil {
			outcome.Error = conciseOptimizationError(optimizeErr)
		}
		result.Peers = append(result.Peers, outcome)
	}
	return result, nil
}

func OptimizeRemote(ctx context.Context, repo *repository.Repository, peerID string) (optimization.State, error) {
	connection, stream, _, response, err := Open(ctx, repo, peerID, Request{Operation: "optimize"})
	if err != nil {
		return optimization.State{}, err
	}
	defer connection.CloseWithError(0, "")
	defer stream.Close()
	var state optimization.State
	if err := json.Unmarshal(response.Payload, &state); err != nil {
		return optimization.State{}, fmt.Errorf("decode remote optimization result: %w", err)
	}
	if state.Version != optimization.Version || state.PeerID != peerID {
		return optimization.State{}, errors.New("remote returned invalid optimization state")
	}
	return state, nil
}

func measurePeer(ctx context.Context, repo *repository.Repository, member optimization.Member, progress func(OptimizationProgress)) optimization.Measurement {
	measurement := optimization.Measurement{PeerID: member.PeerID, PeerName: member.PeerName, Status: "OFFLINE"}
	var latencies, ttfbs, interactiveRates, bulkRates []float64
	for sample := 1; sample <= optimizationSamples; sample++ {
		if progress != nil {
			progress(OptimizationProgress{PeerID: member.PeerID, PeerName: member.PeerName, Stage: "interactive", Sample: sample, Samples: optimizationSamples})
		}
		requestCtx, cancel := context.WithTimeout(ctx, benchmarkTimeout)
		interactive, interactiveErr := benchmark(requestCtx, repo, member.PeerID, int64(sample*7919), interactiveBytes)
		cancel()
		if interactiveErr != nil {
			measurement.Failures++
			measurement.LastError = conciseOptimizationError(interactiveErr)
			continue
		}
		latencies = append(latencies, interactive.latencyMS)
		ttfbs = append(ttfbs, interactive.ttfbMS)
		interactiveRates = append(interactiveRates, interactive.mbps)
		if progress != nil {
			progress(OptimizationProgress{PeerID: member.PeerID, PeerName: member.PeerName, Stage: "bulk", Sample: sample, Samples: optimizationSamples})
		}
		requestCtx, cancel = context.WithTimeout(ctx, benchmarkTimeout)
		bulk, bulkErr := benchmark(requestCtx, repo, member.PeerID, int64(sample*104729), bulkBytes)
		cancel()
		if bulkErr != nil {
			measurement.Failures++
			measurement.LastError = conciseOptimizationError(bulkErr)
			continue
		}
		bulkRates = append(bulkRates, bulk.mbps)
		measurement.Samples++
	}
	if len(interactiveRates) == 0 && len(bulkRates) == 0 {
		return measurement
	}
	measurement.Status = "MEASURED"
	if measurement.Failures > 0 || len(interactiveRates) != optimizationSamples || len(bulkRates) != optimizationSamples {
		measurement.Status = "DEGRADED"
	}
	measurement.LatencyMedianMS = median(latencies)
	measurement.LatencyP95MS = percentile(latencies, 0.95)
	measurement.TTFBMedianMS = median(ttfbs)
	measurement.TTFBP95MS = percentile(ttfbs, 0.95)
	measurement.InteractiveMedianMbps = median(interactiveRates)
	measurement.InteractiveP10Mbps = percentile(interactiveRates, 0.10)
	measurement.BulkMedianMbps = median(bulkRates)
	measurement.BulkP10Mbps = percentile(bulkRates, 0.10)
	return measurement
}

func benchmark(ctx context.Context, repo *repository.Repository, peerID string, offset, length int64) (benchmarkResult, error) {
	started := time.Now()
	connection, stream, reader, response, err := Open(ctx, repo, peerID, Request{Operation: "benchmark", Offset: offset, Length: length})
	if err != nil {
		return benchmarkResult{}, err
	}
	defer connection.CloseWithError(0, "")
	defer stream.Close()
	opened := time.Now()
	if response.Size != length || length <= 0 {
		return benchmarkResult{}, errors.New("invalid benchmark response")
	}
	first, err := reader.ReadByte()
	if err != nil {
		return benchmarkResult{}, err
	}
	if first != benchmarkByte(offset) {
		return benchmarkResult{}, errors.New("benchmark payload validation failed")
	}
	firstAt := time.Now()
	if err := validateBenchmark(reader, offset+1, length-1); err != nil {
		return benchmarkResult{}, err
	}
	completed := time.Now()
	duration := completed.Sub(started).Seconds()
	if duration <= 0 {
		duration = math.SmallestNonzeroFloat64
	}
	return benchmarkResult{latencyMS: float64(opened.Sub(started).Microseconds()) / 1000,
		ttfbMS: float64(firstAt.Sub(started).Microseconds()) / 1000,
		mbps:   float64(length*8) / duration / 1_000_000}, nil
}

func serveBenchmark(stream *quic.Stream, offset, length int64) {
	if offset < 0 || length <= 0 || length > benchmarkMaxBytes {
		writeResponse(stream, Response{Error: "invalid benchmark range"})
		return
	}
	if err := writeResponse(stream, Response{OK: true, Size: length}); err != nil {
		return
	}
	buffer := make([]byte, 64<<10)
	for written := int64(0); written < length; {
		chunk := int64(len(buffer))
		if remaining := length - written; remaining < chunk {
			chunk = remaining
		}
		for index := int64(0); index < chunk; index++ {
			buffer[index] = benchmarkByte(offset + written + index)
		}
		if _, err := stream.Write(buffer[:chunk]); err != nil {
			return
		}
		written += chunk
	}
}

func validateBenchmark(reader *bufio.Reader, offset, length int64) error {
	buffer := make([]byte, 64<<10)
	for read := int64(0); read < length; {
		chunk := int64(len(buffer))
		if remaining := length - read; remaining < chunk {
			chunk = remaining
		}
		if _, err := io.ReadFull(reader, buffer[:chunk]); err != nil {
			return err
		}
		for index := int64(0); index < chunk; index++ {
			if buffer[index] != benchmarkByte(offset+read+index) {
				return errors.New("benchmark payload validation failed")
			}
		}
		read += chunk
	}
	return nil
}

func benchmarkByte(position int64) byte {
	return byte((position*31 + 17) % 251)
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 0 {
		return (ordered[middle-1] + ordered[middle]) / 2
	}
	return ordered[middle]
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	index := int(math.Ceil(quantile*float64(len(ordered)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}

func rankSources(measurements []optimization.Measurement, bulk bool) []optimization.RankedSource {
	ordered := append([]optimization.Measurement(nil), measurements...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		leftClass, rightClass := measurementClass(left.Status), measurementClass(right.Status)
		if leftClass != rightClass {
			return leftClass < rightClass
		}
		if leftClass < 2 {
			if bulk && left.BulkMedianMbps != right.BulkMedianMbps {
				return left.BulkMedianMbps > right.BulkMedianMbps
			}
			if !bulk && left.TTFBMedianMS != right.TTFBMedianMS {
				return left.TTFBMedianMS < right.TTFBMedianMS
			}
			if !bulk && left.InteractiveMedianMbps != right.InteractiveMedianMbps {
				return left.InteractiveMedianMbps > right.InteractiveMedianMbps
			}
		}
		return left.PeerID < right.PeerID
	})
	result := make([]optimization.RankedSource, 0, len(ordered))
	for _, item := range ordered {
		result = append(result, optimization.RankedSource{PeerID: item.PeerID, PeerName: item.PeerName, Status: item.Status})
	}
	return result
}

func measurementClass(status string) int {
	switch status {
	case "MEASURED":
		return 0
	case "DEGRADED":
		return 1
	default:
		return 2
	}
}

func conciseOptimizationError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 200 {
		message = message[:197] + "..."
	}
	return message
}
