package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreLockMigrationAndFutureVersion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "\u542b \u7a7a\u683c?#")
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	var journal string
	if e = s.DB.QueryRow("PRAGMA journal_mode").Scan(&journal); e != nil || journal != "wal" {
		t.Fatal(journal, e)
	}
	var foreign int
	if e = s.DB.QueryRow("PRAGMA foreign_keys").Scan(&foreign); e != nil || foreign != 1 {
		t.Fatal(foreign, e)
	}
	if other, e := Open(dir); e == nil {
		other.Close()
		t.Fatal("Multiple instances were allowed")
	}
	info, e := os.Stat(filepath.Join(dir, "pebril.db"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("Database permissions", e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	var count int
	if e = s.DB.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); e != nil || count != 2 {
		t.Fatal("Migration is not idempotent", e)
	}
	if _, e = s.DB.Exec("INSERT INTO schema_migrations VALUES(3,0)"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if other, e := Open(dir); e == nil {
		other.Close()
		t.Fatal("Future database schema was not rejected")
	}
}
