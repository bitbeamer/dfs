package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSize(t *testing.T) {
	tests := map[string]int64{
		"1":      1,
		"10MiB":  10 << 20,
		"1.5GiB": int64(1.5 * (1 << 30)),
		"2 GB":   2_000_000_000,
	}
	for input, expected := range tests {
		actual, err := ParseSize(input)
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", input, err)
		}
		if actual != expected {
			t.Fatalf("ParseSize(%q) = %d, want %d", input, actual, expected)
		}
	}
}

func TestParseSizeRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{"", "-1GiB", "many"} {
		if _, err := ParseSize(input); err == nil {
			t.Fatalf("ParseSize(%q) unexpectedly succeeded", input)
		}
	}
}

func TestLoadMigratesLegacyStateIntoGitDirectory(t *testing.T) {
	repository := t.TempDir()
	legacy := filepath.Join(repository, LegacyDirectory)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Default("legacy", repository)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "state.db"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "legacy" {
		t.Fatalf("loaded peer name = %q", loaded.Name)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy state directory remains: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(Directory), "state.db")); err != nil || string(content) != "state" {
		t.Fatalf("migrated state = %q, %v", content, err)
	}
}
