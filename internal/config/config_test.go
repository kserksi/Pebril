package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testDeployment(t *testing.T) Deployment {
	t.Helper()
	d, e := LoadDeployment(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestValidationAndSnapshot(t *testing.T) {
	d := testDeployment(t)
	m, e := New(d, true)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	first := m.Snapshot()
	first.Policy.UploadFormats["png"] = false
	if !m.Snapshot().Policy.UploadFormats["png"] {
		t.Fatal("Snapshot exposes a mutable map")
	}
	path := filepath.Join(d.Root, "config.json")
	for _, text := range []string{`{"static_quality":101}`, `{"static_quality":null}`, `{"static_quality":80,"static_quality":20}`, `{"new_user_quota":1}`, `{"upload_formats":{"avif":true}}`, `{} {}`, `[]`, `{"preserve_animation":false}`} {
		if e = os.WriteFile(path, []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
		if e = m.Reload(); e == nil {
			t.Fatalf("Expected rejection of %s", text)
		}
		if got := m.Snapshot(); got.Revision != 1 || got.Policy.StaticQuality != 80 {
			t.Fatal("Invalid configuration replaced the valid snapshot")
		}
	}
	if e = os.WriteFile(path, []byte(`{"static_quality":75}`), 0600); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := m.Reload(); e != nil {
				t.Error(e)
			}
			_ = m.Snapshot()
		}()
	}
	wg.Wait()
	if m.Snapshot().Revision != 13 {
		t.Fatal("Concurrent update was lost")
	}
}
func TestWatchAtomicRename(t *testing.T) {
	d := testDeployment(t)
	m, e := New(d, true)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	done := make(chan error, 10)
	if e = m.Watch(func(e error) { done <- e }); e != nil {
		t.Fatal(e)
	}
	temp := filepath.Join(d.Root, "new.json")
	if e = os.WriteFile(temp, []byte(`{"static_quality":72}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(temp, filepath.Join(d.Root, "config.json")); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Atomic replacement was not detected")
	}
	if m.Snapshot().Policy.StaticQuality != 72 {
		t.Fatal("Configuration was not applied")
	}
}
func TestDeploymentPathsAndAVIF(t *testing.T) {
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, ".env"), []byte("PEBRIL_DATA_DIR=custom\nPEBRIL_AVIF=true\n"), 0600); e != nil {
		t.Fatal(e)
	}
	d, e := LoadDeployment(root)
	if e != nil {
		t.Fatal(e)
	}
	if d.DBDir != filepath.Join(root, "custom/db") || !d.AVIF {
		t.Fatal(d)
	}
	t.Setenv("PEBRIL_LISTEN", "127.0.0.1:abc")
	if _, e = LoadDeployment(root); e == nil {
		t.Fatal("Invalid port was accepted")
	}
	t.Setenv("PEBRIL_LISTEN", "127.0.0.1:8080")
	t.Setenv("PEBRIL_PUBLIC_DIR", "same")
	t.Setenv("PEBRIL_PRIVATE_DIR", "same/nested")
	if _, e = LoadDeployment(root); e == nil {
		t.Fatal("Overlapping paths were accepted")
	}
}
