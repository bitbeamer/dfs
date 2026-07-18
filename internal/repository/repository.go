package repository

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitbeamer/dfs/internal/command"
	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/store"
)

const RelayRemote = "dfs-relay"

type Repository struct {
	Config config.Config
	Store  *store.Store
	runner command.Runner
	mu     sync.Mutex
}

type Remote struct {
	Name string
	URL  string
}

type CachedFile struct {
	Path string
	Size int64
}

func CheckDependencies() error {
	var missing []string
	for _, name := range []string{"git", "git-annex", "ssh", "rsync"} {
		if !command.Exists(name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required commands: %s", strings.Join(missing, ", "))
	}
	return nil
}

func Init(ctx context.Context, path, name string, cacheLimit int64) (*Repository, error) {
	if err := CheckDependencies(); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if name == "" {
		host, _ := os.Hostname()
		name = host
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	runner := command.Runner{Directory: path}
	if _, err := runner.Run(ctx, "git", "init", "-b", "main"); err != nil {
		return nil, err
	}
	if _, err := runner.Run(ctx, "git", "annex", "init", name); err != nil {
		return nil, err
	}
	if _, err := runner.Run(ctx, "git", "config", "annex.largefiles", "anything"); err != nil {
		return nil, err
	}
	ignore := ".dfs/\n.DS_Store\n"
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte(ignore), 0o644); err != nil {
		return nil, err
	}
	cfg := config.Default(name, path)
	if cacheLimit > 0 {
		cfg.CacheLimit = cacheLimit
	}
	if err := config.Save(cfg); err != nil {
		return nil, err
	}
	if _, err := runner.Run(ctx, "git", "add", ".gitignore"); err != nil {
		return nil, err
	}
	if _, err := runner.Run(ctx, "git", "commit", "-m", "Initialize DFS repository"); err != nil {
		return nil, err
	}
	return Open(path)
}

func Join(ctx context.Context, remote, path, name string, cacheLimit int64) (*Repository, error) {
	if err := CheckDependencies(); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	runner := command.Runner{Directory: parent}
	if _, err := runner.Run(ctx, "git", "clone", remote, path); err != nil {
		return nil, err
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	runner.Directory = path
	if _, err := runner.Run(ctx, "git", "annex", "init", name); err != nil {
		return nil, err
	}
	cfg := config.Default(name, path)
	if cacheLimit > 0 {
		cfg.CacheLimit = cacheLimit
	}
	if err := config.Save(cfg); err != nil {
		return nil, err
	}
	return Open(path)
}

func Open(path string) (*Repository, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	state, err := store.Open(filepath.Join(cfg.Repository, config.Directory, "state.db"))
	if err != nil {
		return nil, err
	}
	return &Repository{Config: cfg, Store: state, runner: command.Runner{Directory: cfg.Repository}}, nil
}

func (r *Repository) Close() error { return r.Store.Close() }

// SetLogger enables diagnostic logging for commands run on behalf of this
// repository. Call it before starting concurrent repository operations.
func (r *Repository) SetLogger(logger *slog.Logger) {
	if logger == nil {
		r.runner.Logger = nil
		return
	}
	r.runner.Logger = logger.With("component", "command")
}

func (r *Repository) SaveConfig() error { return config.Save(r.Config) }

// WithWorkTreeLock runs fn while repository operations that may replace paths
// in the worktree are excluded. The callback must not call another Repository
// method, because those methods acquire the same lock.
func (r *Repository) WithWorkTreeLock(fn func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn()
}

func (r *Repository) CommitPending(ctx context.Context, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commitPendingLocked(ctx, message)
}

// CheckConsistency validates Git object connectivity and lets git-annex repair
// inexpensive metadata inconsistencies without hashing all stored content.
func (r *Repository) CheckConsistency(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.runner.Run(ctx, "git", "fsck", "--no-dangling"); err != nil {
		return err
	}
	if _, err := r.runner.Run(ctx, "git", "annex", "fsck", "--fast"); err != nil {
		return err
	}
	return nil
}

func (r *Repository) commitPendingLocked(ctx context.Context, message string) (bool, error) {
	// git-annex handles new and modified user files. Git then records deletions,
	// renames, pointer updates, and ordinary control files.
	if _, err := r.runner.Run(ctx, "git", "annex", "add", "."); err != nil {
		return false, err
	}
	if _, err := r.runner.Run(ctx, "git", "add", "-A"); err != nil {
		return false, err
	}
	status, err := r.runner.Run(ctx, "git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil
	}
	if message == "" {
		message = "Update files"
	}
	if _, err := r.runner.Run(ctx, "git", "commit", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Repository) Sync(ctx context.Context, metadataOnly bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.commitPendingLocked(ctx, "Synchronize local changes"); err != nil {
		return err
	}
	// A DFS move is committed as an add/delete pair. Disable Git's heuristic
	// rename pairing during merges so concurrent operations on different paths
	// with identical content cannot be cross-paired and lose a valid version.
	args := []string{"-c", "merge.renames=false", "annex", "sync"}
	if metadataOnly {
		args = append(args, "--no-content")
	}
	_, err := r.runner.Run(ctx, "git", args...)
	return err
}

func (r *Repository) Fetch(ctx context.Context, path, from string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	args := []string{"annex", "get"}
	if from != "" {
		args = append(args, "--from="+from)
	}
	args = append(args, "--", filepath.ToSlash(path))
	if _, err := r.runner.Run(ctx, "git", args...); err != nil {
		return err
	}
	return r.Store.Touch(path)
}

func (r *Repository) Unlock(ctx context.Context, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.runner.Run(ctx, "git", "annex", "unlock", "--", filepath.ToSlash(path))
	return err
}

func (r *Repository) Evict(ctx context.Context, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pinned, err := r.Store.IsPinned(path)
	if err != nil {
		return err
	}
	if pinned {
		return fmt.Errorf("%s is pinned; unpin it before eviction", path)
	}
	_, err = r.runner.Run(ctx, "git", "annex", "drop", "--", filepath.ToSlash(path))
	return err
}

func (r *Repository) Pin(ctx context.Context, path string) error {
	if err := r.Fetch(ctx, path, ""); err != nil {
		return err
	}
	return r.Store.Pin(path)
}

func (r *Repository) Unpin(path string) error { return r.Store.Unpin(path) }

func (r *Repository) AddRemote(ctx context.Context, name, url string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" || url == "" {
		return errors.New("remote name and URL are required")
	}
	_, err := r.runner.Run(ctx, "git", "remote", "add", name, url)
	return err
}

func (r *Repository) SetRelay(ctx context.Context, url string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	remotes, err := r.remotesLocked(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, remote := range remotes {
		if remote.Name == RelayRemote {
			found = true
			break
		}
	}
	if found {
		if _, err := r.runner.Run(ctx, "git", "remote", "set-url", RelayRemote, url); err != nil {
			return err
		}
	} else if _, err := r.runner.Run(ctx, "git", "remote", "add", RelayRemote, url); err != nil {
		return err
	}
	r.Config.Relay = url
	return r.SaveConfig()
}

func (r *Repository) RemoveRemote(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.runner.Run(ctx, "git", "remote", "remove", name)
	return err
}

func (r *Repository) Remotes(ctx context.Context) ([]Remote, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remotesLocked(ctx)
}

func (r *Repository) remotesLocked(ctx context.Context) ([]Remote, error) {
	out, err := r.runner.Run(ctx, "git", "remote", "-v")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var result []Remote
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || seen[fields[0]] {
			continue
		}
		seen[fields[0]] = true
		result = append(result, Remote{Name: fields[0], URL: fields[1]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (r *Repository) RemovePeer(ctx context.Context, name string) error {
	if name == RelayRemote {
		return errors.New("use relay configuration to remove the metadata relay")
	}
	return r.RemoveRemote(ctx, name)
}

func (r *Repository) InitS3(ctx context.Context, name, bucket, region, host, encryption string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" || bucket == "" {
		return errors.New("storage name and bucket are required")
	}
	if encryption == "" {
		encryption = "shared"
	}
	args := []string{"annex", "initremote", name, "type=S3", "bucket=" + bucket, "encryption=" + encryption}
	if region != "" {
		args = append(args, "region="+region)
	}
	if host != "" {
		args = append(args, "host="+host, "protocol=https")
	}
	_, err := r.runner.Run(ctx, "git", args...)
	return err
}

func (r *Repository) EnableStorage(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.runner.Run(ctx, "git", "annex", "enableremote", name)
	return err
}

func (r *Repository) CopyTo(ctx context.Context, name string, paths []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	args := []string{"annex", "copy", "--to=" + name, "--"}
	for _, path := range paths {
		args = append(args, filepath.ToSlash(path))
	}
	_, err := r.runner.Run(ctx, "git", args...)
	return err
}

func (r *Repository) Status(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status, err := r.runner.Run(ctx, "git", "status", "--short", "--branch")
	if err != nil {
		return "", err
	}
	usage, err := r.cacheUsageLocked()
	if err != nil {
		return "", err
	}
	remotes, err := r.remotesLocked(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Peer: %s\nRepository: %s\nCache: %s / %s\nRemotes: %d\n\n%s",
		r.Config.Name, r.Config.Repository, config.FormatSize(usage), config.FormatSize(r.Config.CacheLimit), len(remotes), status), nil
}

func (r *Repository) CacheUsage() (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cacheUsageLocked()
}

func (r *Repository) cacheUsageLocked() (int64, error) {
	root := filepath.Join(r.Config.Repository, ".git", "annex", "objects")
	var size int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
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

func (r *Repository) CachedFiles(ctx context.Context) ([]CachedFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out, err := r.runner.Run(ctx, "git", "annex", "find", "--in=here", "--format=${file}\t${bytesize}\n")
	if err != nil {
		return nil, err
	}
	var files []CachedFile
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		size, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		files = append(files, CachedFile{Path: parts[0], Size: size})
	}
	return files, scanner.Err()
}

func (r *Repository) Prune(ctx context.Context) ([]string, error) {
	usage, err := r.CacheUsage()
	if err != nil || usage <= r.Config.CacheLimit {
		return nil, err
	}
	files, err := r.CachedFiles(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return r.Store.LastAccess(files[i].Path).Before(r.Store.LastAccess(files[j].Path))
	})
	var dropped []string
	for _, file := range files {
		if usage <= r.Config.CacheLimit {
			break
		}
		pinned, err := r.Store.IsPinned(file.Path)
		if err != nil {
			return dropped, err
		}
		if pinned {
			continue
		}
		if err := r.Evict(ctx, file.Path); err != nil {
			continue // annex refuses unsafe drops; try another candidate.
		}
		usage, err = r.CacheUsage()
		if err != nil {
			return dropped, err
		}
		dropped = append(dropped, file.Path)
	}
	return dropped, nil
}

func (r *Repository) History(ctx context.Context, path string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	args := []string{"log", "--date=iso", "--pretty=format:%h %ad %s"}
	if path != "" {
		args = append(args, "--", filepath.ToSlash(path))
	}
	return r.runner.Run(ctx, "git", args...)
}

func (r *Repository) Restore(ctx context.Context, revision, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	args := []string{"restore", "--source", revision, "--"}
	if path == "" {
		args = append(args, ".")
	} else {
		args = append(args, filepath.ToSlash(path))
	}
	if _, err := r.runner.Run(ctx, "git", args...); err != nil {
		return err
	}
	if _, err := r.runner.Run(ctx, "git", "add", "-A"); err != nil {
		return err
	}
	message := "Restore " + revision
	if path != "" {
		message += " for " + filepath.ToSlash(path)
	}
	_, err := r.runner.Run(ctx, "git", "commit", "-m", message)
	return err
}

func (r *Repository) Conflicts(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out, err := r.runner.Run(ctx, "git", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			result = append(result, line)
		}
	}
	return result, nil
}

func (r *Repository) SetCacheLimit(limit int64) error {
	if limit <= 0 {
		return errors.New("cache limit must be greater than zero")
	}
	r.Config.CacheLimit = limit
	return r.SaveConfig()
}

func (r *Repository) AnnexFileSize(ctx context.Context, path string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, err := r.runner.Run(ctx, "git", "annex", "lookupkey", "--", filepath.ToSlash(path))
	if err != nil {
		return 0, err
	}
	key = strings.TrimSpace(key)
	start := strings.Index(key, "-s")
	if start < 0 {
		return 0, errors.New("annex key does not contain a size")
	}
	rest := key[start+2:]
	end := strings.Index(rest, "--")
	if end < 0 {
		return 0, errors.New("invalid annex key")
	}
	return strconv.ParseInt(rest[:end], 10, 64)
}

func (r *Repository) Touch(path string) { _ = r.Store.Touch(path) }

func DefaultContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Minute)
}
