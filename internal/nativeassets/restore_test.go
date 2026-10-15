package nativeassets

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreChecksContentPermissionsAndSymlinks(t *testing.T) {
	directory := t.TempDir()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	writer.Write([]byte("worker-content"))
	writer.Close()
	name := "worker"
	path := filepath.Join(directory, name)
	for _, corrupt := range []string{"missing", "content", "mode", "symlink"} {
		os.Remove(path)
		switch corrupt {
		case "content":
			os.WriteFile(path, []byte("worker-changed"), 0700)
		case "mode":
			os.WriteFile(path, []byte("worker-content"), 0600)
		case "symlink":
			target := filepath.Join(directory, "target")
			os.WriteFile(target, []byte("untouched"), 0600)
			os.Symlink(target, path)
		}
		if err := restore(directory, name, compressed.Bytes(), 1024, 0700); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(path)
		if err != nil || string(content) != "worker-content" {
			t.Fatal("Invalid restored content", err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 {
			t.Fatal("Invalid restored permissions", err)
		}
	}
	if err := restore(directory, name, compressed.Bytes(), 1, 0700); err == nil {
		t.Fatal("Oversize embedded asset accepted")
	}
	target, _ := os.ReadFile(filepath.Join(directory, "target"))
	if string(target) != "untouched" {
		t.Fatal("Recovery overwrote a symlink target")
	}
}
