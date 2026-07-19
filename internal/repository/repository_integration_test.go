package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
)

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
