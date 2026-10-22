package nativeprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kserksi/Pebril/internal/nativeinstall"
)

func TestRunDoesNotDownloadMissingRuntime(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	upstream := nativeinstall.Vips
	defer func() { nativeinstall.Vips = upstream }()
	nativeinstall.Vips.URL = server.URL
	directory := t.TempDir()
	child := filepath.Join(directory, "reject-probe")
	if err := os.WriteFile(child, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), child, directory, false)
	if err == nil || result.Ready || !strings.Contains(err.Error(), "pebril.sh") {
		t.Fatal("Missing deployment must fail with script guidance", result, err)
	}
	if requests.Load() != 0 {
		t.Fatal("Service attempted a runtime download")
	}
	if _, err = os.Lstat(filepath.Join(directory, "vips-current")); !os.IsNotExist(err) {
		t.Fatal("Offline check changed runtime selection", err)
	}
}

func TestCheckRejectsUnreviewedLoadedLibrary(t *testing.T) {
	directory := t.TempDir()
	library := filepath.Join(directory, "libvips.so.42")
	if err := os.WriteFile(library, []byte("old-or-replaced-runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(Result{Ready: true, Raster: true, SVG: true, GIF: true,
		VipsVersion: "8.18.7", VipsPath: library})
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(directory, "probe")
	if err := os.WriteFile(child, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s' '%s'\n", response)), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = Check(context.Background(), child, directory, library, false)
	if err == nil || !strings.Contains(err.Error(), "security-reviewed artifact") {
		t.Fatal("Unreviewed library reached the conversion worker", err)
	}
}
