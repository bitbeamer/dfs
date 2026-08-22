package repository

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SyncPlan describes what a synchronization pass would do, without changing
// anything. Eviction candidates assume inactive range-cache extents are
// reclaimed first and remain subject to git-annex copy-safety checks.
type SyncPlan struct {
	Remotes    []Remote            `json:"remotes"`
	Pins       []PinnedPathHealth  `json:"pins,omitempty"`
	CacheBytes int64               `json:"cache_bytes"`
	CacheLimit int64               `json:"cache_limit_bytes"`
	Evictions  []EvictionCandidate `json:"evictions,omitempty"`
}

// EvictionCandidate is one unpinned cached file that cache enforcement would
// try to evict, least recently used first.
type EvictionCandidate struct {
	Path string `json:"path"`
	Size int64  `json:"size_bytes"`
}

// SyncPlan computes the synchronization plan read-only: the remotes metadata
// would be exchanged with, the hydration state of every pinned path, and the
// LRU eviction candidates needed to bring the cache within its limit.
func (r *Repository) SyncPlan(ctx context.Context, metadataOnly bool) (*SyncPlan, error) {
	remotes, err := r.Remotes(ctx)
	if err != nil {
		return nil, err
	}
	plan := &SyncPlan{Remotes: remotes}
	if metadataOnly {
		return plan, nil
	}
	pinRecords, err := r.Store.PinRecords()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	all, err := r.annexHealthFilesLocked(ctx)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	local, err := r.annexHealthFilesHereLocked(ctx)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	tree, err := r.runner.Run(ctx, "git", "ls-tree", "-r", "-z", "--format=%(objecttype)%x09%(objectsize)%x09%(path)", "HEAD")
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	usage, err := r.cacheUsageLocked()
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	annexed := make(map[string]annexHealthFile, len(all))
	fileSizes := make(map[string]int64, len(all))
	for _, file := range all {
		annexed[file.Path] = file
		fileSizes[file.Path] = file.Size
	}
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		fields := strings.SplitN(entry, "\t", 3)
		if len(fields) != 3 || fields[0] != "blob" {
			continue
		}
		path := filepath.ToSlash(fields[2])
		if _, found := annexed[path]; found {
			continue
		}
		if size, parseErr := strconv.ParseInt(fields[1], 10, 64); parseErr == nil {
			fileSizes[path] = size
		}
	}
	plan.Pins = pinnedPathHealth(pinRecords, fileSizes, local, annexed)
	plan.CacheBytes = usage
	plan.CacheLimit = r.Config.CacheLimit
	if usage <= r.Config.CacheLimit {
		return plan, nil
	}
	files, err := r.CachedFiles(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return r.Store.LastAccess(files[i].Path).Before(r.Store.LastAccess(files[j].Path))
	})
	remaining := usage
	for _, file := range files {
		if remaining <= r.Config.CacheLimit {
			break
		}
		pinned, err := r.Store.IsPinned(file.Path)
		if err != nil {
			return nil, err
		}
		if pinned {
			continue
		}
		plan.Evictions = append(plan.Evictions, EvictionCandidate{Path: file.Path, Size: file.Size})
		remaining -= file.Size
	}
	return plan, nil
}
