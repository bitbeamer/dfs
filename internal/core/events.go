package core

import (
	"context"
	"sync"
	"time"
)

const defaultEventRetention = 4096

type eventLog struct {
	mu          sync.Mutex
	events      []eventRecord
	next        Cursor
	limit       int
	changed     chan struct{}
	closed      bool
	subscribers int
}

type eventRecord struct {
	cursor      Cursor
	operationID string
	kind        string
	paths       [2]string
	pathCount   uint8
	at          time.Time
}

func (r eventRecord) public() Event {
	paths := make([]string, int(r.pathCount))
	copy(paths, r.paths[:r.pathCount])
	return Event{Cursor: r.cursor, OperationID: r.operationID, Kind: r.kind, Paths: paths, At: r.at}
}

func newEventLog(limit int) *eventLog {
	if limit <= 0 {
		limit = defaultEventRetention
	}
	return &eventLog{limit: limit, changed: make(chan struct{}), next: 1}
}

func (l *eventLog) publish(kind, operationID string, paths ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	event := eventRecord{cursor: l.next, operationID: operationID, kind: kind, at: time.Now()}
	if len(paths) > len(event.paths) {
		paths = paths[:len(event.paths)]
	}
	event.pathCount = uint8(copy(event.paths[:], paths))
	l.next++
	if len(l.events) < l.limit {
		l.events = append(l.events, event)
	} else {
		copy(l.events, l.events[1:])
		l.events[len(l.events)-1] = event
	}
	if l.subscribers > 0 {
		close(l.changed)
		l.changed = make(chan struct{})
	}
}

func (l *eventLog) subscribe(ctx context.Context, after Cursor) (Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, classify("subscribe", "", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.events) > 0 && after != 0 && after < l.events[0].cursor-1 {
		return nil, &Error{Code: CodeEventGone, Op: "subscribe"}
	}
	l.subscribers++
	return &subscription{log: l, after: after}, nil
}

func (l *eventLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.changed)
	}
}

type subscription struct {
	log       *eventLog
	mu        sync.Mutex
	after     Cursor
	closed    bool
	closeOnce sync.Once
}

func (s *subscription) Next(ctx context.Context) (Event, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return Event{}, &Error{Code: CodeCanceled, Op: "next event"}
		}
		after := s.after
		s.mu.Unlock()

		s.log.mu.Lock()
		if len(s.log.events) > 0 && after != 0 && after < s.log.events[0].cursor-1 {
			s.log.mu.Unlock()
			return Event{}, &Error{Code: CodeEventGone, Op: "next event"}
		}
		for _, event := range s.log.events {
			if event.cursor > after {
				s.log.mu.Unlock()
				s.mu.Lock()
				s.after = event.cursor
				s.mu.Unlock()
				return event.public(), nil
			}
		}
		changed, closed := s.log.changed, s.log.closed
		s.log.mu.Unlock()
		if closed {
			return Event{}, &Error{Code: CodeUnavailable, Op: "next event"}
		}
		select {
		case <-ctx.Done():
			return Event{}, classify("next event", "", ctx.Err())
		case <-changed:
		}
	}
}

func (s *subscription) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.log.mu.Lock()
		if s.log.subscribers > 0 {
			s.log.subscribers--
		}
		if !s.log.closed {
			close(s.log.changed)
			s.log.changed = make(chan struct{})
		}
		s.log.mu.Unlock()
	})
	return nil
}
