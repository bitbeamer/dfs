package mount

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
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
	if err := prepareMountpoint(mountpoint); err != nil {
		logger.Error("creating mountpoint failed", "mountpoint", mountpoint, "error", err)
		return err
	}
	scheduler := syncer.New(repo, repo.Config.SyncInterval, logger.With("component", "sync"))
	scheduler.Start()
	defer scheduler.Stop()

	filesystem := NewFileSystem(repo, scheduler, logger.With("component", "filesystem"))
	pathNodes := pathfs.NewPathNodeFs(filesystem, &pathfs.PathNodeFsOptions{ClientInodes: true})
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
		if err := server.Unmount(); err != nil {
			logger.Error("automatic unmount failed",
				"mountpoint", mountpoint,
				"error", err,
				"recovery", fmt.Sprintf("run dfs unmount %s from another terminal", mountpoint),
			)
			<-serveDone
			return fmt.Errorf("unmount DFS at %s during shutdown: %w", mountpoint, err)
		}
		<-serveDone
	case <-serveDone:
	}
	logger.Info("mount stopped", "mountpoint", mountpoint)
	return nil
}

func prepareMountpoint(mountpoint string) error {
	info, err := os.Stat(mountpoint)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("mountpoint %s exists but is not a directory", mountpoint)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("mountpoint %s is inaccessible: %w; unmount any stale DFS/FUSE mount before retrying", mountpoint, err)
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("create mountpoint %s: %w", mountpoint, err)
	}
	return nil
}

func Unmount(mountpoint string) error {
	var command string
	var args []string
	if runtime.GOOS == "darwin" {
		command, args = "umount", []string{mountpoint}
	} else {
		command, args = "fusermount3", []string{"-u", mountpoint}
	}
	output, err := exec.Command(command, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unmount %s: %s", mountpoint, string(output))
	}
	return nil
}
