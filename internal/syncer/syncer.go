package syncer

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
)

type Scheduler struct {
	repo     *repository.Repository
	interval time.Duration
	log      io.Writer
	events   chan string
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	writers  atomic.Int64
}

func New(repo *repository.Repository, interval time.Duration, log io.Writer) *Scheduler {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Scheduler{
		repo:     repo,
		interval: interval,
		log:      log,
		events:   make(chan string, 128),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (s *Scheduler) Start() {
	go s.loop()
	s.Notify("startup")
}

func (s *Scheduler) Stop() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

func (s *Scheduler) Notify(reason string) {
	select {
	case s.events <- reason:
	default:
	}
}

func (s *Scheduler) BeginWrite() { s.writers.Add(1) }

func (s *Scheduler) EndWrite() {
	if s.writers.Add(-1) < 0 {
		s.writers.Store(0)
	}
	s.Notify("completed write")
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
	if s.writers.Load() > 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.repo.Sync(ctx, true); err != nil {
		s.printf("automatic sync (%s) failed: %v\n", reason, err)
		return
	}
	pins, err := s.repo.Store.Pins()
	if err != nil {
		s.printf("read pins: %v\n", err)
		return
	}
	for _, path := range pins {
		if err := s.repo.Fetch(ctx, path, ""); err != nil {
			s.printf("refresh pinned path %s: %v\n", path, err)
		}
	}
	if dropped, err := s.repo.Prune(ctx); err != nil {
		s.printf("cache prune: %v\n", err)
	} else if len(dropped) > 0 {
		s.printf("evicted %d file(s) to enforce cache quota\n", len(dropped))
	}
}

func (s *Scheduler) printf(format string, args ...any) {
	if s.log != nil {
		_, _ = fmt.Fprintf(s.log, format, args...)
	}
}
