package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
)

type Scheduler struct {
	repo     *repository.Repository
	interval time.Duration
	logger   *slog.Logger
	events   chan string
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	writers  atomic.Int64
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

func (s *Scheduler) Stop() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

func (s *Scheduler) Notify(reason string) {
	s.logger.Debug("sync requested", "reason", reason)
	select {
	case s.events <- reason:
	default:
	}
}

func (s *Scheduler) BeginWrite() {
	writers := s.writers.Add(1)
	s.logger.Debug("writer opened", "open_writers", writers)
}

func (s *Scheduler) EndWrite() {
	writers := s.writers.Add(-1)
	if writers < 0 {
		s.writers.Store(0)
		writers = 0
	}
	s.logger.Debug("writer closed", "open_writers", writers)
}

func (s *Scheduler) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	var debounce *time.Timer
	var debounceC <-chan time.Time
	for {
		select {
		case <-s.stop:
			if debounce != nil {
				debounce.Stop()
			}
			s.sync("shutdown")
			s.logger.Info("sync scheduler stopped")
			return
		case <-ticker.C:
			s.sync("periodic")
		case <-s.events:
			if debounce == nil {
				debounce = time.NewTimer(1500 * time.Millisecond)
			} else {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounce.Reset(1500 * time.Millisecond)
			}
			debounceC = debounce.C
		case <-debounceC:
			debounceC = nil
			s.sync("filesystem change")
		}
	}
}

func (s *Scheduler) sync(reason string) {
	writers := s.writers.Load()
	if writers > 0 {
		s.logger.Debug("automatic sync skipped", "reason", reason, "open_writers", writers)
		return
	}
	started := time.Now()
	s.logger.Info("automatic sync started", "reason", reason)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	passes, err := syncUntilConverged(ctx, 4, 2, s.repo.TreeID, func(ctx context.Context) error {
		return s.repo.Sync(ctx, true)
	})
	if err != nil {
		s.logger.Error("automatic sync failed", "reason", reason, "duration", time.Since(started), "error", err)
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

func syncUntilConverged(
	ctx context.Context,
	maxPasses, stablePasses int,
	treeID func(context.Context) (string, error),
	syncPass func(context.Context) error,
) (int, error) {
	if maxPasses < 1 || stablePasses < 1 {
		return 0, fmt.Errorf("invalid convergence limits: max=%d stable=%d", maxPasses, stablePasses)
	}
	previous, err := treeID(ctx)
	if err != nil {
		return 0, fmt.Errorf("read tree before synchronization: %w", err)
	}
	changed := false
	stable := 0
	for pass := 1; pass <= maxPasses; pass++ {
		if err := syncPass(ctx); err != nil {
			return pass, err
		}
		current, err := treeID(ctx)
		if err != nil {
			return pass, fmt.Errorf("read tree after synchronization: %w", err)
		}
		if current == previous {
			stable++
			// Do not make every unchanged periodic sync run twice. Once a pass
			// has changed the tree, require two confirming stable passes.
			if !changed || stable >= stablePasses {
				return pass, nil
			}
		} else {
			changed = true
			stable = 0
		}
		previous = current
	}
	return maxPasses, fmt.Errorf("filesystem tree did not converge after %d synchronization passes", maxPasses)
}
