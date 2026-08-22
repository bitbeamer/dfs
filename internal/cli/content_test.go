package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
)

func TestContentListShowsLocalResidency(t *testing.T) {
	if err := repository.CheckDependencies(); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	t.Cleanup(func() {
		// Frozen annex objects are read-only; let TempDir removal succeed.
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, info.Mode()|0o700)
			}
			return nil
		})
	})
	repo, err := repository.Init(ctx, filepath.Join(dir, "repo"), "content-test", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var output bytes.Buffer
	app := &App{Out: &output, Err: &output, repo: repo.Config.Repository}

	command := app.contentListCommand()
	command.SetArgs([]string{})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("No files in the DFS namespace")) {
		t.Fatalf("empty listing output = %q", output.String())
	}

	if err := os.WriteFile(filepath.Join(repo.Config.Repository, "annexed.txt"), []byte("annex content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, ctx, repo.Config.Repository, "annex", "add", "--", "annexed.txt")
	if err := os.WriteFile(filepath.Join(repo.Config.Repository, "plain.txt"), []byte("git content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, ctx, repo.Config.Repository, "-c", "annex.largefiles=nothing", "add", "--", "plain.txt")
	if _, err := repo.CommitPending(ctx, "Add content list fixtures"); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	command = app.contentListCommand()
	command.SetArgs([]string{})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"annexed.txt", "hydrated", "plain.txt", "git"} {
		if !bytes.Contains(output.Bytes(), []byte(expected)) {
			t.Fatalf("local listing missing %q: %q", expected, output.String())
		}
	}

	output.Reset()
	command = app.contentListCommand()
	command.SetArgs([]string{"--scope", "cluster"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"annexed.txt", "content-test (here)", "plain.txt", "git metadata", "can be stale"} {
		if !bytes.Contains(output.Bytes(), []byte(expected)) {
			t.Fatalf("cluster listing missing %q: %q", expected, output.String())
		}
	}

	output.Reset()
	command = app.contentListCommand()
	command.SetArgs([]string{"does-not-exist"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`No files match "does-not-exist"`)) {
		t.Fatalf("filtered listing output = %q", output.String())
	}
}

func git(t *testing.T, ctx context.Context, directory string, args ...string) {
	t.Helper()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", args[0], output, err)
	}
}
