package nativeinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sum(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
func pack(t *testing.T, entries []struct {
	name, body string
	kind       byte
}) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tarWriter := tar.NewWriter(gz)
	for _, entry := range entries {
		if e := tarWriter.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.body)), Typeflag: entry.kind}); e != nil {
			t.Fatal(e)
		}
		if _, e := tarWriter.Write([]byte(entry.body)); e != nil {
			t.Fatal(e)
		}
	}
	if e := tarWriter.Close(); e != nil {
		t.Fatal(e)
	}
	if e := gz.Close(); e != nil {
		t.Fatal(e)
	}
	return data.Bytes()
}
func fixture(t *testing.T, body string) ([]byte, Artifact) {
	t.Helper()
	data := pack(t, []struct {
		name, body string
		kind       byte
	}{{"lib/libvips.so.42", body, tar.TypeReg}})
	return data, Artifact{ID: "test", SHA256: sum(data), LibrarySHA256: sum([]byte(body)), Members: map[string]string{"lib/libvips.so.42": "libvips.so.42"}}
}
func assertNoStage(t *testing.T, dir string) {
	t.Helper()
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pebril-install-") || strings.HasPrefix(entry.Name(), ".pebril-current-") {
			t.Fatal("Staging residue", entry.Name())
		}
	}
}
func TestConcurrentInstallAndRepair(t *testing.T) {
	data, artifact := fixture(t, "valid-library")
	var downloads atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downloads.Add(1); _, _ = w.Write(data) }))
	defer server.Close()
	artifact.URL = server.URL
	dir := t.TempDir()
	verify := func(p string) error {
		raw, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		if string(raw) != "valid-library" {
			return errors.New("Verification failed")
		}
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := install(context.Background(), dir, artifact, server.Client(), verify); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if downloads.Load() != 1 {
		t.Fatal("Concurrent installations downloaded more than once", downloads.Load())
	}
	assertNoStage(t, dir)
	active, e := Active(dir, artifact)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(active, []byte("corrupted"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Active(dir, artifact); e == nil {
		t.Fatal("Corrupt files must be rejected")
	}
	if _, e = install(context.Background(), dir, artifact, server.Client(), verify); e != nil {
		t.Fatal(e)
	}
	if downloads.Load() != 2 {
		t.Fatal(downloads.Load())
	}
	assertNoStage(t, dir)
}
func TestFailedUpdatePreservesCurrent(t *testing.T) {
	data, first := fixture(t, "first")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	first.URL = server.URL
	dir := t.TempDir()
	ok := func(string) error { return nil }
	active, e := install(context.Background(), dir, first, server.Client(), ok)
	if e != nil {
		t.Fatal(e)
	}
	secondData, second := fixture(t, "second")
	secondServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(secondData) }))
	defer secondServer.Close()
	second.URL = secondServer.URL
	if _, e = install(context.Background(), dir, second, secondServer.Client(), func(string) error { return errors.New("Capability self-test failed") }); e == nil {
		t.Fatal("Failed self-tests must be rejected")
	}
	retained, e := Active(dir, first)
	if e != nil || retained != active {
		t.Fatal("Failed update replaced the original library", e)
	}
	assertNoStage(t, dir)
}
func TestDownloadAndArchiveFailures(t *testing.T) {
	valid, base := fixture(t, "valid")
	cases := []struct {
		name    string
		data    []byte
		badHash bool
		status  int
	}{
		{"sha256", valid, true, 200}, {"http", valid, false, 503},
		{"traversal", pack(t, []struct {
			name, body string
			kind       byte
		}{{"../escaped", "bad", tar.TypeReg}}), false, 200},
		{"missing", pack(t, []struct {
			name, body string
			kind       byte
		}{{"other", "bad", tar.TypeReg}}), false, 200},
		{"duplicate", pack(t, []struct {
			name, body string
			kind       byte
		}{{"lib/libvips.so.42", "valid", tar.TypeReg}, {"lib/libvips.so.42", "valid", tar.TypeReg}}), false, 200},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			artifact := base
			artifact.SHA256 = sum(test.data)
			if test.badHash {
				artifact.SHA256 = strings.Repeat("0", 64)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); _, _ = w.Write(test.data) }))
			defer server.Close()
			artifact.URL = server.URL
			dir := t.TempDir()
			if _, e := install(context.Background(), dir, artifact, server.Client(), func(string) error { return nil }); e == nil {
				t.Fatal("Expected rejection")
			}
			assertNoStage(t, dir)
			if _, e := os.Lstat(filepath.Join(dir, currentName)); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("Failed installation published a pointer")
			}
		})
	}
}
func TestCancelledDownload(t *testing.T) {
	_, artifact := fixture(t, "valid")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	artifact.URL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	dir := t.TempDir()
	if _, e := install(ctx, dir, artifact, server.Client(), func(string) error { return nil }); e == nil {
		t.Fatal("Cancellation must fail")
	}
	assertNoStage(t, dir)
}
func TestRedirectPolicy(t *testing.T) {
	for _, raw := range []string{"http://github.com/file", "https://evil.example/file"} {
		request, e := http.NewRequest("GET", raw, nil)
		if e != nil {
			t.Fatal(e)
		}
		if client().CheckRedirect(request, nil) == nil {
			t.Fatal("Unapproved redirect was allowed", raw)
		}
	}
}
func TestLargeEntryAndSelectedSymlink(t *testing.T) {
	for _, kind := range []byte{tar.TypeReg, tar.TypeSymlink} {
		var data bytes.Buffer
		gz := gzip.NewWriter(&data)
		writer := tar.NewWriter(gz)
		header := &tar.Header{Name: "lib/libvips.so.42", Typeflag: kind, Mode: 0600, Linkname: "/etc/passwd"}
		if kind == tar.TypeReg {
			header.Size = maxExpanded + 1
		}
		if e := writer.WriteHeader(header); e != nil {
			t.Fatal(e)
		}
		_ = writer.Close()
		_ = gz.Close()
		dir := t.TempDir()
		archive := filepath.Join(dir, "input.gz")
		if e := os.WriteFile(archive, data.Bytes(), 0600); e != nil {
			t.Fatal(e)
		}
		if e := extract(archive, dir, map[string]string{"lib/libvips.so.42": "libvips.so.42"}); e == nil {
			t.Fatal("Oversized entries and symlinks must be rejected")
		}
	}
}

