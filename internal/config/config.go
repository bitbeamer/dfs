package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	Directory       = ".git/dfs"
	LegacyDirectory = ".dfs"
	FileName        = "config.json"
)

type Config struct {
	Version      int           `json:"version"`
	FileSystemID string        `json:"filesystem_id,omitempty"`
	Name         string        `json:"name"`
	Hostname     string        `json:"hostname"`
	PeerID       string        `json:"peer_id"`
	NetworkName  string        `json:"network_name"`
	Repository   string        `json:"repository"`
	CacheLimit   int64         `json:"cache_limit_bytes"`
	SyncInterval time.Duration `json:"sync_interval"`
	Relay        string        `json:"relay,omitempty"`
}

func Default(name, repository string) Config {
	hostname, _ := hostnameProvider()
	return Config{
		Version:      4,
		Name:         name,
		Hostname:     canonicalHostname(hostname),
		PeerID:       randomID(),
		NetworkName:  filepath.Base(filepath.Clean(repository)),
		Repository:   repository,
		CacheLimit:   100 * 1024 * 1024 * 1024,
		SyncInterval: 30 * time.Second,
	}
}

var hostnameProvider = os.Hostname

type HostnameMismatchError struct {
	Configured string
	Current    string
	PeerID     string
}

func (e *HostnameMismatchError) Error() string {
	peer := e.PeerID
	if len(peer) > 12 {
		peer = peer[:12]
	}
	return fmt.Sprintf(
		"DFS peer hostname changed from %q to %q; pairing %s is no longer valid; remove dfs-peer-%s from another mesh member and join this machine again as a new peer",
		e.Configured, e.Current, peer, peer,
	)
}

func canonicalHostname(value string) string {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	return strings.TrimSuffix(value, ".local")
}

func ValidateHostname(cfg Config) error {
	value, err := hostnameProvider()
	if err != nil {
		return fmt.Errorf("determine current hostname: %w", err)
	}
	current := canonicalHostname(value)
	if current == "" {
		return errors.New("determine current hostname: hostname is empty")
	}
	if cfg.Hostname != current {
		return &HostnameMismatchError{Configured: cfg.Hostname, Current: current, PeerID: cfg.PeerID}
	}
	return nil
}

func Path(repository string) string {
	return filepath.Join(repository, Directory, FileName)
}

func Load(repository string) (Config, error) {
	repository, err := ResolveRepository(repository)
	if err != nil {
		return Config{}, err
	}
	if err := migrateLegacyState(repository); err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(Path(repository))
	if err != nil {
		return Config{}, fmt.Errorf("read DFS configuration: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode DFS configuration: %w", err)
	}
	cfg.Repository = repository
	upgraded := false
	if cfg.Version < 4 {
		cfg.Version = 4
		upgraded = true
	}
	if cfg.Hostname == "" {
		hostname, hostnameErr := hostnameProvider()
		if hostnameErr != nil {
			return Config{}, fmt.Errorf("determine hostname while upgrading DFS identity: %w", hostnameErr)
		}
		cfg.Hostname = canonicalHostname(hostname)
		if cfg.Hostname == "" {
			return Config{}, errors.New("determine hostname while upgrading DFS identity: hostname is empty")
		}
		upgraded = true
	}
	if cfg.PeerID == "" {
		cfg.PeerID = randomID()
		upgraded = true
	}
	if cfg.NetworkName == "" {
		cfg.NetworkName = filepath.Base(filepath.Clean(repository))
		upgraded = true
	}
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = 30 * time.Second
		upgraded = true
	}
	if upgraded {
		if err := Save(cfg); err != nil {
			return Config{}, fmt.Errorf("upgrade DFS configuration: %w", err)
		}
	}
	if err := ValidateHostname(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		// The operating system random source is expected to be available on all
		// supported platforms. Keep configuration creation total if it is not;
		// the timestamp keeps this fallback distinct enough to be replaced later.
		return fmt.Sprintf("peer-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", value[:])
}

func Save(cfg Config) error {
	if cfg.Repository == "" {
		return errors.New("repository path is empty")
	}
	repository, err := filepath.Abs(cfg.Repository)
	if err != nil {
		return err
	}
	cfg.Repository = repository
	if err := os.MkdirAll(filepath.Join(repository, Directory), 0o700); err != nil {
		return fmt.Errorf("create DFS state directory: %w", err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(Path(repository), b, 0o600); err != nil {
		return fmt.Errorf("write DFS configuration: %w", err)
	}
	return nil
}

func ResolveRepository(repository string) (string, error) {
	if repository == "" {
		repository = os.Getenv("DFS_REPO")
	}
	if repository != "" {
		return filepath.Abs(repository)
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(Path(current)); err == nil {
			return current, nil
		}
		if _, err := os.Stat(filepath.Join(current, LegacyDirectory, FileName)); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", errors.New("not inside a DFS repository; pass --repo or set DFS_REPO")
}

func migrateLegacyState(repository string) error {
	legacy := filepath.Join(repository, LegacyDirectory)
	destination := filepath.Join(repository, filepath.FromSlash(Directory))
	if _, err := os.Stat(filepath.Join(destination, FileName)); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect DFS state directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, FileName)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect legacy DFS state directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("prepare DFS state migration: %w", err)
	}
	if err := os.Rename(legacy, destination); err != nil {
		return fmt.Errorf("move DFS state from %s to %s: %w", legacy, destination, err)
	}
	return nil
}

func ParseSize(value string) (int64, error) {
	v := strings.TrimSpace(strings.ToUpper(value))
	if v == "" {
		return 0, errors.New("size cannot be empty")
	}
	units := []struct {
		suffix string
		value  int64
	}{
		{"TIB", 1 << 40}, {"TB", 1_000_000_000_000},
		{"GIB", 1 << 30}, {"GB", 1_000_000_000},
		{"MIB", 1 << 20}, {"MB", 1_000_000},
		{"KIB", 1 << 10}, {"KB", 1_000},
		{"B", 1},
	}
	for _, unit := range units {
		if strings.HasSuffix(v, unit.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(v, unit.suffix))
			f, err := strconv.ParseFloat(number, 64)
			if err != nil || f < 0 {
				return 0, fmt.Errorf("invalid size %q", value)
			}
			return int64(f * float64(unit.value)), nil
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return n, nil
}

func FormatSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	for _, unit := range []struct {
		name  string
		value int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n >= unit.value {
			return fmt.Sprintf("%.1f %s", float64(n)/float64(unit.value), unit.name)
		}
	}
	return fmt.Sprintf("%d B", n)
}
