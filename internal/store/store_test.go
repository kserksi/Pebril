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
	if e = s.DB.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); e != nil || count != 4 {
		t.Fatal("Migration is not idempotent", e)
	}
	if _, e = s.DB.Exec("INSERT INTO schema_migrations VALUES(5,0)"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if other, e := Open(dir); e == nil {
		other.Close()
		t.Fatal("Future database schema was not rejected")
	}
}

func TestUpgradeVersionTwoPreservesRecordsAndCharges(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO users(id,username,password,quota,used) VALUES(1,'fixture',x'00',1024,20);
 INSERT INTO images(id,user_id,state,policy,created_at,hash,bytes,format,frames) VALUES('original',1,'queued','{"revision":1,"policy":{}}',0,'input',8,'png',1),('safe',1,'completed','{"revision":1,"policy":{}}',0,'output',12,'svg',1);
 DROP TABLE image_files; DROP INDEX conversion_schedule;
 ALTER TABLE images DROP COLUMN generation; ALTER TABLE images DROP COLUMN attempts; ALTER TABLE images DROP COLUMN next_run; ALTER TABLE images DROP COLUMN fallback;
 ALTER TABLE images DROP COLUMN delivered; DROP TABLE deployment; DELETE FROM schema_migrations WHERE version>=3;`)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	original, err := s.Image("original")
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.Image("safe")
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.User(1)
	if err != nil {
		t.Fatal(err)
	}
	if FileRole(original.Files, "original").Hash != "input" || FileRole(completed.Files, "result").Hash != "output" || completed.URL != "/image/safe" || user.Used != 20 {
		t.Fatal("Migration changed retained records or storage charges")
	}
}
