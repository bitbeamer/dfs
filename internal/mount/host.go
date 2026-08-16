package mount

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/core"
	"github.com/bitbeamer/dfs/internal/daemon"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/wakeup"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
)

type Options struct {
	Context             context.Context
	Logger              *slog.Logger
	FUSEDebug           bool
	RecoverStaleSession bool
	Signals             <-chan os.Signal
}

type daemonNotifier struct{ repository string }

func (n daemonNotifier) Notify(reason string) { _ = wakeup.Notify(n.repository, reason) }
func (daemonNotifier) BeginWrite()            {}
func (daemonNotifier) EndWrite()              {}

type fuseLogWriter struct{ logger *slog.Logger }

type unmountServer interface {
	Unmount() error
}

type mountpointAccess struct {
	stat       func(string) (os.FileInfo, error)
	mkdirAll   func(string, os.FileMode) error
	clearStale func(string) error
}

type nodeContentInvalidator struct {
	paths       entryNotifier
	directories directoryChangeNotifier
	logger      *slog.Logger
}

type entryNotifier interface {
	FileNotify(path string, off int64, length int64) fuse.Status
	EntryNotify(dir string, name string) fuse.Status
}

func (i nodeContentInvalidator) InvalidateContent(path string) {
	// This is discovered from getattr. Notify after that request returns to
	// avoid nesting a FUSE upcall, while retaining the vnode and its read
	// offset. Tail receives a content event without a replacement event.
	time.AfterFunc(10*time.Millisecond, func() {
		i.paths.FileNotify(path, 0, 0)
	})
}

func (i nodeContentInvalidator) InvalidateEntry(path string) {
	time.AfterFunc(10*time.Millisecond, func() {
		// Do not mutate pathfs's internal node tree here. RmChild races active
		// lookups on macOS and can leave a name lookup blocked indefinitely.
		// If the changed path's parent is not represented in that tree, walk up
		// to the nearest represented ancestor. Invalidating that child clears
		// negative kernel lookups for the whole unknown branch.
		status := i.invalidateEntry(path)
		if status != fuse.OK && i.logger != nil {
			i.logger.Warn("FUSE entry invalidation failed", "path", path, "status", status)
		}
		if i.directories != nil {
			i.directories.NotifyPath(path)
		}
	})
}

func (i nodeContentInvalidator) invalidateEntry(path string) fuse.Status {
	path = strings.Trim(filepath.ToSlash(path), "/")
	for path != "" {
		directory, name := filepath.Split(path)
		directory = strings.TrimSuffix(filepath.ToSlash(directory), "/")
		status := i.paths.EntryNotify(directory, name)
		if status != fuse.ENOENT || directory == "" {
			return status
		}
		path = directory
	}
	return fuse.OK
}

const normalUnmountGrace = time.Second

func (w fuseLogWriter) Write(p []byte) (int, error) {
	message := strings.TrimSpace(string(p))
	if message != "" {
		w.logger.Debug("FUSE request", "detail", message)
	}
	return len(p), nil
}