func TestUnownedPreviousDirectoryPreserved(t *testing.T) {
	data, artifact := fixture(t, "valid")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	artifact.URL = server.URL
	dir := t.TempDir()
	previous := filepath.Join(dir, "pebril-vips-user")
	if e := os.Mkdir(previous, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(previous, "user-file"), []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Base(previous), filepath.Join(dir, currentName)); e != nil {
		t.Fatal(e)
	}
	if _, e := install(context.Background(), dir, artifact, server.Client(), func(string) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if data, e := os.ReadFile(filepath.Join(previous, "user-file")); e != nil || string(data) != "keep" {
		t.Fatal("An unmanaged directory was removed", e)
	}
	assertNoStage(t, dir)
}
func TestCancellationBeforePublication(t *testing.T) {
	data, artifact := fixture(t, "valid")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	artifact.URL = server.URL
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, e := install(ctx, dir, artifact, server.Client(), func(string) error { cancel(); return nil }); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e := os.Lstat(filepath.Join(dir, currentName)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("Publication occurred after cancellation", e)
	}
	assertNoStage(t, dir)
}

func TestNestedNoticesAndManifestPathValidation(t *testing.T) {
	data := pack(t, []struct {
		name, body string
		kind       byte
	}{
		{"runtime/lib/libvips.so.42", "library", tar.TypeReg},
		{"runtime/licenses/subcomponent/LICENSE", "license", tar.TypeReg},
		{"runtime/DISTRIBUTION-NOTICE.txt", "source companion notice", tar.TypeReg},
	})
	directory := t.TempDir()
	archive := filepath.Join(directory, "runtime.tar.gz")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	members := map[string]string{
		"runtime/lib/libvips.so.42": "libvips.so.42",

		"runtime/DISTRIBUTION-NOTICE.txt": "DISTRIBUTION-NOTICE.txt",
	}
	if err := extract(archive, directory, members, "runtime/licenses/"); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(directory, "licenses/subcomponent/LICENSE")); err != nil || string(body) != "license" {
		t.Fatal("Nested notice was not retained", err)
	}
	for _, target := range []string{"../escape", "/absolute", "licenses/../escape"} {
		if err := extract(archive, t.TempDir(), map[string]string{"runtime/lib/libvips.so.42": target}); err == nil {
			t.Fatal("Invalid manifest destination accepted", target)
		}
	}
}

func TestLicenseSubtreeRejectsSymlinks(t *testing.T) {
	data := pack(t, []struct {
		name, body string
		kind       byte
	}{{"runtime/licenses/linked", "", tar.TypeSymlink}})
	directory := t.TempDir()
	archive := filepath.Join(directory, "runtime.tar.gz")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := extract(archive, directory, nil, "runtime/licenses/"); err == nil {
		t.Fatal("License subtree symlink accepted")
	}
}

func TestRetiredRuntimeIsUnavailable(t *testing.T) {
	for _, name := range []string{"pebril", "https://example.com/arbitrary"} {
		if _, ok := Source(name); ok {
			t.Fatal("Unreviewed source accepted", name)
		}
	}
}

func TestReviewedChecksActualLibraryBytes(t *testing.T) {
	library := filepath.Join(t.TempDir(), "libvips.so.42")
	if err := os.WriteFile(library, []byte("reviewed-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := Vips
	defer func() { Vips = previous }()
	Vips.LibrarySHA256 = sum([]byte("reviewed-fixture"))
	if err := Reviewed(library); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(library, []byte("replaced-library"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Reviewed(library); err == nil {
		t.Fatal("Modified library accepted")
	}
}

func TestFinalPathFailurePreservesPreviousRuntime(t *testing.T) {
	data, artifact := fixture(t, "valid")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	artifact.URL = server.URL
	directory := t.TempDir()
	_, err := install(context.Background(), directory, artifact, server.Client(), func(library string) error {
		if strings.HasPrefix(filepath.Base(filepath.Dir(library)), "pebril-vips-") {
			return errors.New("Final path rejected")
		}
		return nil
	})
	if err == nil {
		t.Fatal("Final-path validation failure was accepted")
	}
	if _, err = os.Lstat(filepath.Join(directory, currentName)); !os.IsNotExist(err) {
		t.Fatal("Invalid runtime was selected")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pebril-vips-") {
			t.Fatal("Invalid generation was retained")
		}
	}
	assertNoStage(t, directory)
}
