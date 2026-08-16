package core

import (
	"context"
	"sync"
)

type heldLock struct {
	owner uint64
	lock  FileLock
}

type lockTable struct {
	mu      sync.Mutex
	byPath  map[string][]heldLock
	changed chan struct{}
	closed  bool
}

func newLockTable() *lockTable {
	return &lockTable{byPath: make(map[string][]heldLock), changed: make(chan struct{})}
}

func locksOverlap(first, second FileLock) bool {
	return first.Start <= second.End && second.Start <= first.End
}

func locksConflict(first, second FileLock) bool {
	return locksOverlap(first, second) && (first.Kind == LockWrite || second.Kind == LockWrite)
}

func (s *Service) GetLock(ctx context.Context, path string, owner uint64, requested FileLock) (FileLock, error) {
	cleaned, _, err := s.resolve(path)
	if err != nil {
		return FileLock{}, err
	}
	if err := ctx.Err(); err != nil {
		return FileLock{}, classify("get lock", cleaned, err)
	}
	s.locks.mu.Lock()
	defer s.locks.mu.Unlock()
	if s.locks.closed {
		return FileLock{}, &Error{Code: CodeUnavailable, Op: "get lock", Path: cleaned}
	}
	for _, candidate := range s.locks.byPath[cleaned] {
		if candidate.owner != owner && locksConflict(candidate.lock, requested) {
			return candidate.lock, nil
		}
	}
	requested.Kind = LockUnlocked
	return requested, nil
}

func (s *Service) SetLock(ctx context.Context, path string, owner uint64, requested FileLock, wait bool) error {
	cleaned, _, err := s.resolve(path)
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return classify("set lock", cleaned, err)
		}
		s.locks.mu.Lock()
		if s.locks.closed {
			s.locks.mu.Unlock()
			return &Error{Code: CodeUnavailable, Op: "set lock", Path: cleaned}
		}
		if requested.Kind == LockUnlocked {
			s.locks.byPath[cleaned] = unlockRange(s.locks.byPath[cleaned], owner, requested)
			s.locks.signalLocked()
			s.locks.mu.Unlock()
			return nil
		}
		conflict := false
		for _, candidate := range s.locks.byPath[cleaned] {
			if candidate.owner != owner && locksConflict(candidate.lock, requested) {
				conflict = true
				break
			}
		}
		if !conflict {
			current := unlockRange(s.locks.byPath[cleaned], owner, requested)
			s.locks.byPath[cleaned] = append(current, heldLock{owner: owner, lock: requested})
			s.locks.signalLocked()
			s.locks.mu.Unlock()
			return nil
		}
		changed := s.locks.changed
		s.locks.mu.Unlock()
		if !wait {
			return &Error{Code: CodeConflict, Op: "set lock", Path: cleaned}
		}
		select {
		case <-ctx.Done():
			return classify("set lock", cleaned, ctx.Err())
		case <-changed:
		}
	}
}

func unlockRange(locks []heldLock, owner uint64, unlocked FileLock) []heldLock {
	result := locks[:0]
	for _, candidate := range locks {
		if candidate.owner != owner || !locksOverlap(candidate.lock, unlocked) {
			result = append(result, candidate)
			continue
		}
		if candidate.lock.Start < unlocked.Start {
			left := candidate
			left.lock.End = unlocked.Start - 1
			result = append(result, left)
		}
		if candidate.lock.End > unlocked.End && unlocked.End != ^uint64(0) {
			right := candidate
			right.lock.Start = unlocked.End + 1
			result = append(result, right)
		}
	}
	return result
}

func (l *lockTable) signalLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *lockTable) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.changed)
	}
}