func Run(repo *repository.Repository, mountpoint string, options Options) (runErr error) {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	logger = logger.With("peer", repo.Config.Name)
	repo.SetLogger(logger)
	if _, err := daemon.CheckHealth(repo.Config.Repository); err != nil {
		return fmt.Errorf("DFS core daemon is not ready: %w", err)
	}
	logger.Info("mount starting",
		"repository", repo.Config.Repository,
		"mountpoint", mountpoint,
		"sync_interval", repo.Config.SyncInterval,
		"cache_limit_bytes", repo.Config.CacheLimit,
		"fuse_debug", options.FUSEDebug,
	)
	session, err := recoverStartup(ctx, repo, mountpoint, logger, options.RecoverStaleSession)
	if err != nil {
		logger.Error("startup recovery failed", "error", err)
		return fmt.Errorf("recover DFS repository before mount: %w", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			logger.Error("removing mount session record failed", "error", err)
		}
	}()
	clearedStale, err := prepareMountpoint(mountpoint)
	if err != nil {
		logger.Error("creating mountpoint failed", "mountpoint", mountpoint, "error", err)
		return err
	}
	if clearedStale {
		logger.Info("stale mountpoint detached", "mountpoint", mountpoint)
	}
	notifier := daemonNotifier{repository: repo.Config.Repository}

	operationCtx, cancelOperations := context.WithCancel(ctx)
	defer cancelOperations()
	coreService := core.New(repo, core.Options{ManagedContent: true})
	defer coreService.Close()
	filesystem := NewFileSystemWithContext(operationCtx, coreService, notifier, logger.With("component", "filesystem"))
	// The annex working tree may replace a regular file with a symlink after a
	// transaction is committed. Let go-fuse own stable inode identities instead
	// of exposing those internal inode changes to applications.
	pathNodes := pathfs.NewPathNodeFs(filesystem, &pathfs.PathNodeFsOptions{ClientInodes: false})
	directoryNotifier := newDirectoryChangeNotifier(mountpoint, logger.With("component", "desktop"))
	defer directoryNotifier.Close()
	invalidator := nodeContentInvalidator{
		paths:       pathNodes,
		directories: directoryNotifier,
		logger:      logger.With("component", "fuse"),
	}
	filesystem.cacheInvalidator = invalidator
	frontendEvents, err := wakeup.ListenFrontend(repo.Config.Repository)
	if err != nil {
		return fmt.Errorf("listen for core namespace events: %w", err)
	}
	defer frontendEvents.Close()
	go func() {
		for {
			path, receiveErr := frontendEvents.Receive()
			if receiveErr != nil {
				return
			}
			if path != "" {
				invalidator.InvalidateEntry(path)
			}
		}
	}()
	mountOptions := &fuse.MountOptions{
		FsName: "dfs", Name: "dfs", DisableXAttrs: false,
		Options: []string{"default_permissions"}, Debug: options.FUSEDebug,
		EnableLocks: true,
	}
	if options.FUSEDebug {
		mountOptions.Logger = log.New(fuseLogWriter{logger.With("component", "fuse")}, "", 0)
	}
	nodeOptions := nodefs.NewOptions()
	if runtime.GOOS == "darwin" {
		// Git and git-annex update the backing worktree outside FUSE requests.
		// macOS can otherwise retain a positive vnode indefinitely even after a
		// successful FUSE deletion notification, making removed names pass stat.
		nodeOptions.EntryTimeout = 0
		nodeOptions.AttrTimeout = 0
	} else {
		nodeOptions.EntryTimeout = time.Second
		nodeOptions.AttrTimeout = time.Second
	}
	server, _, err := nodefs.Mount(mountpoint, pathNodes.Root(), mountOptions, nodeOptions)
	if err != nil {
		logger.Error("mount failed", "mountpoint", mountpoint, "error", err)
		return fmt.Errorf("mount DFS at %s: %w; if the mountpoint is stale, run dfs unmount %s before retrying", mountpoint, err, mountpoint)
	}
	logger.Info("mount ready", "mountpoint", mountpoint)
	serveDone := make(chan struct{})
	go func() {
		server.Serve()
		close(serveDone)
	}()
	shutdown, shutdownReason := waitForMountStop(ctx, options.Signals, serveDone)
	// Cancel FUSE operations before asking the kernel to unmount. In particular,
	// an on-demand content fetch must not keep macFUSE teardown blocked after the
	// daemon has received SIGTERM.
	cancelOperations()
	if shutdown {
		logger.Info("shutdown requested", "reason", shutdownReason)
		if err := unmountAndWait(server, serveDone, func() error { return forceUnmount(mountpoint) }, normalUnmountGrace); err != nil {
			logger.Error("automatic unmount failed",
				"mountpoint", mountpoint,
				"error", err,
				"recovery", fmt.Sprintf("run dfs unmount %s from another terminal", mountpoint),
			)
			return fmt.Errorf("unmount DFS at %s during shutdown: %w", mountpoint, err)
		}
	}
	logger.Info("mount stopped", "mountpoint", mountpoint)
	return nil
}

