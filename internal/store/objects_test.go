package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestObjectGenerationsPersistAndRetireOnlyUnreferencedRepresentations(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	generation, err := s.AcquireObjectKey(hash)
	if err != nil || generation != 1 {
		t.Fatal(generation, err)
	}
	if retired, e := s.RetireObjectKey(hash, 1); e != nil || !retired {
		t.Fatal(retired, e)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.ResolveObjectKey(hash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("Retired object remained active", err)
	}
	generation, err = s.AcquireObjectKey(hash)
	if err != nil || generation != 2 {
		t.Fatal("Retirement counter was lost across restart", generation, err)
	}
	if _, err = s.RetireObjectKey(hash, 1); err != nil {
		t.Fatal(err)
	}
	if got, e := s.ResolveObjectKey(hash); e != nil || got != 2 {
		t.Fatal("Late retirement affected another generation", got, e)
	}
	if _, err = s.DB.Exec("INSERT INTO users(id,username,password,quota) VALUES(1,'owner',x'00',1024)"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("INSERT INTO images(id,user_id,state,policy,created_at,hash) VALUES('record',1,'completed','{}',0,?)", hash); err != nil {
		t.Fatal(err)
	}
	if retired, e := s.RetireObjectKey(hash, 2); e != nil || retired {
		t.Fatal("Live references were retired", retired, e)
	}
	if _, err = s.DB.Exec("DELETE FROM images"); err != nil {
		t.Fatal(err)
	}
	if retired, e := s.RetireObjectKey(hash, 2); e != nil || !retired {
		t.Fatal(retired, e)
	}
	if _, err = s.RetireObjectKey(hash, 10); err != nil {
		t.Fatal(err)
	}
	if got, e := s.AcquireObjectKey(hash); e != nil || got != 11 {
		t.Fatal("Observed retired generation was reused", got, e)
	}
}
func TestLegacyObjectAndStorageBinding(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindStorage("local"); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("b", 64)
	if _, err = s.DB.Exec("INSERT INTO users(id,username,password,quota) VALUES(1,'owner',x'00',1024);INSERT INTO images(id,user_id,state,policy,created_at,hash) VALUES('legacy',1,'completed','{}',0,?)", hash); err != nil {
		t.Fatal(err)
	}
	if err = s.BindStorage("s3"); err == nil {
		t.Fatal("Configuration switch was treated as data migration")
	}
	if got, e := s.ResolveObjectKey(hash); e != nil || got != 0 {
		t.Fatal("Legacy hash object was not preserved", got, e)
	}
	if retired, e := s.RetireObjectKey(hash, 0); e != nil || retired {
		t.Fatal("Referenced legacy object was retired", retired, e)
	}
	if _, err = s.DB.Exec("DELETE FROM images"); err != nil {
		t.Fatal(err)
	}
	if retired, e := s.RetireObjectKey(hash, 0); e != nil || !retired {
		t.Fatal(retired, e)
	}
	if got, e := s.AcquireObjectKey(hash); e != nil || got != 1 {
		t.Fatal("Legacy physical name was reused", got, e)
	}
	if err = s.BindStorage("s3"); err != nil {
		t.Fatal("Empty stopped deployment could not select storage", err)
	}
}
