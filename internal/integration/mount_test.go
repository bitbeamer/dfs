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
	logContent, err := os.ReadFile(mountLogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"msg=\"mount ready\"", "msg=\"file created\"", "msg=\"write completed\"", "msg=\"automatic sync completed\""} {
		if !strings.Contains(string(logContent), expected) {
			t.Fatalf("mount info log does not contain %q:\n%s", expected, logContent)
		}
	}
}