func waitForMountStop(ctx context.Context, signals <-chan os.Signal, serveDone <-chan struct{}) (bool, any) {
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case received := <-signals:
		return true, received
	case <-serveDone:
		return false, nil
	}
}

func unmountAndWait(server unmountServer, serveDone <-chan struct{}, force func() error, grace time.Duration) error {
	normalDone := make(chan error, 1)
	go func() { normalDone <- server.Unmount() }()
	timer := time.NewTimer(grace)
	defer timer.Stop()

	var normalErr error
	select {
	case normalErr = <-normalDone:
		if normalErr == nil {
			<-serveDone
			return nil
		}
	case <-timer.C:
		normalErr = fmt.Errorf("did not finish within %s", grace)
	}

	// A shell or another process may have its working directory inside the
	// mount. Detach it so existing references can drain without blocking DFS
	// shutdown. If that also fails, return without waiting for Serve forever.
	if forceErr := force(); forceErr != nil {
		// The normal attempt may have won a race with the forced detach.
		select {
		case err := <-normalDone:
			if err == nil {
				<-serveDone
				return nil
			}
		default:
		}
		select {
		case <-serveDone:
			return nil
		default:
		}
		return fmt.Errorf("normal unmount failed: %v; forced detach failed: %w", normalErr, forceErr)
	}
	// A lazy/forced detach deliberately allows existing references to outlive
	// the mount namespace entry. Serve may therefore remain active until this
	// process exits; waiting for it would recreate the EBUSY shutdown hang.
	return nil
}

func prepareMountpoint(mountpoint string) (bool, error) {
	return prepareMountpointWithAccess(mountpoint, mountpointAccess{
		stat:       os.Stat,
		mkdirAll:   os.MkdirAll,
		clearStale: forceUnmount,
	})
}

func prepareMountpointWithAccess(mountpoint string, access mountpointAccess) (bool, error) {
	info, err := access.stat(mountpoint)
	clearedStale := false
	if isStaleMountError(err) {
		if cleanupErr := access.clearStale(mountpoint); cleanupErr != nil {
			return false, fmt.Errorf("detach stale mountpoint %s: %w", mountpoint, cleanupErr)
		}
		clearedStale = true
		info, err = access.stat(mountpoint)
	}
	if err == nil {
		if !info.IsDir() {
			return clearedStale, fmt.Errorf("mountpoint %s exists but is not a directory", mountpoint)
		}
		return clearedStale, nil
	}
	if !os.IsNotExist(err) {
		return clearedStale, fmt.Errorf("mountpoint %s is inaccessible: %w; unmount any stale DFS/FUSE mount before retrying", mountpoint, err)
	}
	if err := access.mkdirAll(mountpoint, 0o755); err != nil {
		return clearedStale, fmt.Errorf("create mountpoint %s: %w", mountpoint, err)
	}
	return clearedStale, nil
}

func isStaleMountError(err error) bool {
	return isStaleMountErrorForOS(err, runtime.GOOS)
}

func isStaleMountErrorForOS(err error, goos string) bool {
	if errors.Is(err, syscall.ENOTCONN) {
		return true
	}
	// A disconnected macFUSE endpoint is surfaced by stat as ENXIO
	// ("Device not configured") rather than Linux's ENOTCONN.
	return goos == "darwin" && errors.Is(err, syscall.ENXIO)
}

func Unmount(mountpoint string) error {
	return runUnmount(mountpoint, false)
}

func forceUnmount(mountpoint string) error {
	return runUnmount(mountpoint, true)
}

func runUnmount(mountpoint string, stale bool) error {
	var command string
	var args []string
	if runtime.GOOS == "darwin" {
		args = []string{mountpoint}
		if stale {
			args = append([]string{"-f"}, args...)
		}
		command = "umount"
	} else {
		option := "-u"
		if stale {
			option = "-uz"
		}
		command, args = "fusermount3", []string{option, mountpoint}
	}
	output, err := exec.Command(command, args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("unmount %s: %s", mountpoint, detail)
	}
	return nil
}
