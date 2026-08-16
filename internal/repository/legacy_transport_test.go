package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRemovesOnlyLegacyDFSTransportConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Migration Test\nemail = migration@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo, err := Init(ctx, filepath.Join(home, "repository"), "migration", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	path := repo.Config.Repository
	for _, arguments := range [][]string{
		{"config", "core.sshCommand", "legacy-command"},
		{"config", "annex.ssh-options", "legacy-options"},
		{"config", "remote.dfs-peer-123456789abc.dfs-ssh-url", "legacy://peer"},
		{"config", "dfs.user-setting", "keep"},
	} {
		if _, err := repo.runner.Run(ctx, "git", arguments...); err != nil {
			t.Fatal(err)
		}
	}
	stateDirectory := filepath.Join(path, ".git", "dfs")
	for _, name := range []string{"peer-ssh-key", "peer-ssh-key.pub", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(stateDirectory, name), []byte("legacy\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for _, key := range []string{"core.sshCommand", "annex.ssh-options", "remote.dfs-peer-123456789abc.dfs-ssh-url"} {
		if _, err := repo.runner.Run(ctx, "git", "config", "--get", key); err == nil {
			t.Fatalf("legacy setting %s remains", key)
		}
	}
	value, err := repo.runner.Run(ctx, "git", "config", "--get", "dfs.user-setting")
	if err != nil || value != "keep\n" {
		t.Fatalf("unrelated setting = %q, %v", value, err)
	}
	for _, name := range []string{"peer-ssh-key", "peer-ssh-key.pub", "known_hosts"} {
		if _, err := os.Stat(filepath.Join(stateDirectory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy credential %s remains: %v", name, err)
		}
	}
}
