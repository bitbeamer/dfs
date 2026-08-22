package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
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

func TestRecoverWorkTreeWaitsForCrossProcessLockBeforeRecovery(t *testing.T) {
	root := t.TempDir()
	first := &Repository{Config: config.Default("first", root)}
	second := &Repository{Config: config.Default("second", root)}
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = first.WithWorkTreeLock(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	prepared := false
	err := second.RecoverWorkTree(ctx, func() error {
		prepared = true
		return nil
	})
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) || prepared {
		t.Fatalf("recovery under live worktree lock = prepared %t, error %v", prepared, err)
	}
}
