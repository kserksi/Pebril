package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/store"
)

func TestResetRequiresExistingInitializedDatabase(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	if existingResetDatabase(missing) == nil {
		t.Fatal("Missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("Reset created missing directory")
	}
	blank := filepath.Join(root, "blank")
	if err := os.Mkdir(blank, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blank, "pebril.db"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if existingResetDatabase(blank) == nil {
		t.Fatal("Blank database accepted")
	}
	if entries, err := os.ReadDir(blank); err != nil || len(entries) != 1 {
		t.Fatalf("Blank database was initialized: %v %v", entries, err)
	}
	if info, err := os.Stat(filepath.Join(blank, "pebril.db")); err != nil || info.Size() != 0 {
		t.Fatal("Blank database was modified")
	}
	dir := filepath.Join(root, "initialized")
	database, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if existingResetDatabase(dir) == nil {
		t.Fatal("Database without an administrator accepted")
	}
	service, err := auth.New(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Initialize("administrator", "old password"); err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	if err = existingResetDatabase(dir); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked")
	if err = os.Symlink(dir, linked); err != nil {
		t.Fatal(err)
	}
	if existingResetDatabase(linked) == nil {
		t.Fatal("Symbolic-link database directory accepted")
	}
	for _, name := range []string{"pebril.db", "pebril.lock"} {
		path := filepath.Join(dir, name)
		backup := path + ".original"
		if err = os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(backup, path); err != nil {
			t.Fatal(err)
		}
		if existingResetDatabase(dir) == nil {
			t.Fatalf("Symbolic-link %s accepted", name)
		}
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	if existingResetDatabase(dir) == nil {
		t.Fatal("Group-writable database directory accepted")
	}
}

func TestResetRefusesRedirectedPasswordInput(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "password-input-")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.CreateTemp(t.TempDir(), "password-output-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if password, err := resetPassword(input, output); err == nil || password != nil {
		t.Fatal("Redirected password input accepted")
	}
	if info, err := output.Stat(); err != nil || info.Size() != 0 {
		t.Fatal("Password was prompted before terminal validation")
	}
}
