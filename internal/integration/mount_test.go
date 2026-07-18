package integration

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dfsmount "github.com/bitbeamer/dfs/internal/mount"
	"github.com/bitbeamer/dfs/internal/repository"
)

func TestMountedWriteIsAnnexed(t *testing.T) {
	if os.Getenv("DFS_INTEGRATION") == "" {
		t.Skip("set DFS_INTEGRATION=1 to run mount tests")
	}
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}
	home := t.TempDir()
	// git-annex deliberately freezes object directories; thaw them before the
	// testing package attempts to remove its temporary directory.
	t.Cleanup(func() {
		_ = filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				if info.IsDir() {
					_ = os.Chmod(path, 0o700)
				} else {
					_ = os.Chmod(path, 0o600)
				}
			}
			return nil
		})
	})
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname=DFS Test\nemail=dfs@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repo, err := repository.Init(ctx, filepath.Join(home, "repo"), "test", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	mountpoint := filepath.Join(home, "mnt")
	mountLogPath := filepath.Join(home, "mount.log")
	mountLog, err := os.OpenFile(mountLogPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer mountLog.Close()
	logger := slog.New(slog.NewTextHandler(mountLog, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mountContext, cancelMount := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- dfsmount.Run(repo, mountpoint, dfsmount.Options{Context: mountContext, Logger: logger})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(mountpoint, ".gitignore")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mount did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer func() {
		cancelMount()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("mount: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("mount did not stop")
		}
		logContent, err := os.ReadFile(mountLogPath)
		if err != nil {
			t.Errorf("read mount log after shutdown: %v", err)
			return
		}
		for _, expected := range []string{"msg=\"shutdown requested\"", "msg=\"mount stopped\"", "reason=shutdown"} {
			if !strings.Contains(string(logContent), expected) {
				t.Errorf("mount shutdown log does not contain %q:\n%s", expected, logContent)
			}
		}
	}()
	file, err := os.OpenFile(filepath.Join(mountpoint, "mounted.txt"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("mounted\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	lookup := exec.CommandContext(ctx, "git", "annex", "lookupkey", "mounted.txt")
	lookup.Dir = repo.Config.Repository
	if err := lookup.Run(); err == nil {
		t.Fatal("open file was annexed before close")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	command := exec.CommandContext(ctx, "git", "annex", "whereis", "mounted.txt")
	command.Dir = repo.Config.Repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("mounted file was not annexed: %s: %v", output, err)
	}
	underlying := filepath.Join(repo.Config.Repository, "mounted.txt")
	if info, err := os.Lstat(underlying); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("annexed file is not locked after sync: mode=%v err=%v", infoMode(info), err)
	}
	originalTarget, err := os.Readlink(underlying)
	if err != nil {
		t.Fatal(err)
	}
	originalHead := gitOutput(t, ctx, repo.Config.Repository, "rev-parse", "HEAD")
	noop, err := os.OpenFile(filepath.Join(mountpoint, "mounted.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := noop.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if target, err := os.Readlink(underlying); err != nil || target != originalTarget {
		t.Fatalf("mutation-free writable open changed annex entry: target=%q err=%v", target, err)
	}
	if head := gitOutput(t, ctx, repo.Config.Repository, "rev-parse", "HEAD"); head != originalHead {
		t.Fatalf("mutation-free writable open created a commit: %s -> %s", originalHead, head)
	}

	first, err := os.OpenFile(filepath.Join(mountpoint, "mounted.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.OpenFile(filepath.Join(mountpoint, "mounted.txt"), os.O_RDWR, 0)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if _, err := first.WriteAt([]byte("shared\n"), 0); err != nil {
		t.Fatal(err)
	}
	if err := first.Truncate(int64(len("shared\n"))); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(underlying); err != nil || target != originalTarget {
		t.Fatalf("transaction published before all writable handles closed: target=%q err=%v", target, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)

	// A writable handle uses a copy-on-write staging file. Reads through the
	// mount see the transaction, while git-annex's locked working-tree entry is
	// not replaced until the final writable handle closes.
	edit, err := os.OpenFile(filepath.Join(mountpoint, "mounted.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edit.WriteAt([]byte("updated\n"), 0); err != nil {
		t.Fatal(err)
	}
	if err := edit.Truncate(int64(len("updated\n"))); err != nil {
		t.Fatal(err)
	}
	visibleDuringWrite, err := os.ReadFile(filepath.Join(mountpoint, "mounted.txt"))
	if err != nil || string(visibleDuringWrite) != "updated\n" {
		t.Fatalf("staged content during write = %q, %v", visibleDuringWrite, err)
	}
	if info, err := os.Lstat(underlying); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("writable open replaced annex entry before close: mode=%v err=%v", infoMode(info), err)
	}
	if err := edit.Close(); err != nil {
		t.Fatal(err)
	}
	visibleAfterWrite, err := os.Stat(filepath.Join(mountpoint, "mounted.txt"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	visibleAfterSync, err := os.Stat(filepath.Join(mountpoint, "mounted.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(visibleAfterWrite, visibleAfterSync) {
		t.Fatalf("visible inode changed when git-annex rewrote its internal representation")
	}
	if !visibleAfterWrite.ModTime().Equal(visibleAfterSync.ModTime()) {
		t.Fatalf("visible mtime changed during annex sync: %v -> %v", visibleAfterWrite.ModTime(), visibleAfterSync.ModTime())
	}

	if vim, err := exec.LookPath("vim"); err == nil {
		for _, text := range []string{"vim-one", "vim-two"} {
			vimCommand := exec.CommandContext(ctx, vim,
				"-Nu", "NONE", "-i", "NONE", "-n", "-es",
				filepath.Join(mountpoint, "mounted.txt"),
				"-c", "normal! Go"+text,
				"-c", "write",
				"-c", "quit",
			)
			if output, err := vimCommand.CombinedOutput(); err != nil {
				t.Fatalf("Vim save %q failed: %s: %v", text, output, err)
			}
			time.Sleep(3 * time.Second)
		}
	}
	logContent, err := os.ReadFile(mountLogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"msg=\"mount ready\"", "msg=\"file created\"", "msg=\"write transaction committed\"", "msg=\"automatic sync completed\""} {
		if !strings.Contains(string(logContent), expected) {
			t.Fatalf("mount info log does not contain %q:\n%s", expected, logContent)
		}
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode()
}

func gitOutput(t *testing.T, ctx context.Context, directory string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), output, err)
	}
	return strings.TrimSpace(string(output))
}
