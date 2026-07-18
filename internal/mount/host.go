package mount

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/syncer"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
)

type Options struct {
	Context   context.Context
	Logger    *slog.Logger
	FUSEDebug bool
}

type fuseLogWriter struct{ logger *slog.Logger }

type unmountServer interface {
	Unmount() error
}

type mountpointAccess struct {
	stat       func(string) (os.FileInfo, error)
	mkdirAll   func(string, os.FileMode) error
	clearStale func(string) error
}

func (w fuseLogWriter) Write(p []byte) (int, error) {
	message := strings.TrimSpace(string(p))
	if message != "" {
		w.logger.Debug("FUSE request", "detail", message)
	}
	return len(p), nil
}

func Run(repo *repository.Repository, mountpoint string, options Options) error {
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
	logger.Info("mount starting",
		"repository", repo.Config.Repository,
		"mountpoint", mountpoint,
		"sync_interval", repo.Config.SyncInterval,
		"cache_limit_bytes", repo.Config.CacheLimit,
		"fuse_debug", options.FUSEDebug,
	)
	clearedStale, err := prepareMountpoint(mountpoint)
	if err != nil {
		logger.Error("creating mountpoint failed", "mountpoint", mountpoint, "error", err)
		return err
	}
	if clearedStale {
		logger.Info("stale mountpoint detached", "mountpoint", mountpoint)
	}
	scheduler := syncer.New(repo, repo.Config.SyncInterval, logger.With("component", "sync"))
	scheduler.Start()
	defer scheduler.Stop()

	filesystem := NewFileSystem(repo, scheduler, logger.With("component", "filesystem"))
	// The annex working tree may replace a regular file with a symlink after a
	// transaction is committed. Let go-fuse own stable inode identities instead
	// of exposing those internal inode changes to applications.
	pathNodes := pathfs.NewPathNodeFs(filesystem, &pathfs.PathNodeFsOptions{ClientInodes: false})
	mountOptions := &fuse.MountOptions{
		FsName: "dfs", Name: "dfs", DisableXAttrs: false,
		Options: []string{"default_permissions"}, Debug: options.FUSEDebug,
	}
	if options.FUSEDebug {
		mountOptions.Logger = log.New(fuseLogWriter{logger.With("component", "fuse")}, "", 0)
	}
	nodeOptions := nodefs.NewOptions()
	nodeOptions.EntryTimeout = time.Second
	nodeOptions.AttrTimeout = time.Second
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
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested", "reason", ctx.Err())
		if err := unmountAndWait(server, serveDone); err != nil {
			logger.Error("automatic unmount failed",
				"mountpoint", mountpoint,
				"error", err,
				"recovery", fmt.Sprintf("run dfs unmount %s from another terminal", mountpoint),
			)
			return fmt.Errorf("unmount DFS at %s during shutdown: %w", mountpoint, err)
		}
	case <-serveDone:
	}
	logger.Info("mount stopped", "mountpoint", mountpoint)
	return nil
}

func unmountAndWait(server unmountServer, serveDone <-chan struct{}) error {
	if err := server.Unmount(); err != nil {
		// A failed unmount leaves Serve running. Do not wait for it here: the
		// caller must be able to return, restore normal signal handling, and
		// report recovery instructions instead of swallowing every later Ctrl-C.
		return err
	}
	<-serveDone
	return nil
}

func prepareMountpoint(mountpoint string) (bool, error) {
	return prepareMountpointWithAccess(mountpoint, mountpointAccess{
		stat:       os.Stat,
		mkdirAll:   os.MkdirAll,
		clearStale: clearStaleMount,
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

func clearStaleMount(mountpoint string) error {
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
