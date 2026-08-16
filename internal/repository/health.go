package repository

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bitbeamer/dfs/internal/store"
)

type HealthStats struct {
	LogicalFiles       int64              `json:"logical_files"`
	LogicalBytes       int64              `json:"logical_bytes"`
	ContentFiles       int64              `json:"content_files"`
	ContentBytes       int64              `json:"content_bytes"`
	PinnedPaths        int64              `json:"pinned_paths"`
	MissingPinnedFiles int64              `json:"missing_pinned_files"`
	CacheBytes         int64              `json:"cache_bytes"`
	CacheLimitBytes    int64              `json:"cache_limit_bytes"`
	RepositoryBytes    int64              `json:"repository_bytes"`
	MetadataBytes      int64              `json:"metadata_bytes"`
	PrivateStateBytes  int64              `json:"private_state_bytes"`
	DiskTotalBytes     int64              `json:"disk_total_bytes"`
	DiskAvailableBytes int64              `json:"disk_available_bytes"`
	Pinned             []PinnedPathHealth `json:"pinned,omitempty"`
}

type PinnedPathHealth struct {
	Path         string `json:"path"`
	Scope        string `json:"scope"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	LogicalFiles int64  `json:"logical_files"`
	LogicalBytes int64  `json:"logical_bytes"`
	MissingFiles int64  `json:"missing_files"`
	MissingBytes int64  `json:"missing_bytes"`
}

type annexHealthFile struct {
	Path string
	Size int64
}

// HealthStats reports namespace and disk use without hydrating annex content.
func (r *Repository) HealthStats(ctx context.Context) (HealthStats, error) {
	pins, err := r.Store.Pins()
	if err != nil {
		return HealthStats{}, err
	}
	pinRecords, err := r.Store.PinRecords()
	if err != nil {
		return HealthStats{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	all, err := r.annexHealthFilesLocked(ctx)
	if err != nil {
		return HealthStats{}, err
	}
	local, err := r.annexHealthFilesHereLocked(ctx)
	if err != nil {
		return HealthStats{}, err
	}
	tree, err := r.runner.Run(ctx, "git", "ls-tree", "-r", "-z", "--format=%(objecttype)%x09%(objectsize)%x09%(path)", "HEAD")
	if err != nil {
		return HealthStats{}, err
	}
	stats := HealthStats{PinnedPaths: int64(len(pinRecords)), CacheLimitBytes: r.Config.CacheLimit}
	annexed := make(map[string]annexHealthFile, len(all))
	fileSizes := make(map[string]int64, len(all))
	for _, file := range all {
		annexed[file.Path] = file
		fileSizes[file.Path] = file.Size
		stats.LogicalBytes += file.Size
		if pathMatchesAnyPin(file.Path, pins) {
			if _, found := local[file.Path]; !found {
				stats.MissingPinnedFiles++
			}
		}
	}
	for path := range local {
		stats.ContentFiles++
		stats.ContentBytes += local[path].Size
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
		stats.LogicalFiles++
		if _, found := annexed[path]; found {
			continue
		}
		if size, parseErr := strconv.ParseInt(fields[1], 10, 64); parseErr == nil {
			stats.LogicalBytes += size
			fileSizes[path] = size
		}
	}
	stats.Pinned = pinnedPathHealth(pinRecords, fileSizes, local, annexed)
	stats.CacheBytes, err = directorySize(filepath.Join(r.Config.Repository, ".git", "annex", "objects"), nil)
	if err != nil {
		return HealthStats{}, err
	}
	objects := filepath.Clean(filepath.Join(r.Config.Repository, ".git", "annex", "objects"))
	stats.MetadataBytes, err = directorySize(filepath.Join(r.Config.Repository, ".git"), func(path string, entry fs.DirEntry) bool {
		return entry.IsDir() && filepath.Clean(path) == objects
	})
	if err != nil {
		return HealthStats{}, err
	}
	stats.PrivateStateBytes, err = directorySize(filepath.Join(r.Config.Repository, ".git", "dfs"), nil)
	if err != nil {
		return HealthStats{}, err
	}
	worktreeBytes, err := directorySize(r.Config.Repository, func(path string, entry fs.DirEntry) bool {
		return entry.IsDir() && filepath.Base(path) == ".git" && filepath.Dir(path) == filepath.Clean(r.Config.Repository)
	})
	if err != nil {
		return HealthStats{}, err
	}
	stats.RepositoryBytes = worktreeBytes + stats.MetadataBytes + stats.CacheBytes
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(r.Config.Repository, &filesystem); err == nil {
		stats.DiskTotalBytes = int64(filesystem.Blocks) * int64(filesystem.Bsize)
		stats.DiskAvailableBytes = int64(filesystem.Bavail) * int64(filesystem.Bsize)
	}
	updatePinStatuses(stats.Pinned, stats.DiskAvailableBytes)
	return stats, nil
}

func updatePinStatuses(pins []PinnedPathHealth, diskAvailableBytes int64) {
	for index := range pins {
		pin := &pins[index]
		switch {
		case pin.MissingFiles == 0 && pin.Kind != "missing":
			pin.Status = "ready"
		case pin.MissingBytes > 0 && diskAvailableBytes > 0 && pin.MissingBytes > diskAvailableBytes:
			pin.Status = "capacity-constrained"
		default:
			pin.Status = "hydrating"
		}
	}
}

func pinnedPathHealth(pins []store.Pin, fileSizes map[string]int64, local map[string]annexHealthFile, annexed map[string]annexHealthFile) []PinnedPathHealth {
	result := make([]PinnedPathHealth, 0, len(pins))
	for _, original := range pins {
		pin := strings.Trim(filepath.ToSlash(original.Path), "/")
		entry := PinnedPathHealth{Path: pin, Scope: original.Scope, Kind: "directory"}
		if _, found := fileSizes[pin]; found {
			entry.Kind = "file"
		}
		for path, size := range fileSizes {
			if pin != "" && path != pin && !strings.HasPrefix(path, pin+"/") {
				continue
			}
			entry.LogicalFiles++
			entry.LogicalBytes += size
			if _, isAnnexed := annexed[path]; isAnnexed {
				if _, isLocal := local[path]; !isLocal {
					entry.MissingFiles++
					entry.MissingBytes += size
				}
			}
		}
		if entry.LogicalFiles == 0 {
			entry.Kind = "missing"
		}
		result = append(result, entry)
	}
	return result
}

func (r *Repository) annexHealthFilesLocked(ctx context.Context) ([]annexHealthFile, error) {
	// git-annex find otherwise defaults to content available in this repository,
	// which would make logical namespace size depend on each peer's cache.
	output, err := r.runner.Run(ctx, "git", "annex", "find", "--anything", "--json")
	if err != nil {
		return nil, err
	}
	return parseAnnexHealthFiles(output)
}

func (r *Repository) annexHealthFilesHereLocked(ctx context.Context) (map[string]annexHealthFile, error) {
	output, err := r.runner.Run(ctx, "git", "annex", "find", "--in=here", "--json")
	if err != nil {
		return nil, err
	}
	files, err := parseAnnexHealthFiles(output)
	result := make(map[string]annexHealthFile, len(files))
	for _, file := range files {
		result[file.Path] = file
	}
	return result, err
}

func parseAnnexHealthFiles(output string) ([]annexHealthFile, error) {
	type record struct {
		File     string `json:"file"`
		ByteSize string `json:"bytesize"`
	}
	var result []annexHealthFile
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var value record
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(value.ByteSize, 10, 64)
		if err != nil {
			return nil, err
		}
		result = append(result, annexHealthFile{Path: filepath.ToSlash(value.File), Size: size})
	}
	return result, scanner.Err()
}

func pathMatchesAnyPin(path string, pins []string) bool {
	path = strings.Trim(filepath.ToSlash(path), "/")
	for _, pin := range pins {
		pin = strings.Trim(filepath.ToSlash(pin), "/")
		if pin == "" || path == pin || strings.HasPrefix(path, pin+"/") {
			return true
		}
	}
	return false
}

func directorySize(root string, skip func(string, fs.DirEntry) bool) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if skip != nil && skip(path, entry) {
			return filepath.SkipDir
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return size, err
}
