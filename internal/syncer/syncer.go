package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
)

const eventDebounce = 300 * time.Millisecond

const (
	receiveAndPublishReason  = "filesystem change and managed Git receive"
	maintenanceReceiveReason = "maintenance and managed Git receive"
)

type Scheduler struct {
	repo           *repository.Repository
	interval       time.Duration
	logger         *slog.Logger
	events         chan string
	stop           chan struct{}
	done           chan struct{}
	once           sync.Once
	writers        atomic.Int64
	activeMu       sync.Mutex
	active         string
	cancel         context.CancelFunc
	entries        EntryInvalidator
	reconcile      func(context.Context) error
	syncOverride   func(string)
	deferredMu     sync.Mutex
	deferredReason string
}

type EntryInvalidator interface {
	InvalidateEntry(path string)
}

func New(repo *repository.Repository, interval time.Duration, logger *slog.Logger) *Scheduler {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Scheduler{
		repo:     repo,
		interval: interval,
		logger:   logger,
		events:   make(chan string, 128),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (s *Scheduler) Start() {
	s.logger.Info("sync scheduler started", "interval", s.interval)
	go s.loop()
	s.Notify("startup")
}

func (s *Scheduler) SetEntryInvalidator(invalidator EntryInvalidator) {
	s.entries = invalidator
}

func (s *Scheduler) Invalidate(paths []string) {
	if s.entries == nil {
		return
	}
	for _, path := range paths {
		s.entries.InvalidateEntry(path)
	}
}

func (s *Scheduler) SetReconciler(reconcile func(context.Context) error) {
	s.reconcile = reconcile
}

func (s *Scheduler) Stop() {
	_ = s.StopContext(context.Background())
}

func (s *Scheduler) StopContext(ctx context.Context) error {
	s.once.Do(func() { close(s.stop) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		s.activeMu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.activeMu.Unlock()
		select {
		case <-s.done:
			return nil
		case <-time.After(time.Second):
			return ctx.Err()
		}
	}
}

func (s *Scheduler) Notify(reason string) {
	s.logger.Debug("sync requested", "reason", reason)
	s.activeMu.Lock()
	if s.cancel != nil && (s.active == "startup" || s.active == "periodic") {
		s.cancel()
	}
	s.activeMu.Unlock()
	select {
	case s.events <- reason:
	default:
	}
}

func (s *Scheduler) BeginWrite() {
	writers := s.writers.Add(1)
	// A local FUSE mutation takes priority over an outbound background pass.
	// Canceling it releases the repository lock; EndWrite's subsequent change
	// notification schedules a fresh pass containing the local operation.
	// Applying a received commit and shutdown cleanup are intentionally atomic.
	s.activeMu.Lock()
	if s.cancel != nil && s.active != "managed Git receive" && s.active != "shutdown" {
		s.cancel()
	}
	s.activeMu.Unlock()
	s.logger.Debug("writer opened", "open_writers", writers)
}

func (s *Scheduler) EndWrite() {
	writers := s.writers.Add(-1)
	if writers < 0 {
		s.writers.Store(0)
		writers = 0
	}
	s.logger.Debug("writer closed", "open_writers", writers)
	if writers == 0 {
		s.deferredMu.Lock()
		reason := s.deferredReason
		s.deferredReason = ""
		s.deferredMu.Unlock()
		if reason != "" {
			s.Notify(reason)
		}
	}
}

func (s *Scheduler) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	var debounce *time.Timer
	var debounceC <-chan time.Time
	pendingReason := ""
	for {
		select {
		case <-s.stop:
			if debounce != nil {
				debounce.Stop()
			}
			s.logger.Info("sync scheduler stopped")
			return
		case <-ticker.C:
			if debounceC != nil {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounceC = nil
				pendingReason = mergeReasons(pendingReason, "periodic")
				s.runSync(pendingReason)
				pendingReason = ""
			} else {
				s.runSync("periodic")
			}
		case reason := <-s.events:
			if debounceC == nil {
				pendingReason = reason
			} else {
				pendingReason = mergeReasons(pendingReason, reason)
			}
			if debounceC != nil {
				continue
			}
			if debounce == nil {
				debounce = time.NewTimer(eventDebounce)
			} else {
				debounce.Reset(eventDebounce)
			}
			debounceC = debounce.C
		case <-debounceC:
			debounceC = nil
			s.runSync(pendingReason)
			pendingReason = ""
		}
	}
}

func (s *Scheduler) runSync(reason string) {
	if s.syncOverride != nil {
		s.syncOverride(reason)
		return
	}
	s.sync(reason)
}

func (s *Scheduler) sync(reason string) {
	writers := s.writers.Load()
	if writers > 0 && requiresWriterDrain(reason) {
		s.logger.Debug("automatic sync skipped", "reason", reason, "open_writers", writers)
		s.deferredMu.Lock()
		s.deferredReason = mergeReasons(s.deferredReason, reason)
		s.deferredMu.Unlock()
		return
	}
	started := time.Now()
	s.logger.Info("automatic sync started", "reason", reason)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	s.activeMu.Lock()
	s.active = reason
	s.cancel = cancel
	s.activeMu.Unlock()
	defer func() {
		cancel()
		s.activeMu.Lock()
		s.active = ""
		s.cancel = nil
		s.activeMu.Unlock()
	}()
	degradedRemotes := make(map[string]error)
	maxPasses, stablePasses := 1, 0
	maintenance := isMaintenanceReason(reason)
	if maintenance {
		maxPasses, stablePasses = 4, 1
	}
	passes, _, err := syncUntilConverged(ctx, maxPasses, stablePasses, s.repo.TreeID, s.repo.ChangedPaths, func(ctx context.Context) error {
		var err error
		switch reason {
		case "managed Git receive":
			err = s.repo.ApplyReceived(ctx)
		case receiveAndPublishReason:
			if err = s.repo.ApplyReceived(ctx); err == nil {
				err = s.repo.SyncDirectional(ctx, true, false, true)
			}
		case maintenanceReceiveReason:
			if err = s.repo.ApplyReceived(ctx); err == nil {
				err = s.repo.Sync(ctx, true)
			}
		case "startup", "periodic", "shutdown":
			err = s.repo.Sync(ctx, true)
		default:
			err = s.repo.SyncDirectional(ctx, true, false, true)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var degraded *repository.RemoteSyncError
		if errors.As(err, &degraded) {
			for _, failure := range degraded.Failures {
				degradedRemotes[failure.Remote] = failure.Err
			}
			return nil
		}
		return err
	}, func(paths []string) {
		if s.entries != nil {
			for _, path := range paths {
				s.entries.InvalidateEntry(path)
			}
		}
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			s.logger.Info("automatic sync preempted", "reason", reason, "duration", time.Since(started))
			return
		}
		s.logger.Error("automatic sync failed", "reason", reason, "duration", time.Since(started), "error", err)
		return
	}
	if len(degradedRemotes) > 0 {
		remotes := make([]string, 0, len(degradedRemotes))
		for remote := range degradedRemotes {
			remotes = append(remotes, remote)
		}
		sort.Strings(remotes)
		for _, remote := range remotes {
			s.logger.Warn("remote synchronization deferred", "remote", remote, "error", degradedRemotes[remote])
		}
	}
	if maintenance && s.reconcile != nil {
		if err := s.reconcile(ctx); err != nil {
			s.logger.Error("membership reconciliation failed", "error", err)
			return
		}
	}
	if !maintenance {
		s.logger.Info("automatic sync completed",
			"reason", reason,
			"duration", time.Since(started),
			"convergence_passes", passes,
			"pins_refreshed", 0,
			"files_evicted", 0,
		)
		return
	}
	pins, err := s.repo.Store.Pins()
	if err != nil {
		s.logger.Error("reading pins failed", "error", err)
		return
	}
	refreshed := 0
	for _, path := range pins {
		if err := s.repo.Fetch(ctx, path, ""); err != nil {
			s.logger.Error("refreshing pinned path failed", "path", path, "error", err)
		} else {
			refreshed++
		}
	}
	if dropped, err := s.repo.Prune(ctx); err != nil {
		s.logger.Error("cache prune failed", "error", err)
	} else {
		s.logger.Info("automatic sync completed",
			"reason", reason,
			"duration", time.Since(started),
			"convergence_passes", passes,
			"pins_refreshed", refreshed,
			"files_evicted", len(dropped),
		)
	}
}

func requiresWriterDrain(reason string) bool {
	switch reason {
	case "managed Git receive", receiveAndPublishReason, maintenanceReceiveReason, "startup", "periodic", "shutdown":
		return true
	default:
		return false
	}
}

func isMaintenanceReason(reason string) bool {
	switch reason {
	case "startup", "periodic", "shutdown", "pin policy changed", "peer requested membership reconciliation", maintenanceReceiveReason:
		return true
	default:
		return false
	}
}

func mergeReasons(current, next string) string {
	maintenance := isMaintenanceReason(current) || isMaintenanceReason(next)
	receive := includesReceive(current) || includesReceive(next)
	local := includesPublish(current) || includesPublish(next)
	switch {
	case maintenance && receive:
		return maintenanceReceiveReason
	case maintenance:
		if isMaintenanceReason(next) {
			return next
		}
		return current
	case receive && local:
		return receiveAndPublishReason
	case receive:
		return "managed Git receive"
	default:
		return next
	}
}

func includesReceive(reason string) bool {
	return reason == "managed Git receive" || reason == receiveAndPublishReason || reason == maintenanceReceiveReason
}

func includesPublish(reason string) bool {
	return reason != "" && !isMaintenanceReason(reason) && reason != "managed Git receive"
}

func syncUntilConverged(
	ctx context.Context,
	maxPasses, stablePasses int,
	treeID func(context.Context) (string, error),
	changedPaths func(context.Context, string, string) ([]string, error),
	syncPass func(context.Context) error,
	onChanged func([]string),
) (int, []string, error) {
	if maxPasses < 1 || stablePasses < 0 {
		return 0, nil, fmt.Errorf("invalid convergence limits: max=%d stable=%d", maxPasses, stablePasses)
	}
	previous, err := treeID(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("read tree before synchronization: %w", err)
	}
	changedSet := make(map[string]bool)
	var changedList []string
	changed := false
	stable := 0
	for pass := 1; pass <= maxPasses; pass++ {
		syncErr := syncPass(ctx)
		current, err := treeID(ctx)
		if err != nil {
			if syncErr != nil {
				return pass, changedList, errors.Join(syncErr, fmt.Errorf("read tree after failed synchronization: %w", err))
			}
			return pass, changedList, fmt.Errorf("read tree after synchronization: %w", err)
		}
		if current != previous {
			paths, err := changedPaths(ctx, previous, current)
			if err != nil {
				if syncErr != nil {
					return pass, changedList, errors.Join(syncErr, fmt.Errorf("list partially synchronized paths: %w", err))
				}
				return pass, changedList, fmt.Errorf("list synchronized paths: %w", err)
			}
			for _, path := range paths {
				if !changedSet[path] {
					changedSet[path] = true
					changedList = append(changedList, path)
				}
			}
			if onChanged != nil && len(paths) > 0 {
				onChanged(paths)
			}
		}
		if syncErr != nil {
			return pass, changedList, syncErr
		}
		if stablePasses == 0 {
			return pass, changedList, nil
		}
		if current == previous {
			stable++
			// Do not make every unchanged periodic sync run twice. Once a pass
			// has changed the tree, require two confirming stable passes.
			if !changed || stable >= stablePasses {
				return pass, changedList, nil
			}
		} else {
			changed = true
			stable = 0
		}
		previous = current
	}
	return maxPasses, changedList, fmt.Errorf("filesystem tree did not converge after %d synchronization passes", maxPasses)
}
