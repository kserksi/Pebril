package nativeprobe

import (
	"context"
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
	upstream, pebril := nativeinstall.Vips, nativeinstall.PebrilVips
	defer func() { nativeinstall.Vips, nativeinstall.PebrilVips = upstream, pebril }()
	nativeinstall.Vips.URL, nativeinstall.PebrilVips.URL = server.URL, server.URL
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
