package server

import (
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusHealthAndAssets(t *testing.T) {
	d, e := config.LoadDeployment(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, e := config.New(d, true)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	db, e := store.Open(d.DBDir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	h := Handler("0.0.1-dev", m, db, nativeprobe.Result{Ready: true, Raster: true, SVG: true, GIF: true, VipsVersion: "8.16.1"})
	for _, path := range []string{"/", "/app.js", "/style.css", "/healthz", "/api/v1/status"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(path, r.Code)
		}
		if path == "/api/v1/status" && (strings.Contains(r.Body.String(), d.Root) || !strings.Contains(r.Body.String(), "conversion")) {
			t.Fatal("Status exposed paths or reported the wrong stage")
		}
	}
	db.Close()
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/healthz", nil))
	if r.Code != 503 {
		t.Fatal("Readiness was not revoked after database failure")
	}
}
