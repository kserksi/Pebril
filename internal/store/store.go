package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	DB      *sql.DB
	lock    *os.File
	Started time.Time
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "pebril.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("Data directory is already used by another instance")
	}
	s := &Store{lock: lock, Started: time.Now()}
	fail := func(e error) (*Store, error) { s.Close(); return nil, e }
	// Serialize opening and creation permissions to prevent permissive database or shared-memory files.
	dbPath := filepath.Join(dir, "pebril.db")
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	if err = f.Close(); err != nil {
		return fail(err)
	}
	s.DB, err = sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL"}).String())
	if err != nil {
		return fail(err)
	}
	s.DB.SetMaxOpenConns(1)
	if err = s.DB.Ping(); err != nil {
		return fail(err)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);`)
	if err != nil {
		return fail(err)
	}
	var version int
	err = tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return fail(err)
	}
	if version > 1 {
		return fail(fmt.Errorf("Database schema version %d exceeds supported version 1", version))
	}
	if version == 0 {
		_, err = tx.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(1,?)`, time.Now().UTC().UnixMilli())
		if err != nil {
			return fail(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Store) Ping() error { return s.DB.Ping() }
func (s *Store) Close() error {
	var err error
	if s.DB != nil {
		err = s.DB.Close()
	}
	if s.lock != nil {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		if e := s.lock.Close(); err == nil {
			err = e
		}
	}
	return err
}
