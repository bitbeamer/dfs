package config

import (
	"encoding/json"
	"errors"
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
	if loaded.Version != 4 || loaded.PeerID == "" || loaded.Hostname == "" || loaded.NetworkName != filepath.Base(repository) {
		t.Fatalf("migrated identity = version %d, peer %q, network %q", loaded.Version, loaded.PeerID, loaded.NetworkName)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy state directory remains: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(Directory), "state.db")); err != nil || string(content) != "state" {
		t.Fatalf("migrated state = %q, %v", content, err)
	}
}

func TestLoadUpgradesPeerIdentity(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "family-files")
	if err := os.MkdirAll(filepath.Join(repository, Directory), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := Config{Version: 1, Name: "desktop", Repository: repository, CacheLimit: 1024}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(repository), data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 4 || loaded.PeerID == "" || loaded.Hostname == "" || loaded.NetworkName != "family-files" {
		t.Fatalf("upgraded config = %#v", loaded)
	}
	reloaded, err := Load(repository)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PeerID != loaded.PeerID {
		t.Fatalf("peer ID changed across loads: %q != %q", reloaded.PeerID, loaded.PeerID)
	}
}

func TestLoadRejectsChangedHostname(t *testing.T) {
	original := hostnameProvider
	t.Cleanup(func() { hostnameProvider = original })
	hostnameProvider = func() (string, error) { return "Iris.local", nil }
	repository := t.TempDir()
	cfg := Default("iris", repository)
	if cfg.Hostname != "iris" {
		t.Fatalf("canonical hostname = %q", cfg.Hostname)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	hostnameProvider = func() (string, error) { return "Hera.local", nil }
	_, err := Load(repository)
	var mismatch *HostnameMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Load error = %v, want HostnameMismatchError", err)
	}
	if mismatch.Configured != "iris" || mismatch.Current != "hera" || mismatch.PeerID != cfg.PeerID {
		t.Fatalf("hostname mismatch = %#v", mismatch)
	}
}

func TestValidateHostnameTreatsLocalSuffixAsEquivalent(t *testing.T) {
	original := hostnameProvider
	t.Cleanup(func() { hostnameProvider = original })
	hostnameProvider = func() (string, error) { return "ZEUS.local.", nil }
	cfg := Config{Hostname: "zeus", PeerID: "peer"}
	if err := ValidateHostname(cfg); err != nil {
		t.Fatal(err)
	}
}
