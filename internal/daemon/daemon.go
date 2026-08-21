package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/peer"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/syncer"
	"github.com/bitbeamer/dfs/internal/wakeup"
	"golang.org/x/sys/unix"
)

type Options struct {
	Context          context.Context
	Logger           *slog.Logger
	Signals          <-chan os.Signal
	PairingPort      int
	Mountpoint       string
	DisableDiscovery bool
}

type frontendInvalidator struct{ repository string }

func (i frontendInvalidator) InvalidateEntry(path string) {
	_ = wakeup.NotifyFrontend(i.repository, path)
}

func Run(repo *repository.Repository, options Options) (runErr error) {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	logger = logger.With("peer", repo.Config.Name, "repository", repo.Config.Repository)
	repo.SetLogger(logger)
	unlock, err := lock(repo.Config.Repository)
	if err != nil {
		return err
	}
	defer unlock()
	health := newHealthReporter(repo.Config.Name, repo.Config.Repository, options.Mountpoint, logger)
	health.update("starting", false, nil)
	notifySystemd("STATUS=Starting DFS core")
	defer func() {
		if runErr != nil {
			health.update("failed", false, runErr)
			notifySystemd("STATUS=DFS core failed: " + runErr.Error())
		}
	}()
	repo.SetManagedFetcher(managed.FetchPath)
	repo.SetManagedRangeFetcher(managed.FetchRange)
	repo.SetManagedCloser(func() { managed.CloseContentSessions(repo.Config.Repository) })
	scheduler := syncer.New(repo, repo.Config.SyncInterval, logger.With("component", "sync"))
	scheduler.SetReconciler(func(ctx context.Context) error { return peer.ReconcileMembership(ctx, repo) })
	scheduler.SetEntryInvalidator(frontendInvalidator{repository: repo.Config.Repository})
	eventListener, err := wakeup.Listen(repo.Config.Repository)
	if err != nil {
		return fmt.Errorf("listen for repository events: %w", err)
	}
	defer eventListener.Close()
	go func() {
		for {
			reason, receiveErr := eventListener.Receive()
			if receiveErr != nil {
				return
			}
			if reason != "" {
				scheduler.Notify(reason)
			}
		}
	}()
	peerService, err := peer.StartWithDiscovery(repo, logger, options.PairingPort, !options.DisableDiscovery, func(reason string, paths []string) {
		for _, path := range paths {
			_ = wakeup.NotifyFrontend(repo.Config.Repository, path)
		}
		scheduler.Notify(reason)
	})
	if err != nil {
		return fmt.Errorf("start authenticated peer service: %w", err)
	}
	defer peerService.Close()
	contentLiveness, err := managed.StartContentLivenessMonitor(ctx, repo)
	if err != nil {
		return fmt.Errorf("start local content liveness monitor: %w", err)
	}
	defer contentLiveness.Close()
	scheduler.Start()
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{}, 2)
	go func() { health.heartbeat(heartbeatStop); heartbeatDone <- struct{}{} }()
	go func() { health.observe(heartbeatStop, repo, repo.Config.SyncInterval); heartbeatDone <- struct{}{} }()
	health.update("ready", true, nil)
	notifySystemd("READY=1\nSTATUS=DFS core ready")
	logger.Info("core daemon ready", "port", options.PairingPort)
	select {
	case <-ctx.Done():
	case <-options.Signals:
	}
	health.update("stopping", false, nil)
	notifySystemd("STOPPING=1\nSTATUS=Stopping DFS core")
	close(heartbeatStop)
	<-heartbeatDone
	<-heartbeatDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := scheduler.StopContext(shutdownCtx); err != nil {
		return fmt.Errorf("stop DFS core scheduler: %w", err)
	}
	health.update("stopped", false, nil)
	return nil
}

func lock(repositoryPath string) (func(), error) {
	path := filepath.Join(repositoryPath, ".git", "dfs", "daemon.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another DFS core daemon is already running for this repository")
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}
