package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
)

func TestSyncDryRunShowsPlan(t *testing.T) {
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
	repo, err := repository.Init(ctx, filepath.Join(dir, "repo"), "sync-test", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for name, content := range map[string]string{"small.txt": "small\n", "big.bin": "big\n"} {
		if err := os.WriteFile(filepath.Join(repo.Config.Repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, ctx, repo.Config.Repository, "annex", "add", "--", "small.txt", "big.bin")
	if _, err := repo.CommitPending(ctx, "Add sync dry-run fixtures"); err != nil {
		t.Fatal(err)
	}
	git(t, ctx, repo.Config.Repository, "annex", "drop", "--force", "--", "big.bin")
	if err := repo.SetLocalPin("big.bin"); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	app := &App{Out: &output, Err: &output, repo: repo.Config.Repository}
	command := app.syncCommand()
	command.SetArgs([]string{"--dry-run"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Dry run: sync (mode full); no changes will be applied",
		"Metadata: bidirectional sync with 0 remote(s)",
		"Pins: refresh 1 pinned path(s)",
		"big.bin",
		"would fetch 1 missing file(s)",
		"within limit; no eviction needed",
	} {
		if !bytes.Contains(output.Bytes(), []byte(expected)) {
			t.Fatalf("sync dry run missing %q: %q", expected, output.String())
		}
	}
	// A dry run must not hydrate the missing pinned file.
	if pinned, err := repo.Store.IsPinned("big.bin"); err != nil || !pinned {
		t.Fatalf("pin after dry run: pinned=%v err=%v", pinned, err)
	}
	gitOutput := git(t, ctx, repo.Config.Repository, "annex", "find", "--in=here", "--", "big.bin")
	if gitOutput != "" {
		t.Fatalf("dry run hydrated big.bin: %q", gitOutput)
	}

	output.Reset()
	command = app.syncCommand()
	command.SetArgs([]string{"--dry-run", "--metadata-only"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("Metadata mode skips pin refresh")) {
		t.Fatalf("metadata dry run output = %q", output.String())
	}

	output.Reset()
	jsonApp := &App{Out: &output, Err: &output, repo: repo.Config.Repository, output: "json"}
	command = jsonApp.syncCommand()
	command.SetArgs([]string{"--dry-run"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Action string `json:"action"`
		Plan   struct {
			CacheLimit int64            `json:"cache_limit_bytes"`
			Pins       []map[string]any `json:"pins"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatalf("decode sync dry-run JSON: %v: %q", err, output.String())
	}
	if envelope.Action != "sync" || envelope.Plan.CacheLimit != 1<<20 || len(envelope.Plan.Pins) != 1 {
		t.Fatalf("sync dry-run JSON = %+v", envelope)
	}
}
