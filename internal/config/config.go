package config

import (
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
	Name         string        `json:"name"`
	Repository   string        `json:"repository"`
	CacheLimit   int64         `json:"cache_limit_bytes"`
	SyncInterval time.Duration `json:"sync_interval"`
	Relay        string        `json:"relay,omitempty"`
}

func Default(name, repository string) Config {
	return Config{
		Version:      1,
		Name:         name,
		Repository:   repository,
		CacheLimit:   100 * 1024 * 1024 * 1024,
		SyncInterval: 30 * time.Second,
	}
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
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = 30 * time.Second
	}
	return cfg, nil
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
