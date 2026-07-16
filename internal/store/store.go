package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Access struct {
	Path       string
	LastAccess time.Time
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS access (path TEXT PRIMARY KEY, last_access INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS pins (path TEXT PRIMARY KEY, created_at INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize state database: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func normalize(path string) string {
	path = filepath.ToSlash(filepath.Clean(path))
	path = strings.TrimPrefix(path, "./")
	if path == "." {
		return ""
	}
	return strings.TrimPrefix(path, "/")
}

func (s *Store) Touch(path string) error {
	_, err := s.db.Exec(`INSERT INTO access(path,last_access) VALUES(?,?)
		ON CONFLICT(path) DO UPDATE SET last_access=excluded.last_access`, normalize(path), time.Now().UnixNano())
	return err
}

func (s *Store) Pin(path string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO pins(path,created_at) VALUES(?,?)`, normalize(path), time.Now().UnixNano())
	return err
}

func (s *Store) Unpin(path string) error {
	_, err := s.db.Exec(`DELETE FROM pins WHERE path=?`, normalize(path))
	return err
}

func (s *Store) Pins() ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM pins ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		result = append(result, path)
	}
	return result, rows.Err()
}

func (s *Store) IsPinned(path string) (bool, error) {
	path = normalize(path)
	rows, err := s.db.Query(`SELECT path FROM pins`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var pin string
		if err := rows.Scan(&pin); err != nil {
			return false, err
		}
		if pin == "" || path == pin || strings.HasPrefix(path, pin+"/") {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) LastAccess(path string) time.Time {
	var ns int64
	if err := s.db.QueryRow(`SELECT last_access FROM access WHERE path=?`, normalize(path)).Scan(&ns); err != nil {
		return time.Time{}
	}
	return time.Unix(0, ns)
}
