package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
)

func TestSyncIsolatesUnavailableRemoteAndReleasesRepositoryLock(t *testing.T) {
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}
	home := t.TempDir()
	defer makeTreeWritable(home)
	gitconfig := []byte("[user]\n\tname = DFS Test\n\temail = dfs@example.invalid\n")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), gitconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	source, err := Init(ctx, filepath.Join(home, "source"), "source", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	healthy, err := Join(ctx, source.Config.Repository, filepath.Join(home, "healthy"), "healthy", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	if err := source.AddRemote(ctx, "healthy", healthy.Config.Repository); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(home, "offline-probe-started")
	helper := filepath.Join(home, "offline-remote")
	script := fmt.Sprintf("#!/bin/sh\n: > %q\nsleep 4\nexit 1\n", marker)
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := source.runner.Run(ctx, "git", "config", "protocol.ext.allow", "always"); err != nil {
		t.Fatal(err)
	}
	if err := source.AddRemote(ctx, "offline", "ext::"+helper+" %S"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Config.Repository, "new.txt"), []byte("healthy peer receives this\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	syncDone := make(chan error, 1)
	go func() { syncDone <- source.Sync(ctx, true) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("offline remote probe did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	operationDone := make(chan error, 1)
	go func() {
		_, operationErr := source.TreeID(ctx)
		operationDone <- operationErr
	}()
	select {
	case err := <-operationDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("repository operation waited for the unavailable remote probe")
	}

	err = <-syncDone
	var degraded *RemoteSyncError
	if !errors.As(err, &degraded) || len(degraded.Failures) != 1 || degraded.Failures[0].Remote != "offline" {
		t.Fatalf("sync error = %v, want one unavailable offline remote", err)
	}
	if err := healthy.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(healthy.Config.Repository, "new.txt")); err != nil {
		t.Fatalf("healthy remote did not receive namespace update: %v", err)
	}

	if err := os.Remove(filepath.Join(source.Config.Repository, "new.txt")); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = source.Sync(ctx, true)
	if !errors.As(err, &degraded) {
		t.Fatalf("backoff sync error = %v, want unavailable remote warning", err)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("unavailable remote backoff took %v", elapsed)
	}
	if err := healthy.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(healthy.Config.Repository, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("healthy remote retained deleted path: %v", err)
	}
}

func TestDirectionalPushPublishesThroughPeerInbox(t *testing.T) {
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}
	home := t.TempDir()
	defer makeTreeWritable(home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = DFS Test\n\temail = dfs@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	source, err := Init(ctx, filepath.Join(home, "source"), "source", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	receiver, err := Join(ctx, source.Config.Repository, filepath.Join(home, "receiver"), "receiver", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if err := source.AddRemote(ctx, "receiver", receiver.Config.Repository); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(source.Config.Repository, "event.txt")
	if err := os.WriteFile(path, []byte("created\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := source.SyncDirectional(ctx, true, false, true); err != nil {
		t.Fatal(err)
	}
	inbox := "refs/heads/dfs-incoming/" + source.Config.PeerID + "/main"
	if _, err := receiver.runner.Run(ctx, "git", "rev-parse", "--verify", inbox); err != nil {
		t.Fatalf("receiver has no sender inbox ref: %v", err)
	}
	inboxTree, err := receiver.runner.Run(ctx, "git", "ls-tree", "-r", "--name-only", inbox)
	if err != nil || !strings.Contains(inboxTree, "event.txt") {
		t.Fatalf("sender inbox tree = %q, %v", inboxTree, err)
	}
	if err := receiver.ApplyReceived(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(receiver.Config.Repository, "event.txt")); err != nil {
		t.Fatalf("received inbox did not materialize file metadata: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := source.SyncDirectional(ctx, true, false, true); err != nil {
		t.Fatal(err)
	}
	if err := receiver.ApplyReceived(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(receiver.Config.Repository, "event.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("received inbox retained deleted file: %v", err)
	}
}

func TestTwoPeerMetadataAndContentFlow(t *testing.T) {
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}
	home := t.TempDir()
	gitconfig := []byte("[user]\n\tname = DFS Test\n\temail = dfs@example.invalid\n")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), gitconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	linux, err := Init(ctx, filepath.Join(home, "linux"), "linux", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer linux.Close()
	if _, err := os.Stat(filepath.Join(linux.Config.Repository, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("DFS init created a user-visible .gitignore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linux.Config.Repository, config.LegacyDirectory)); !os.IsNotExist(err) {
		t.Fatalf("DFS init created legacy worktree state: %v", err)
	}
	if _, err := os.Stat(config.Path(linux.Config.Repository)); err != nil {
		t.Fatalf("private DFS config is not under Git metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(linux.Config.Repository, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := linux.CommitPending(ctx, "Add hello"); err != nil {
		t.Fatal(err)
	}

	mac, err := Join(ctx, linux.Config.Repository, filepath.Join(home, "mac"), "mac", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer mac.Close()
	if err := mac.Fetch(ctx, "hello.txt", "origin"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(mac.Config.Repository, "hello.txt"))
	if err != nil || string(content) != "hello\n" {
		t.Fatalf("fetched content = %q, %v", content, err)
	}
	if err := mac.Evict(ctx, "hello.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(mac.Config.Repository, "Archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(mac.Config.Repository, "hello.txt"), filepath.Join(mac.Config.Repository, "Archive", "hello.txt")); err != nil {
		t.Fatal(err)
	}
	if err := mac.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := linux.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(linux.Config.Repository, "Archive", "hello.txt")); err != nil {
		t.Fatalf("metadata move did not reach Linux peer: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(linux.Config.Repository, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("old path still exists: %v", err)
	}
}
