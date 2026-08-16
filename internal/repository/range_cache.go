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

const rangeReadAhead = int64(4 << 20)

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
	mu     sync.Mutex
	active int
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

// ReadRange returns an annex object's requested bytes immediately, retaining
// successfully transferred extents in peer-private sparse storage. The cache
// is shared by every visible path for the same key and survives daemon restarts.
func (r *Repository) ReadRange(ctx context.Context, path, key string, size, offset int64, destination []byte) (int, error) {
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
	state.mu.Lock()
	defer state.mu.Unlock()

	fetcher := r.rangeFetcher()
	if fetcher == nil {
		return 0, errors.New("managed annex range transport is unavailable")
	}
	cachePath, metadataPath := r.rangeCachePaths(key)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return 0, err
	}
	metadata, err := loadRangeMetadata(metadataPath, key, size)
	if err != nil {
		_ = os.Remove(cachePath)
		_ = os.Remove(metadataPath)
		metadata = rangeMetadata{Key: key, Size: size}
	}
	file, err := os.OpenFile(cachePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if err := file.Truncate(size); err != nil {
		return 0, err
	}

	requestedEnd := offset + int64(len(destination))
	windowStart := (offset / rangeReadAhead) * rangeReadAhead
	windowEnd := ((requestedEnd + rangeReadAhead - 1) / rangeReadAhead) * rangeReadAhead
	if windowEnd > size {
		windowEnd = size
	}
	for _, missing := range rangeGaps(metadata.Ranges, windowStart, windowEnd) {
		if err := r.pruneRangeCache(key, missing.End-missing.Start); err != nil {
			return 0, err
		}
		writer := io.NewOffsetWriter(file, missing.Start)
		total, fetchErr := fetcher(ctx, r, key, missing.Start, missing.End-missing.Start, writer)
		if fetchErr != nil {
			return 0, fetchErr
		}
		if total != size {
			return 0, fmt.Errorf("annex object size changed: peer reported %d, expected %d", total, size)
		}
		// Never publish an extent in resumable metadata before its bytes are
		// durable. After a crash an unrecorded extent is safely downloaded again.
		if err := file.Sync(); err != nil {
			return 0, err
		}
		metadata.Ranges = mergeRanges(append(metadata.Ranges, missing))
		metadata.UpdatedAt = time.Now().UTC()
		if err := saveRangeMetadata(metadataPath, metadata); err != nil {
			return 0, err
		}
	}

	if rangeCovered(metadata.Ranges, 0, size) {
		if err := verifySHA256AnnexKey(file, key); err != nil {
			_ = file.Close()
			_ = os.Remove(cachePath)
			_ = os.Remove(metadataPath)
			return 0, err
		}
	}
	n, readErr := file.ReadAt(destination, offset)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return n, readErr
	}
	if rangeCovered(metadata.Ranges, 0, size) {
		if err := file.Sync(); err != nil {
			return 0, err
		}
		if err := file.Close(); err != nil {
			return 0, err
		}
		// git-annex verifies the key again and atomically installs the object.
		if err := r.ReinjectContent(ctx, cachePath, path); err != nil {
			return 0, fmt.Errorf("promote verified annex content: %w", err)
		}
		_ = os.Remove(metadataPath)
	}
	r.Touch(path)
	return n, nil
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
	if state.active == 0 {
		delete(r.rangeStates, key)
	}
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

// pruneRangeCache keeps partial content inside the peer's configured cache
// budget. In-flight objects are never removed; the oldest resumable partials
// are discarded first.
func (r *Repository) pruneRangeCache(currentKey string, incoming int64) error {
	directory := filepath.Join(r.Config.Repository, ".git", "dfs", "range-cache")
	paths, err := filepath.Glob(filepath.Join(directory, "*.json"))
	if err != nil {
		return err
	}
	var entries []rangeCacheEntry
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
