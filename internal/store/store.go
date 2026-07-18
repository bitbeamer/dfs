package store

import (
	"database/sql"
	"errors"
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

type FileMetadata struct {
	Mode      uint32
	UID       uint32
	GID       uint32
	AtimeNS   int64
	MtimeNS   int64
	CtimeNS   int64
	Signature string
}

var (
	ErrXAttrExists   = errors.New("extended attribute already exists")
	ErrXAttrNotFound = errors.New("extended attribute not found")
)

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
		`CREATE TABLE IF NOT EXISTS file_metadata (
			path TEXT PRIMARY KEY, mode INTEGER NOT NULL, uid INTEGER NOT NULL, gid INTEGER NOT NULL,
			atime_ns INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, ctime_ns INTEGER NOT NULL,
			signature TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS xattrs (
			path TEXT NOT NULL, name TEXT NOT NULL, value BLOB NOT NULL,
			PRIMARY KEY(path, name)
		)`,
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

func (s *Store) SaveFileMetadata(path string, metadata FileMetadata) error {
	_, err := s.db.Exec(`INSERT INTO file_metadata(path,mode,uid,gid,atime_ns,mtime_ns,ctime_ns,signature)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET
		mode=excluded.mode,uid=excluded.uid,gid=excluded.gid,atime_ns=excluded.atime_ns,
		mtime_ns=excluded.mtime_ns,ctime_ns=excluded.ctime_ns,signature=excluded.signature`,
		normalize(path), metadata.Mode, metadata.UID, metadata.GID, metadata.AtimeNS,
		metadata.MtimeNS, metadata.CtimeNS, metadata.Signature)
	return err
}

func (s *Store) FileMetadata(path string) (FileMetadata, bool, error) {
	var metadata FileMetadata
	err := s.db.QueryRow(`SELECT mode,uid,gid,atime_ns,mtime_ns,ctime_ns,signature
		FROM file_metadata WHERE path=?`, normalize(path)).Scan(
		&metadata.Mode, &metadata.UID, &metadata.GID, &metadata.AtimeNS,
		&metadata.MtimeNS, &metadata.CtimeNS, &metadata.Signature,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return FileMetadata{}, false, nil
	}
	return metadata, err == nil, err
}

func (s *Store) SetXAttr(path, name string, value []byte, flags int) error {
	path = normalize(path)
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM xattrs WHERE path=? AND name=?`, path, name).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if flags&1 != 0 && err == nil {
		return ErrXAttrExists
	}
	if flags&2 != 0 && errors.Is(err, sql.ErrNoRows) {
		return ErrXAttrNotFound
	}
	_, err = s.db.Exec(`INSERT INTO xattrs(path,name,value) VALUES(?,?,?)
		ON CONFLICT(path,name) DO UPDATE SET value=excluded.value`, path, name, value)
	return err
}

func (s *Store) XAttr(path, name string) ([]byte, error) {
	var value []byte
	err := s.db.QueryRow(`SELECT value FROM xattrs WHERE path=? AND name=?`, normalize(path), name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrXAttrNotFound
	}
	return value, err
}

func (s *Store) ListXAttrs(path string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM xattrs WHERE path=? ORDER BY name`, normalize(path))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s *Store) RemoveXAttr(path, name string) error {
	result, err := s.db.Exec(`DELETE FROM xattrs WHERE path=? AND name=?`, normalize(path), name)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrXAttrNotFound
	}
	return nil
}

func (s *Store) RenameFileState(oldPath, newPath string) error {
	oldPath, newPath = normalize(oldPath), normalize(newPath)
	if oldPath == newPath {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	newPrefix := newPath + "/"
	oldPrefix := oldPath + "/"
	for _, statement := range []string{
		`DELETE FROM file_metadata WHERE path=? OR substr(path,1,?)=?`,
		`DELETE FROM xattrs WHERE path=? OR substr(path,1,?)=?`,
	} {
		if _, err := tx.Exec(statement, newPath, len(newPrefix), newPrefix); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE file_metadata SET path=? || substr(path,?)
		WHERE path=? OR substr(path,1,?)=?`, newPath, len(oldPath)+1, oldPath, len(oldPrefix), oldPrefix); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE xattrs SET path=? || substr(path,?)
		WHERE path=? OR substr(path,1,?)=?`, newPath, len(oldPath)+1, oldPath, len(oldPrefix), oldPrefix); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveFileState(path string) error {
	path = normalize(path)
	prefix := path + "/"
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM file_metadata WHERE path=? OR substr(path,1,?)=?`, path, len(prefix), prefix); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM xattrs WHERE path=? OR substr(path,1,?)=?`, path, len(prefix), prefix); err != nil {
		return err
	}
	return tx.Commit()
}
