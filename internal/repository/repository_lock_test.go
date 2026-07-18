package repository

import (
	"testing"
	"time"
)

func TestWithWorkTreeLockSerializesAccess(t *testing.T) {
	repo := &Repository{}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		_ = repo.WithWorkTreeLock(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	go func() {
		_ = repo.WithWorkTreeLock(func() error {
			close(done)
			return nil
		})
	}()

	select {
	case <-done:
		t.Fatal("second worktree operation entered while the first held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second worktree operation did not proceed after lock release")
	}
}
