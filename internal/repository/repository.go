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
	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/store"
	"golang.org/x/sys/unix"
)

const RelayRemote = "dfs-relay"

const (
	remoteProbeTimeout = 5 * time.Second
	remoteRetryDelay   = time.Minute
	remoteSyncTimeout  = 20 * time.Second
	remoteSyncAttempts = 2
)

type Repository struct {
	Config              config.Config
	Store               *store.Store
	runner              command.Runner
	mu                  sync.Mutex
	syncStateMu         sync.Mutex
	remoteRetry         map[string]remoteRetry
	remoteChecked       map[string]bool
	managedFetcher      func(context.Context, *Repository, string, string) error
	managedRangeFetcher ManagedRangeFetcher
	rangeStatesMu       sync.Mutex
	rangeStates         map[string]*rangeState
}

type remoteRetry struct {
	until time.Time
	err   string
}

type RemoteSyncFailure struct {
	Remote string
	Err    error
}

// RemoteSyncError reports peers that could not be synchronized after every
// reachable remote was still processed. Callers responsible for background
// maintenance may treat it as a degraded result rather than a failed local
// transaction.
type RemoteSyncError struct {
	Failures []RemoteSyncFailure
}

func (e *RemoteSyncError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, failure := range e.Failures {
		parts = append(parts, failure.Remote+": "+failure.Err.Error())
	}
	return "unavailable DFS remotes: " + strings.Join(parts, "; ")
}

type Remote struct {
	Name string
	URL  string
}

type CachedFile struct {
	Path string
	Size int64
}

type GitIdentity struct {
	Name  string
	Email string
}

