package store

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// The stopped source remains read-only, including schema-3 data used by the old executable.
func OpenMigrationSource(dir string) (*Store, int, error) {
	path := filepath.Join(dir, "pebril.db")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, errors.New("Migration needs an existing regular SQLite source database")
	}
	lock, err := localLock(dir)
	if err != nil {
		return nil, 0, err
	}
	s := &Store{lock: lock}
	fail := func() (*Store, int, error) {
		s.Close()
		return nil, 0, errors.New("Cannot read a supported SQLite source schema")
	}
	s.DB, err = sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_query_only=1&_busy_timeout=5000"}).String())
	if err != nil {
		return fail()
	}
	s.DB.SetMaxOpenConns(1)
	var version int
	if err = s.DB.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version < 3 || version > 5 {
		return fail()
	}
	if version >= 4 {
		if err = s.DB.QueryRow("SELECT namespace FROM deployment WHERE id=1").Scan(&s.namespace); err != nil {
			return fail()
		}
	}
	rows, err := s.DB.Query("PRAGMA foreign_key_check")
	if err != nil {
		return fail()
	}
	invalid := rows.Next()
	readErr := rows.Err()
	rows.Close()
	if invalid || readErr != nil {
		return fail()
	}
	return s, version, nil
}

func (s *Store) SetMigrationNamespace(namespace string) error {
	if s.namespace == namespace {
		return nil
	}
	if len(namespace) != 32 {
		return errors.New("Invalid source namespace identity")
	}
	for _, character := range namespace {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return errors.New("Invalid source namespace identity")
		}
	}
	for _, table := range []string{"users", "sessions", "upload_tokens", "images", "image_files", "object_keys"} {
		var count int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("Migration target contains existing business data")
		}
	}
	if _, err := s.DB.Exec("UPDATE deployment SET namespace=? WHERE id=1", namespace); err != nil {
		return err
	}
	s.namespace = namespace
	return nil
}
