package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"golang.org/x/sys/unix"
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

func TestReceivedChangesWaitForOpenWriterGuard(t *testing.T) {
	root := t.TempDir()
	writer := &Repository{Config: config.Default("writer", root)}
	receiver := &Repository{Config: config.Default("receiver", root)}
	unlock, err := writer.BeginWriteGuard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		release, lockErr := receiver.lockWriterProcess(context.Background(), unix.LOCK_EX)
		if lockErr == nil {
			close(entered)
			release()
		}
	}()
	select {
	case <-entered:
		t.Fatal("received-change guard entered while a writer was open")
	case <-time.After(75 * time.Millisecond):
	}
	unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("received-change guard did not proceed after the writer closed")
	}
	<-done
}