func CheckDependencies() error {
	var missing []string
	for _, name := range []string{"git", "git-annex"} {
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
	return InitWithIdentity(ctx, path, name, cacheLimit, GitIdentity{})
}

func InitWithIdentity(ctx context.Context, path, name string, cacheLimit int64, identity GitIdentity) (*Repository, error) {
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
	if err := configureGitIdentity(ctx, runner, identity); err != nil {
		return nil, err
	}
	if _, err := runner.Run(ctx, "git", "annex", "init", name); err != nil {
		return nil, err
	}
	if _, err := runner.Run(ctx, "git", "config", "annex.largefiles", "anything"); err != nil {
		return nil, err
	}
	cfg := config.Default(name, path)
	if cacheLimit > 0 {
		cfg.CacheLimit = cacheLimit
	}
	if _, err := runner.Run(ctx, "git", "commit", "--allow-empty", "-m", "Initialize DFS repository"); err != nil {
		return nil, err
	}
	filesystemID, err := rootFileSystemID(ctx, runner)
	if err != nil {
		return nil, err
	}
	cfg.FileSystemID = filesystemID
	if err := config.Save(cfg); err != nil {
		return nil, err
	}
	return Open(path)
}

func Join(ctx context.Context, remote, path, name string, cacheLimit int64, expectedFileSystemID ...string) (*Repository, error) {
	return JoinWithIdentity(ctx, remote, path, name, cacheLimit, GitIdentity{}, expectedFileSystemID...)
}

func JoinWithIdentity(ctx context.Context, remote, path, name string, cacheLimit int64, identity GitIdentity, expectedFileSystemID ...string) (*Repository, error) {
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
	if err := configureGitIdentity(ctx, runner, identity); err != nil {
		return nil, err
	}
	// A pairing bundle can be captured while git-annex has its synchronized
	// branch checked out. DFS mutations and the directional fast path always
	// publish refs/heads/main, so normalize every joined worktree back to that
	// stable primary branch before initializing this peer.
	if _, mainErr := runner.Run(ctx, "git", "rev-parse", "--verify", "refs/heads/main"); mainErr == nil {
		if _, err := runner.Run(ctx, "git", "checkout", "main"); err != nil {
			return nil, err
		}
	} else {
		if _, err := runner.Run(ctx, "git", "rev-parse", "--verify", "refs/heads/synced/main"); err != nil {
			return nil, errors.New("joined DFS repository has neither main nor synced/main")
		}
		if _, err := runner.Run(ctx, "git", "checkout", "-b", "main", "refs/heads/synced/main"); err != nil {
			return nil, err
		}
	}
	if _, err := runner.Run(ctx, "git", "annex", "init", name); err != nil {
		return nil, err
	}
	cfg := config.Default(name, path)
	if len(expectedFileSystemID) > 0 && strings.TrimSpace(expectedFileSystemID[0]) != "" {
		cfg.FileSystemID = strings.TrimSpace(expectedFileSystemID[0])
	} else {
		filesystemID, err := rootFileSystemID(ctx, runner)
		if err != nil {
			return nil, err
		}
		cfg.FileSystemID = filesystemID
	}
	if cacheLimit > 0 {
		cfg.CacheLimit = cacheLimit
	}
	if err := config.Save(cfg); err != nil {
		return nil, err
	}
	return Open(path)
}

func configureGitIdentity(ctx context.Context, runner command.Runner, identity GitIdentity) error {
	identity.Name = strings.TrimSpace(identity.Name)
	identity.Email = strings.TrimSpace(identity.Email)
	if identity.Name == "" && identity.Email == "" {
		return nil
	}
	if identity.Name == "" || identity.Email == "" {
		return errors.New("Git author name and email must both be provided")
	}
	if _, err := runner.Run(ctx, "git", "config", "user.name", identity.Name); err != nil {
		return fmt.Errorf("configure repository Git author name: %w", err)
	}
	if _, err := runner.Run(ctx, "git", "config", "user.email", identity.Email); err != nil {
		return fmt.Errorf("configure repository Git author email: %w", err)
	}
	return nil
}

func Open(path string) (*Repository, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	state, err := store.Open(filepath.Join(cfg.Repository, filepath.FromSlash(config.Directory), "state.db"))
	if err != nil {
		return nil, err
	}
	repo := &Repository{Config: cfg, Store: state, runner: command.Runner{Directory: cfg.Repository}, remoteRetry: make(map[string]remoteRetry), remoteChecked: make(map[string]bool)}
	if _, err := repo.FileSystemID(context.Background()); err != nil {
		_ = state.Close()
		return nil, err
	}
	if err := repo.migrateLegacyTransport(context.Background()); err != nil {
		_ = state.Close()
		return nil, err
	}
	return repo, nil
}

func (r *Repository) Close() error { return r.Store.Close() }

// migrateLegacyTransport removes settings and credentials created by DFS
// versions that predate the authenticated managed transport. Unrelated user
// configuration and remotes are left untouched.
func (r *Repository) migrateLegacyTransport(ctx context.Context) error {
	for _, key := range []string{"core.sshCommand", "annex.ssh-options"} {
		_, _ = r.runner.Run(ctx, "git", "config", "--unset-all", key)
	}
	if output, err := r.runner.Run(ctx, "git", "config", "--name-only", "--get-regexp", `^remote\..*\.dfs-ssh-url$`); err == nil {
		for _, key := range strings.Fields(output) {
			_, _ = r.runner.Run(ctx, "git", "config", "--unset-all", key)
		}
	}
	stateDirectory := filepath.Join(r.Config.Repository, filepath.FromSlash(config.Directory))
	for _, name := range []string{"peer-ssh-key", "peer-ssh-key.pub", "known_hosts"} {
		if err := os.Remove(filepath.Join(stateDirectory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove legacy DFS transport credential %s: %w", name, err)
		}
	}
	return nil
}

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

// FileSystemID returns the stable identity shared by every clone of this DFS
// repository. New repositories persist the initial commit ID in private
// configuration so later Git history maintenance cannot change the identity.
func (r *Repository) FileSystemID(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(r.Config.FileSystemID) != "" {
		return r.Config.FileSystemID, nil
	}
	filesystemID, err := rootFileSystemID(ctx, r.runner)
	if err != nil {
		return "", err
	}
	r.Config.FileSystemID = filesystemID
	if err := r.SaveConfig(); err != nil {
		return "", fmt.Errorf("persist DFS filesystem identity: %w", err)
	}
	return filesystemID, nil
}

func rootFileSystemID(ctx context.Context, runner command.Runner) (string, error) {
	out, err := runner.Run(ctx, "git", "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		return "", fmt.Errorf("determine DFS filesystem identity: %w", err)
	}
	var roots []string
	for _, root := range strings.Fields(out) {
		if root != "" {
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		return "", errors.New("DFS repository has no root commit")
	}
	sort.Strings(roots)
	return roots[0], nil
}

func (r *Repository) SetNetworkName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("network name cannot be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Config.NetworkName = name
	return r.SaveConfig()
}

// WithWorkTreeLock runs fn while repository operations that may replace paths
// in the worktree are excluded. The callback must not call another Repository
// method, because those methods acquire the same lock.
func (r *Repository) WithWorkTreeLock(fn func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := r.lockWorkTreeProcess(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func (r *Repository) CommitPending(ctx context.Context, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := r.lockWorkTreeProcess(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	return r.commitPendingLocked(ctx, message)
}

func (r *Repository) lockWorkTreeProcess(ctx context.Context) (func(), error) {
	if strings.TrimSpace(r.Config.Repository) == "" {
		return func() {}, nil
	}
	path := filepath.Join(r.Config.Repository, filepath.FromSlash(config.Directory), "worktree.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// RepairLegacyPrivateState removes runtime files that older DFS versions kept
// in the worktree from Git's index. If they are the only merge conflicts left
// by git-annex sync, it also completes that cleanup merge.
func (r *Repository) RepairLegacyPrivateState(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.repairLegacyPrivateStateLocked(ctx)
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
	if err := r.repairLegacyPrivateStateLocked(ctx); err != nil {
		return false, err
	}
	// git-annex handles new and modified user files. Git then records deletions,
	// renames, pointer updates, and ordinary control files.
	// git-annex normally skips dotfiles and files below dot-directories. The
	// subsequent git add must never turn those user files into ordinary Git
	// blobs, so override that default for every DFS commit.
	if _, err := r.runner.Run(ctx, "git", "-c", "annex.dotfiles=true", "annex", "add", "."); err != nil {
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

var legacyPrivatePaths = []string{
	".dfs/config.json",
	".dfs/mount.lock",
	".dfs/state.db",
	".dfs/state.db-shm",
	".dfs/state.db-wal",
	".dfs/recovery",
	".dfs/staging",
	".dfs/transactions",
}

func legacyPrivatePath(path string) bool {
	path = filepath.ToSlash(path)
	for _, private := range legacyPrivatePaths {
		if path == private || strings.HasPrefix(path, private+"/") {
			return true
		}
	}
	return false
}

func (r *Repository) repairLegacyPrivateStateLocked(ctx context.Context) error {
	unmerged, err := r.runner.Run(ctx, "git", "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return err
	}
	var conflictPaths []string
	for _, path := range strings.Split(unmerged, "\x00") {
		if path != "" {
			conflictPaths = append(conflictPaths, path)
		}
	}
	args := []string{"rm", "-r", "-f", "--cached", "--ignore-unmatch", "--"}
	args = append(args, legacyPrivatePaths...)
	if _, err := r.runner.Run(ctx, "git", args...); err != nil {
		return fmt.Errorf("remove legacy DFS state from Git: %w", err)
	}
	if len(conflictPaths) == 0 {
		return nil
	}
	for _, path := range conflictPaths {
		if !legacyPrivatePath(path) {
			return nil
		}
	}
	if _, err := os.Stat(filepath.Join(r.Config.Repository, ".git", "MERGE_HEAD")); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if _, err := r.runner.Run(ctx, "git", "commit", "-m", "Remove DFS private state from shared history"); err != nil {
		return fmt.Errorf("complete legacy DFS state cleanup merge: %w", err)
	}
	return nil
}

func (r *Repository) Sync(ctx context.Context, metadataOnly bool) error {
	return r.SyncDirectional(ctx, metadataOnly, true, true)
}

// ApplyReceived merges refs already delivered by a peer into the checked-out
// worktree. It deliberately performs no network I/O: the peer's receive-pack
// has completed, and unrelated or offline peers must not delay visibility.
func (r *Repository) ApplyReceived(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := r.lockWorkTreeProcess(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	refs, err := r.runner.Run(ctx, "git", "for-each-ref", "--format=%(refname)", "refs/heads/dfs-incoming", "refs/heads/synced/main")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(refs) {
		if _, err := r.runner.Run(ctx, "git", "-c", "merge.renames=false", "merge", "--no-edit", ref); err != nil {
			return err
		}
	}
	_, err = r.runner.Run(ctx, "git", "-c", "merge.renames=false", "annex", "sync", "--no-content", "--no-pull", "--no-push")
	return err
}

// SyncDirectional exchanges metadata in only the directions needed by an
// event-driven pass. Periodic synchronization should use Sync so it performs
// a full bidirectional convergence pass.
func (r *Repository) SyncDirectional(ctx context.Context, metadataOnly, pull, push bool) error {
	if err := config.ValidateHostname(r.Config); err != nil {
		return err
	}
	r.mu.Lock()
	processUnlock, lockErr := r.lockWorkTreeProcess(ctx)
	if lockErr != nil {
		r.mu.Unlock()
		return lockErr
	}
	if _, err := r.commitPendingLocked(ctx, "Synchronize local changes"); err != nil {
		processUnlock()
		r.mu.Unlock()
		return err
	}
	remotes, err := r.remotesLocked(ctx)
	processUnlock()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	// A DFS move is committed as an add/delete pair. Disable Git's heuristic
	// rename pairing during merges so concurrent operations on different paths
	// with identical content cannot be cross-paired and lose a valid version.
	args := []string{"-c", "merge.renames=false", "annex", "sync"}
	if metadataOnly {
		args = append(args, "--no-content")
	}
	if !pull {
		args = append(args, "--no-pull")
	}
	if !push {
		args = append(args, "--no-push")
	}
	if len(remotes) == 0 {
		r.mu.Lock()
		unlock, lockErr := r.lockWorkTreeProcess(ctx)
		if lockErr != nil {
			r.mu.Unlock()
			return lockErr
		}
		_, err := r.runner.Run(ctx, "git", args...)
		unlock()
		r.mu.Unlock()
		return err
	}

	// Probe remotes concurrently without holding the worktree lock. A dead peer
	// must not prevent a healthy peer from exchanging metadata, nor may its
	// network timeout block FUSE hydration and writes that need the same lock.
	type probeResult struct {
		remote   Remote
		err      error
		deferred bool
	}
	results := make(chan probeResult, len(remotes))
	now := time.Now()
	for _, remote := range remotes {
		retry, waiting := r.remoteRetryState(remote.Name, now)
		// Full reconciliation honors the unavailable-peer backoff. Directional
		// event delivery deliberately does not: pushes run concurrently, so a
		// dead peer cannot delay a healthy one, and a peer that recovered from a
		// transient network failure receives the very next filesystem event.
		if waiting && pull && push {
			results <- probeResult{remote: remote, err: fmt.Errorf("retry after %s following: %s", retry.until.Format(time.RFC3339), retry.err), deferred: true}
			continue
		}
		// Startup and periodic passes probe every remote to establish and refresh
		// availability. Latency-critical directional passes reuse that state: the
		// Git exchange itself is already a connectivity check, so a preceding
		// ls-remote would add a redundant network round trip in each direction.
		if (!pull || !push) && r.remoteWasChecked(remote.Name) {
			results <- probeResult{remote: remote}
			continue
		}
		go func(remote Remote) {
			probeCtx, cancel := context.WithTimeout(ctx, remoteProbeTimeout)
			defer cancel()
			_, probeErr := r.runner.Run(probeCtx, "git", "ls-remote", "--heads", remote.Name)
			results <- probeResult{remote: remote, err: probeErr}
		}(remote)
	}

	var failures []RemoteSyncFailure
	var available []Remote
	for range remotes {
		result := <-results
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if result.err != nil {
			if !result.deferred {
				r.recordRemoteFailure(result.remote.Name, result.err)
			}
			failures = append(failures, RemoteSyncFailure{Remote: result.remote.Name, Err: result.err})
			continue
		}
		r.clearRemoteFailure(result.remote.Name)
		available = append(available, result.remote)
	}

	// Local filesystem events only need to publish the committed worktree. Give
	// every sender its own inbox ref on each receiver and push those refs in
	// parallel. This avoids both shared-ref races and git-annex sync's per-peer
	// bookkeeping on the latency-critical path. Periodic passes reconcile the
	// full annex metadata graph.
	if !pull && push && len(available) > 0 {
		pushResults := make(chan RemoteSyncFailure, len(available))
		inbox := fmt.Sprintf("refs/heads/dfs-incoming/%s/main", r.Config.PeerID)
		for _, remote := range available {
			go func(remote Remote) {
				var pushErr error
				for attempt := 0; attempt < remoteSyncAttempts; attempt++ {
					pushCtx, cancel := context.WithTimeout(ctx, remoteSyncTimeout)
					_, pushErr = r.runner.Run(pushCtx, "git", "push", "--porcelain", remote.Name, "refs/heads/main:"+inbox)
					cancel()
					if pushErr == nil {
						break
					}
				}
				pushResults <- RemoteSyncFailure{Remote: remote.Name, Err: pushErr}
			}(remote)
		}
		for range available {
			result := <-pushResults
			if result.Err != nil {
				failures = append(failures, result)
			}
		}
	} else {
		for _, remote := range available {
			remoteArgs := append(append([]string(nil), args...), remote.Name)
			var syncErr error
			for attempt := 0; attempt < remoteSyncAttempts; attempt++ {
				syncCtx, cancel := context.WithTimeout(ctx, remoteSyncTimeout)
				r.mu.Lock()
				unlock, lockErr := r.lockWorkTreeProcess(syncCtx)
				if lockErr != nil {
					r.mu.Unlock()
					cancel()
					syncErr = lockErr
					break
				}
				_, syncErr = r.runner.Run(syncCtx, "git", remoteArgs...)
				unlock()
				r.mu.Unlock()
				cancel()
				if syncErr == nil {
					break
				}
			}
			if syncErr == nil {
				continue
			}
			// The probe established that this peer is reachable. A concurrent
			// bidirectional sync can still cause a transient Git lock or push
			// race, so let the convergence loop retry it instead of imposing the
			// unavailable-peer backoff.
			failures = append(failures, RemoteSyncFailure{Remote: remote.Name, Err: syncErr})
		}
	}
	if len(failures) > 0 {
		sort.Slice(failures, func(i, j int) bool { return failures[i].Remote < failures[j].Remote })
		return &RemoteSyncError{Failures: failures}
	}
	return nil
}

func (r *Repository) remoteRetryState(name string, now time.Time) (remoteRetry, bool) {
	r.syncStateMu.Lock()
	defer r.syncStateMu.Unlock()
	retry, found := r.remoteRetry[name]
	return retry, found && now.Before(retry.until)
}

func (r *Repository) recordRemoteFailure(name string, err error) {
	r.syncStateMu.Lock()
	defer r.syncStateMu.Unlock()
	if r.remoteRetry == nil {
		r.remoteRetry = make(map[string]remoteRetry)
	}
	if r.remoteChecked == nil {
		r.remoteChecked = make(map[string]bool)
	}
	r.remoteChecked[name] = true
	r.remoteRetry[name] = remoteRetry{until: time.Now().Add(remoteRetryDelay), err: err.Error()}
}

func (r *Repository) clearRemoteFailure(name string) {
	r.syncStateMu.Lock()
	defer r.syncStateMu.Unlock()
	if r.remoteChecked == nil {
		r.remoteChecked = make(map[string]bool)
	}
	r.remoteChecked[name] = true
	delete(r.remoteRetry, name)
}

func (r *Repository) remoteWasChecked(name string) bool {
	r.syncStateMu.Lock()
	defer r.syncStateMu.Unlock()
	return r.remoteChecked[name]
}

// TreeID returns the current worktree snapshot recorded by HEAD. Comparing
// tree IDs ignores git-annex's metadata-only sync commits while still
// detecting user-visible path and content changes.
func (r *Repository) TreeID(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, err := r.runner.Run(ctx, "git", "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// ChangedPaths returns every path whose directory entry differs between two
// trees. Rename detection is disabled so both the removed and added names are
// invalidated in FUSE clients.
func (r *Repository) ChangedPaths(ctx context.Context, before, after string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, err := r.runner.Run(ctx, "git", "diff", "--name-only", "--no-renames", "-z", before, after)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(value, "\x00") {
		if path != "" {
			paths = append(paths, filepath.ToSlash(path))
		}
	}
	return paths, nil
}

func (r *Repository) Fetch(ctx context.Context, path, from string) error {
	key, _ := r.LookupKey(ctx, path)
	r.mu.Lock()
	fetcher := r.managedFetcher
	r.mu.Unlock()
	if fetcher != nil {
		if err := fetcher(ctx, r, path, from); err == nil {
			r.discardRangeCache(key)
			return r.Store.Touch(path)
		}
	}
	r.mu.Lock()
	args := []string{"annex", "get"}
	if from != "" {
		args = append(args, "--from="+from)
	}
	args = append(args, "--", filepath.ToSlash(path))
	if _, err := r.runner.Run(ctx, "git", args...); err != nil {
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	r.discardRangeCache(key)
	return r.Store.Touch(path)
}

func (r *Repository) SetManagedFetcher(fetcher func(context.Context, *Repository, string, string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.managedFetcher = fetcher
}

func (r *Repository) LookupKey(ctx context.Context, path string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, err := r.runner.Run(ctx, "git", "annex", "lookupkey", "--", filepath.ToSlash(path))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (r *Repository) ReinjectContent(ctx context.Context, source, destination string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.runner.Run(ctx, "git", "annex", "reinject", source, filepath.ToSlash(destination))
	return err
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
	if err := r.Store.Pin(path); err != nil {
		return err
	}
	if err := r.Fetch(ctx, path, ""); err != nil {
		return fmt.Errorf("pin saved; initial hydration failed: %w", err)
	}
	return nil
}

func (r *Repository) Unpin(path string) error { return r.Store.Unpin(path) }

func (r *Repository) SetLocalPin(path string) error { return r.Store.Pin(path) }

func (r *Repository) SetClusterPin(ctx context.Context, path string, pinned bool) error {
	filesystemID, err := r.FileSystemID(ctx)
	if err != nil {
		return err
	}
	policy, err := membership.SetPinPolicy(r.Config.Repository, filesystemID, r.Config.PeerID, path, pinned)
	if err != nil {
		return err
	}
	return r.Store.SetClusterPinned(policy.Path, pinned)
}

func (r *Repository) AddRemote(ctx context.Context, name, url string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" || url == "" {
		return errors.New("remote name and URL are required")
	}
	_, err := r.runner.Run(ctx, "git", "remote", "add", name, url)
	return err
}

// AddPairedRemote creates or refreshes the deterministic remote used for an
// authenticated DFS peer. Peer IDs, rather than display names, prevent name
// collisions and make a renamed device retain the same remote.
func (r *Repository) AddPairedRemote(ctx context.Context, peerID, url string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	peerID = strings.ToLower(strings.TrimSpace(peerID))
	url = strings.TrimSpace(url)
	if peerID == "" || url == "" {
		return "", errors.New("paired peer ID and URL are required")
	}
	for _, value := range peerID {
		if (value < 'a' || value > 'z') && (value < '0' || value > '9') && value != '-' {
			return "", fmt.Errorf("invalid paired peer ID %q", peerID)
		}
	}
	shortID := peerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	name := "dfs-peer-" + shortID
	remotes, err := r.remotesLocked(ctx)
	if err != nil {
		return "", err
	}
	for _, remote := range remotes {
		if remote.Name != name {
			continue
		}
		if remote.URL == url {
			return name, nil
		}
		if _, err := r.runner.Run(ctx, "git", "remote", "set-url", name, url); err != nil {
			return "", err
		}
		return name, nil
	}
	if _, err := r.runner.Run(ctx, "git", "remote", "add", name, url); err != nil {
		return "", err
	}
	return name, nil
}

func (r *Repository) AddManagedRemote(ctx context.Context, peerID, executable string) (string, error) {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	escape := func(value string) string {
		value = strings.ReplaceAll(value, "%", "%%")
		return strings.ReplaceAll(value, " ", "% ")
	}
	managedURL := "ext::" + escape(executable) + " --repo " + escape(r.Config.Repository) + " transport git " + escape(peerID) + " %S"
	name, err := r.AddPairedRemote(ctx, peerID, managedURL)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.runner.Run(ctx, "git", "config", "protocol.ext.allow", "always"); err != nil {
		return "", err
	}
	if _, err := r.runner.Run(ctx, "git", "config", "remote."+name+".dfs-transport", "quic"); err != nil {
		return "", err
	}
	return name, nil
}

func (r *Repository) AdoptClonedPeer(ctx context.Context, peerID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	peerID = strings.ToLower(strings.TrimSpace(peerID))
	if peerID == "" {
		return "", errors.New("paired peer ID is required")
	}
	shortID := peerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	name := "dfs-peer-" + shortID
	remotes, err := r.remotesLocked(ctx)
	if err != nil {
		return "", err
	}
	for _, remote := range remotes {
		if remote.Name == name {
			return name, nil
		}
	}
	for _, remote := range remotes {
		if remote.Name == "origin" {
			if _, err := r.runner.Run(ctx, "git", "remote", "rename", "origin", name); err != nil {
				return "", err
			}
			return name, nil
		}
	}
	return "", errors.New("cloned DFS repository has no origin remote")
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

// ProbeRemote verifies that a configured Git remote can serve repository
// metadata. It is deliberately read-only and does not fetch or update refs.
func (r *Repository) ProbeRemote(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("remote name is required")
	}
	_, err := r.runner.Run(ctx, "git", "ls-remote", "--heads", name)
	return err
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
	var annexBytes int64
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
			annexBytes += info.Size()
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return r.rangeCacheUsage()
	}
	if err != nil {
		return 0, err
	}
	rangeBytes, err := r.rangeCacheUsage()
	return annexBytes + rangeBytes, err
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
	// Sparse ranges and hydrated annex objects share one configured cache
	// budget. Reclaim inactive ranges first, then drop whole annex objects if
	// the combined cache is still above its limit.
	if err := r.pruneRangeCache("", 0); err != nil {
		return nil, err
	}
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
