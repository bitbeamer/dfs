package core

import (
	"context"
	"sync"
	"time"
)

const defaultEventRetention = 4096

type eventLog struct {
	mu      sync.Mutex
	events  []Event
	next    Cursor
	limit   int
	changed chan struct{}
	closed  bool
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
	event := Event{Cursor: l.next, OperationID: operationID, Kind: kind, Paths: append([]string(nil), paths...), At: time.Now()}
	l.next++
	l.events = append(l.events, event)
	if len(l.events) > l.limit {
		l.events = append([]Event(nil), l.events[len(l.events)-l.limit:]...)
	}
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *eventLog) subscribe(ctx context.Context, after Cursor) (Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, classify("subscribe", "", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.events) > 0 && after != 0 && after < l.events[0].Cursor-1 {
		return nil, &Error{Code: CodeEventGone, Op: "subscribe"}
	}
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
	log    *eventLog
	mu     sync.Mutex
	after  Cursor
	closed bool
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
		if len(s.log.events) > 0 && after != 0 && after < s.log.events[0].Cursor-1 {
			s.log.mu.Unlock()
			return Event{}, &Error{Code: CodeEventGone, Op: "next event"}
		}
		for _, event := range s.log.events {
			if event.Cursor > after {
				s.log.mu.Unlock()
				s.mu.Lock()
				s.after = event.Cursor
				s.mu.Unlock()
				return event, nil
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
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
