package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	rangeDemandQuantum     = int64(256 << 10)
	rangeReadAhead         = int64(4 << 20)
	rangeSequentialMedium  = int64(1 << 20)
	durableFallbackMaxSize = int64(32 << 20)
	durableFallbackTimeout = 10 * time.Second
)

var ErrContentUnavailable = errors.New("annex content is unavailable")

const (
	AvailabilityNoTrustedPeers       = "no_trusted_peers"
	AvailabilityAcceptedPeersOffline = "accepted_peers_offline"
	AvailabilityKnownHoldersOffline  = "known_holders_offline"
	AvailabilityNoOnlineCopy         = "no_online_copy"
	AvailabilityTimeout              = "availability_timeout"
	AvailabilityTransferFailed       = "transfer_failed"
	AvailabilityDurableLimit         = "durable_limit"
	AvailabilityDurableUnavailable   = "durable_unavailable"
)

type ContentUnavailableError struct {
	Reason string
	Detail string
}

func (err *ContentUnavailableError) Error() string {
	if err.Detail == "" {
		return "annex content is unavailable: " + err.Reason
	}
	return "annex content is unavailable: " + err.Reason + ": " + err.Detail
}

func (err *ContentUnavailableError) Unwrap() error { return ErrContentUnavailable }

func ContentAvailabilityReason(err error) string {
	var unavailable *ContentUnavailableError
	if errors.As(err, &unavailable) {
		return unavailable.Reason
	}
	return ""
}

// ManagedRangeFetcher copies exactly length bytes at offset from a trusted
// peer and returns the peer's complete object size.
type ManagedRangeFetcher func(context.Context, *Repository, string, int64, int64, io.Writer) (int64, error)

type byteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type rangeMetadata struct {
	Key       string      `json:"key"`
	Size      int64       `json:"size"`
	Ranges    []byteRange `json:"ranges"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type rangeState struct {
	mu              sync.Mutex
	active          int
	initialized     bool
	size            int64
	cachePath       string
	metadataPath    string
	ranges          []byteRange
	flights         []*rangeFlight
	generation      uint64
	persisting      bool
	promotion       bool
	hasLastRead     bool
	lastReadEnd     int64
	sequentialReads int
}

type rangeFlight struct {
	extent byteRange
	done   chan struct{}
	err    error
}

func (r *Repository) SetManagedRangeFetcher(fetcher ManagedRangeFetcher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.managedRangeFetcher = fetcher
}

func (r *Repository) CanStreamRanges() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.managedRangeFetcher != nil
}

// ReadRange returns demanded annex bytes while retaining transferred extents
// in peer-private sparse storage. Foreground reads are capped to a small
// quantum; sequential read-ahead, durable extent publication, verification,
// and promotion are performed after demanded bytes are available.
func (r *Repository) ReadRange(ctx context.Context, path, key string, size, offset int64, destination []byte) (n int, returnErr error) {
	started := time.Now()
	finish := r.BeginContentRead(path, offset, len(destination))
	defer func() { finish(returnErr) }()
	if offset < 0 || size < 0 || offset > size {
		return 0, errors.New("invalid annex range")
	}
	if int64(len(destination)) > size-offset {
		destination = destination[:size-offset]
	}
	if len(destination) == 0 {
		return 0, nil
	}
	// A pin or another reader may have promoted the object since this handle
	// was opened. Prefer the verified annex object whenever it is available.
	if file, err := os.Open(filepath.Join(r.Config.Repository, filepath.FromSlash(path))); err == nil {
		defer file.Close()
		return file.ReadAt(destination, offset)
	}

	state := r.acquireRangeState(key)
	defer r.releaseRangeState(key, state)
	fetcher := r.rangeFetcher()
	if fetcher == nil {
		return 0, errors.New("managed annex range transport is unavailable")
	}
	if err := r.initializeRangeState(state, key, size); err != nil {
		return 0, err
	}

	requestedEnd := offset + int64(len(destination))
	windowStart := (offset / rangeDemandQuantum) * rangeDemandQuantum
	windowEnd := windowStart + rangeDemandQuantum
	if requestedEnd > windowEnd {
		if int64(len(destination)) <= rangeDemandQuantum {
			windowStart = offset
			windowEnd = offset + rangeDemandQuantum
		} else {
			windowStart = offset
			windowEnd = requestedEnd
		}
	}
	if windowEnd > size {
		windowEnd = size
	}
	cacheHit := r.rangeStateCovered(state, offset, requestedEnd)
	var ensureErr error
	if !cacheHit {
		ensureErr = r.ensureRange(ctx, state, path, key, size, windowStart, windowEnd, fetcher)
	}
	if ensureErr != nil {
		if !errors.Is(ensureErr, ErrContentUnavailable) {
			return 0, ensureErr
		}
		if size > durableFallbackMaxSize {
			return 0, &ContentUnavailableError{Reason: AvailabilityDurableLimit,
				Detail: fmt.Sprintf("peer plan failed (%v); %d-byte object exceeds the %d-byte bounded durable hydration limit",
					ensureErr, size, durableFallbackMaxSize)}
		}
		fallbackCtx, cancel := context.WithTimeout(ctx, durableFallbackTimeout)
		fallbackStarted := time.Now()
		r.RecordContentPlan("durable-full-hydration", "")
		fallbackErr := r.FetchFromDurableStorage(fallbackCtx, path)
		cancel()
		r.LogContentRead("durable content fallback", "path", path, "size", size,
			"duration", time.Since(fallbackStarted), "error", fallbackErr)
		if fallbackErr != nil {
			return 0, &ContentUnavailableError{Reason: AvailabilityDurableUnavailable,
				Detail: fmt.Sprintf("peer plan failed (%v); durable plan failed (%v)", ensureErr, fallbackErr)}
		}
		file, openErr := os.Open(filepath.Join(r.Config.Repository, filepath.FromSlash(path)))
		if openErr != nil {
			return 0, openErr
		}
		defer file.Close()
		n, readErr := file.ReadAt(destination, offset)
		r.LogContentRead("content read completed", "path", path, "offset", offset,
			"requested_bytes", len(destination), "returned_bytes", n, "source", "durable-storage",
			"duration", time.Since(started))
		return n, readErr
	}

	file, err := os.Open(state.cachePath)
	if err != nil {
		// Background promotion can atomically consume the partial between
		// ensureRange and this open. Prefer the newly installed annex object.
		file, err = os.Open(filepath.Join(r.Config.Repository, filepath.FromSlash(path)))
		if err != nil {
			return 0, err
		}
	}
	defer file.Close()
	n, readErr := file.ReadAt(destination, offset)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return n, readErr
	}
	r.Touch(path)
	readAhead := r.recordRangeRead(state, offset, requestedEnd)
	if readAhead > 0 && windowEnd < size {
		prefetchEnd := min(size, windowEnd+readAhead)
		r.rangeTasks.Add(1)
		go func() {
			defer r.rangeTasks.Done()
			r.prefetchRange(ctx, state, path, key, size, windowEnd, prefetchEnd, fetcher)
		}()
	}
	r.LogContentRead("content read completed", "path", path, "offset", offset,
		"requested_bytes", len(destination), "returned_bytes", n, "cache_hit", cacheHit,
		"foreground_window_bytes", windowEnd-windowStart, "read_ahead_bytes", readAhead,
		"duration", time.Since(started))
	return n, nil
}

func (r *Repository) initializeRangeState(state *rangeState, key string, size int64) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.initialized && state.size == size {
		return nil
	}
	cachePath, metadataPath := r.rangeCachePaths(key)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return err
	}
	metadata, err := loadRangeMetadata(metadataPath, key, size)
	if err != nil {
		_ = os.Remove(cachePath)
		_ = os.Remove(metadataPath)
		metadata = rangeMetadata{Key: key, Size: size}
	}
	file, err := os.OpenFile(cachePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	state.initialized = true
	state.size = size
	state.cachePath = cachePath
	state.metadataPath = metadataPath
	state.ranges = metadata.Ranges
	return nil
}

func (r *Repository) rangeStateCovered(state *rangeState, start, end int64) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return rangeCovered(state.ranges, start, end)
}

func (r *Repository) ensureRange(ctx context.Context, state *rangeState, path, key string, size, start, end int64, fetcher ManagedRangeFetcher) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state.mu.Lock()
		gaps := rangeGaps(state.ranges, start, end)
		if len(gaps) == 0 {
			state.mu.Unlock()
			return nil
		}
		missing := gaps[0]
		var existing *rangeFlight
		for _, flight := range state.flights {
			if flight.extent.Start < missing.End && missing.Start < flight.extent.End {
				existing = flight
				break
			}
		}
		if existing != nil {
			state.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-existing.done:
				if existing.err != nil {
					return existing.err
				}
				continue
			}
		}
		flight := &rangeFlight{extent: missing, done: make(chan struct{})}
		state.flights = append(state.flights, flight)
		state.mu.Unlock()

		fetchErr := r.fetchRangeExtent(ctx, state, key, size, missing, fetcher)
		state.mu.Lock()
		for index, candidate := range state.flights {
			if candidate == flight {
				state.flights = append(state.flights[:index], state.flights[index+1:]...)
				break
			}
		}
		flight.err = fetchErr
		if fetchErr == nil {
			state.ranges = mergeRanges(append(state.ranges, missing))
			state.generation++
		}
		close(flight.done)
		state.mu.Unlock()
		if fetchErr != nil {
			return fetchErr
		}
		r.scheduleRangePersistence(state, path, key, size)
	}
}

func (r *Repository) fetchRangeExtent(ctx context.Context, state *rangeState, key string, size int64, extent byteRange, fetcher ManagedRangeFetcher) error {
	if err := r.pruneRangeCache(key, extent.End-extent.Start); err != nil {
		return err
	}
	started := time.Now()
	var payload strings.Builder
	payload.Grow(int(extent.End - extent.Start))
	total, err := fetcher(ctx, r, key, extent.Start, extent.End-extent.Start, &payload)
	if err != nil {
		r.LogContentRead("content range fetch failed", "offset", extent.Start, "bytes", extent.End-extent.Start,
			"duration", time.Since(started), "error", err)
		return err
	}
	if total != size {
		return fmt.Errorf("annex object size changed: peer reported %d, expected %d", total, size)
	}
	if int64(payload.Len()) != extent.End-extent.Start {
		return io.ErrUnexpectedEOF
	}
	file, err := os.OpenFile(state.cachePath, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeStarted := time.Now()
	_, writeErr := file.WriteAt([]byte(payload.String()), extent.Start)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	r.LogContentRead("content range cached", "offset", extent.Start, "bytes", extent.End-extent.Start,
		"duration", time.Since(writeStarted))
	r.LogContentRead("content range fetched", "offset", extent.Start, "bytes", extent.End-extent.Start,
		"duration", time.Since(started))
	return nil
}

func (r *Repository) recordRangeRead(state *rangeState, offset, end int64) int64 {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.hasLastRead && offset == state.lastReadEnd {
		state.sequentialReads++
	} else {
		state.sequentialReads = 0
	}
	state.hasLastRead = true
	state.lastReadEnd = end
	switch {
	case state.sequentialReads >= 2:
		return rangeReadAhead
	case state.sequentialReads == 1:
		return rangeSequentialMedium
	default:
		return 0
	}
}

func (r *Repository) prefetchRange(ctx context.Context, state *rangeState, path, key string, size, start, end int64, fetcher ManagedRangeFetcher) {
	state = r.acquireRangeState(key)
	defer r.releaseRangeState(key, state)
	if err := r.ensureRange(ctx, state, path, key, size, start, end, fetcher); err != nil && !errors.Is(err, context.Canceled) {
		r.LogContentRead("content read-ahead failed", "path", path, "offset", start, "bytes", end-start, "error", err)
	}
}

func (r *Repository) scheduleRangePersistence(state *rangeState, path, key string, size int64) {
	state.mu.Lock()
	if state.persisting {
		state.mu.Unlock()
		return
	}
	state.persisting = true
	state.mu.Unlock()
	r.rangeTasks.Add(1)
	go func() {
		defer r.rangeTasks.Done()
		r.persistRangeState(state, path, key, size)
	}()
}

func (r *Repository) persistRangeState(state *rangeState, path, key string, size int64) {
	state = r.acquireRangeState(key)
	defer r.releaseRangeState(key, state)
	for {
		state.mu.Lock()
		generation := state.generation
		ranges := append([]byteRange(nil), state.ranges...)
		cachePath, metadataPath := state.cachePath, state.metadataPath
		state.mu.Unlock()

		persistStarted := time.Now()
		file, err := os.OpenFile(cachePath, os.O_RDWR, 0o600)
		if err == nil {
			err = file.Sync()
			_ = file.Close()
		}
		if err == nil {
			err = saveRangeMetadata(metadataPath, rangeMetadata{Key: key, Size: size, Ranges: ranges, UpdatedAt: time.Now().UTC()})
		}
		if err != nil {
			r.LogContentRead("persist range cache failed", "path", path, "error", err)
		} else {
			r.LogContentRead("range cache persisted", "path", path, "ranges", len(ranges),
				"duration", time.Since(persistStarted))
		}

		state.mu.Lock()
		if err == nil && generation == state.generation {
			state.persisting = false
			complete := rangeCovered(ranges, 0, size) && !state.promotion
			if complete {
				state.promotion = true
			}
			state.mu.Unlock()
			if complete {
				r.promoteRangeCache(state, path, key)
			}
			return
		}
		if err != nil {
			state.persisting = false
			state.mu.Unlock()
			return
		}
		state.mu.Unlock()
	}
}

func (r *Repository) promoteRangeCache(state *rangeState, path, key string) {
	started := time.Now()
	file, err := os.Open(state.cachePath)
	if err == nil {
		err = verifySHA256AnnexKey(file, key)
		_ = file.Close()
	}
	if err == nil {
		err = r.ReinjectContent(context.Background(), state.cachePath, path)
	}
	state.mu.Lock()
	state.promotion = false
	if err == nil {
		state.ranges = nil
		state.initialized = false
		_ = os.Remove(state.metadataPath)
	} else if strings.Contains(err.Error(), "SHA256 verification") {
		state.ranges = nil
		state.initialized = false
		_ = os.Remove(state.cachePath)
		_ = os.Remove(state.metadataPath)
	}
	state.mu.Unlock()
	r.LogContentRead("range cache promotion completed", "path", path, "duration", time.Since(started), "error", err)
}

func (r *Repository) rangeFetcher() ManagedRangeFetcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.managedRangeFetcher
}

func (r *Repository) acquireRangeState(key string) *rangeState {
	r.rangeStatesMu.Lock()
	defer r.rangeStatesMu.Unlock()
	if r.rangeStates == nil {
		r.rangeStates = make(map[string]*rangeState)
	}
	state := r.rangeStates[key]
	if state == nil {
		state = &rangeState{}
		r.rangeStates[key] = state
	}
	state.active++
	return state
}

func (r *Repository) releaseRangeState(key string, state *rangeState) {
	r.rangeStatesMu.Lock()
	defer r.rangeStatesMu.Unlock()
	state.active--
}

func (r *Repository) rangeCachePaths(key string) (string, string) {
	digest := sha256.Sum256([]byte(key))
	base := filepath.Join(r.Config.Repository, ".git", "dfs", "range-cache", hex.EncodeToString(digest[:]))
	return base + ".partial", base + ".json"
}

func (r *Repository) discardRangeCache(key string) {
	if key == "" {
		return
	}
	state := r.acquireRangeState(key)
	defer r.releaseRangeState(key, state)
	state.mu.Lock()
	defer state.mu.Unlock()
	partial, metadata := r.rangeCachePaths(key)
	_ = os.Remove(partial)
	_ = os.Remove(metadata)
	state.initialized = false
	state.ranges = nil
	state.generation++
}

func loadRangeMetadata(path, key string, size int64) (rangeMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rangeMetadata{Key: key, Size: size}, nil
		}
		return rangeMetadata{}, err
	}
	var metadata rangeMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return rangeMetadata{}, err
	}
	if metadata.Key != key || metadata.Size != size {
		return rangeMetadata{}, errors.New("stale annex range metadata")
	}
	metadata.Ranges = mergeRanges(metadata.Ranges)
	for _, extent := range metadata.Ranges {
		if extent.End > size {
			return rangeMetadata{}, errors.New("invalid annex range metadata")
		}
	}
	return metadata, nil
}

func saveRangeMetadata(path string, metadata rangeMetadata) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".range-metadata-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func mergeRanges(ranges []byteRange) []byteRange {
	valid := ranges[:0]
	for _, value := range ranges {
		if value.Start >= 0 && value.End > value.Start {
			valid = append(valid, value)
		}
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Start < valid[j].Start })
	merged := make([]byteRange, 0, len(valid))
	for _, value := range valid {
		if len(merged) == 0 || value.Start > merged[len(merged)-1].End {
			merged = append(merged, value)
		} else if value.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = value.End
		}
	}
	return merged
}

func rangeGaps(ranges []byteRange, start, end int64) []byteRange {
	position := start
	var gaps []byteRange
	for _, value := range mergeRanges(append([]byteRange(nil), ranges...)) {
		if value.End <= position || value.Start >= end {
			continue
		}
		if value.Start > position {
			gaps = append(gaps, byteRange{Start: position, End: min(value.Start, end)})
		}
		if value.End > position {
			position = value.End
		}
		if position >= end {
			break
		}
	}
	if position < end {
		gaps = append(gaps, byteRange{Start: position, End: end})
	}
	return gaps
}

func rangeCovered(ranges []byteRange, start, end int64) bool {
	return len(rangeGaps(ranges, start, end)) == 0
}

func verifySHA256AnnexKey(file *os.File, key string) error {
	prefix := strings.SplitN(key, "-", 2)[0]
	if prefix != "SHA256" && prefix != "SHA256E" {
		return nil // git-annex reinject performs the authoritative backend check.
	}
	separator := strings.Index(key, "--")
	if separator < 0 {
		return errors.New("invalid SHA256 annex key")
	}
	want := key[separator+2:]
	if extension := strings.IndexByte(want, '.'); extension >= 0 {
		want = want[:extension]
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), want) {
		return errors.New("downloaded annex content failed SHA256 verification")
	}
	return nil
}

type rangeCacheEntry struct {
	key      string
	metadata string
	partial  string
	bytes    int64
	updated  time.Time
}

func (r *Repository) rangeCacheUsage() (int64, error) {
	paths, err := filepath.Glob(filepath.Join(r.Config.Repository, ".git", "dfs", "range-cache", "*.json"))
	if err != nil {
		return 0, err
	}
	var used int64
	for _, metadataPath := range paths {
		data, readErr := os.ReadFile(metadataPath)
		if readErr != nil {
			continue
		}
		var metadata rangeMetadata
		if json.Unmarshal(data, &metadata) != nil {
			continue
		}
		for _, extent := range mergeRanges(metadata.Ranges) {
			used += extent.End - extent.Start
		}
	}
	return used, nil
}

// pruneRangeCache keeps partial content inside the peer's configured cache
// budget. In-flight objects are never removed; the oldest resumable partials
// are discarded first.
func (r *Repository) pruneRangeCache(currentKey string, incoming int64) error {
	directory := filepath.Join(r.Config.Repository, ".git", "dfs", "range-cache")
	annexUsed, err := directorySize(filepath.Join(r.Config.Repository, ".git", "annex", "objects"), nil)
	if err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.json"))
	if err != nil {
		return err
	}
	var entries []rangeCacheEntry
	used := annexUsed
	for _, metadataPath := range paths {
		data, readErr := os.ReadFile(metadataPath)
		if readErr != nil {
			continue
		}
		var metadata rangeMetadata
		if json.Unmarshal(data, &metadata) != nil {
			continue
		}
		var bytes int64
		for _, extent := range mergeRanges(metadata.Ranges) {
			bytes += extent.End - extent.Start
		}
		used += bytes
		entries = append(entries, rangeCacheEntry{key: metadata.Key, metadata: metadataPath,
			partial: strings.TrimSuffix(metadataPath, ".json") + ".partial", bytes: bytes, updated: metadata.UpdatedAt})
	}
	limit := r.Config.CacheLimit
	if limit <= 0 || used+incoming <= limit {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].updated.Before(entries[j].updated) })
	for _, entry := range entries {
		if used+incoming <= limit {
			break
		}
		if entry.key == currentKey || r.rangeStateActive(entry.key) {
			continue
		}
		_ = os.Remove(entry.partial)
		_ = os.Remove(entry.metadata)
		used -= entry.bytes
	}
	return nil
}

func (r *Repository) rangeStateActive(key string) bool {
	r.rangeStatesMu.Lock()
	defer r.rangeStatesMu.Unlock()
	return r.rangeStates[key] != nil && r.rangeStates[key].active > 0
}
