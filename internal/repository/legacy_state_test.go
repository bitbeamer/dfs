package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitbeamer/dfs/internal/command"
	"github.com/bitbeamer/dfs/internal/config"
)

func TestRepairLegacyPrivateStateCompletesPrivateOnlyConflict(t *testing.T) {
	root := t.TempDir()
	gitTestRun(t, root, "init", "-q", "-b", "main")
	gitTestRun(t, root, "config", "user.name", "DFS Test")
	gitTestRun(t, root, "config", "user.email", "dfs@example.invalid")
	if err := os.MkdirAll(filepath.Join(root, ".dfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLegacyTestFile(t, root, ".dfs/mount.lock", "base\n")
	writeLegacyTestFile(t, root, ".dfs/user-data", "keep\n")
	gitTestRun(t, root, "add", "-f", ".dfs/mount.lock", ".dfs/user-data")
	gitTestRun(t, root, "commit", "-q", "-m", "track legacy state")

	gitTestRun(t, root, "checkout", "-q", "-b", "peer")
	writeLegacyTestFile(t, root, ".dfs/mount.lock", "peer\n")
	gitTestRun(t, root, "commit", "-qam", "peer state")
	gitTestRun(t, root, "checkout", "-q", "main")
	writeLegacyTestFile(t, root, ".dfs/mount.lock", "local\n")
	gitTestRun(t, root, "commit", "-qam", "local state")
	mergeCommand := exec.Command("git", "merge", "peer")
	mergeCommand.Dir = root
	if err := mergeCommand.Run(); err == nil {
		t.Fatal("legacy state merge unexpectedly succeeded")
	}

	repo := &Repository{Config: config.Default("test", root), runner: command.Runner{Directory: root}}
	if err := repo.RepairLegacyPrivateState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("cleanup merge remains active: %v", err)
	}
	tracked := gitTestOutput(t, root, "ls-files", ".dfs")
	if strings.Contains(tracked, "mount.lock") {
		t.Fatalf("legacy mount lock remains tracked: %q", tracked)
	}
	if !strings.Contains(tracked, ".dfs/user-data") {
		t.Fatalf("user-owned .dfs content was removed from Git: %q", tracked)
	}
}

func writeLegacyTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitTestRun(t *testing.T, root string, args ...string) {
	t.Helper()
	if output := gitTestOutput(t, root, args...); output != "" {
		t.Log(output)
	}
}

func gitTestOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), output, err)
	}
	return strings.TrimSpace(string(output))
}
