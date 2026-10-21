package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/resources"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

type httpResourceState struct{ state resources.State }

func (s *httpResourceState) State() resources.State { return s.state }

type resourceRequestBody struct{ reads int }

func (b *resourceRequestBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (*resourceRequestBody) Close() error               { return nil }

func resourceRequest(h http.Handler, method, path string, session *auth.Session, bearer string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://example.test"+path, body)
	if method == http.MethodPost {
		req.Header.Set("Origin", "https://example.test")
		req.Header.Set("Content-Type", "application/json")
	}
	if session != nil {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		req.Header.Set("X-CSRF-Token", session.CSRF)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	return recorder
}
func resourceJSON(t *testing.T, response *httptest.ResponseRecorder, status int) map[string]json.RawMessage {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func resourcePrivateFieldsAbsent(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	value := resourceJSON(t, response, status)
	for _, key := range []string{"resources", "resource_warnings"} {
		if _, exists := value[key]; exists {
			t.Fatalf("private field %s exposed: %s", key, response.Body.String())
		}
	}
	if strings.Contains(response.Body.String(), "private-resource-warning") {
		t.Fatal("resource diagnostic leaked")
	}
}

func TestResourceHTTPBoundary(t *testing.T) {
	deployment, err := config.LoadDeployment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := config.New(deployment, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	db, err := store.Open(deployment.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	authentication, err := auth.New(db)
	if err != nil {
		t.Fatal(err)
	}
	state := &httpResourceState{resources.State{Concurrency: 1, Cooldown: true, Warnings: []resources.Warning{{Code: "memory-monitor", Message: "private-resource-warning"}}, Memory: &resources.Memory{Used: 96, Limit: 100, Total: 200}}}
	upload, err := uploads.NewWithResources(db, manager, deployment, "/bin/false", nativeprobe.Result{Ready: true}, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(upload.Close)
	capability := nativeprobe.Result{Ready: true}
	handler := HandlerWithUploads("0.0.6-dev", manager, db, capability, authentication, upload)
	username, err := auth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	password, err := auth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	initialize := resourceRequest(handler, "POST", "/api/v1/initialize", nil, "", bytes.NewReader(credentials))
	value := resourceJSON(t, initialize, 200)
	var warnings []resources.Warning
	if err := json.Unmarshal(value["resource_warnings"], &warnings); err != nil || len(warnings) != 1 || warnings[0].Message != "private-resource-warning" {
		t.Fatalf("initial administrator warning missing: %s %v", initialize.Body.String(), err)
	}
	if _, exists := value["resources"]; exists {
		t.Fatal("initialization unexpectedly exposes full resource state")
	}
	admin, err := authentication.Login(username, password)
	if err != nil {
		t.Fatal(err)
	}
	// Ordinary-user login uses the disposable administrator password verifier;
	// the stored user and session remain genuine independent nonadmin records.
	ordinaryName, err := auth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota) SELECT ?,password,quota FROM users WHERE id=?", ordinaryName, admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	ordinaryUser, err := db.User(ordinaryID)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := authentication.NewSession(ordinaryUser)
	if err != nil {
		t.Fatal(err)
	}
	bearer, err := authentication.IssueToken(admin)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("administrator account and login", func(t *testing.T) {
		account := resourceRequest(handler, "GET", "/api/v1/account", &admin, "", nil)
		value := resourceJSON(t, account, 200)
		var got resources.State
		if err := json.Unmarshal(value["resources"], &got); err != nil || !got.Cooldown || got.Memory == nil || got.Memory.Limit != 100 || len(got.Warnings) != 1 {
			t.Fatalf("admin resource contract: %s %v", account.Body.String(), err)
		}
		login := resourceRequest(handler, "POST", "/api/v1/login", nil, "", bytes.NewReader(credentials))
		value = resourceJSON(t, login, 200)
		var gotWarnings []resources.Warning
		if err := json.Unmarshal(value["resource_warnings"], &gotWarnings); err != nil || len(gotWarnings) != 1 {
			t.Fatalf("admin login warning missing: %s %v", login.Body.String(), err)
		}
	})
	t.Run("ordinary anonymous and bearer diagnostics", func(t *testing.T) {
		resourcePrivateFieldsAbsent(t, resourceRequest(handler, "GET", "/api/v1/account", &ordinary, "", nil), 200)
		resourcePrivateFieldsAbsent(t, resourceRequest(handler, "GET", "/api/v1/account", nil, "", nil), 200)
		resourcePrivateFieldsAbsent(t, resourceRequest(handler, "GET", "/api/v1/account", &admin, bearer, nil), 403)
		ordinaryCredentials, err := json.Marshal(map[string]string{"username": ordinaryName, "password": password})
		if err != nil {
			t.Fatal(err)
		}
		resourcePrivateFieldsAbsent(t, resourceRequest(handler, "POST", "/api/v1/login", nil, "", bytes.NewReader(ordinaryCredentials)), 200)
		resourcePrivateFieldsAbsent(t, resourceRequest(handler, "POST", "/api/v1/login", nil, bearer, bytes.NewReader(credentials)), 403)
		for _, session := range []*auth.Session{nil, &ordinary, &admin} {
			status := resourceRequest(handler, "GET", "/api/v1/status", session, "", nil)
			fields := resourceJSON(t, status, 200)
			resourcePrivateFieldsAbsent(t, status, 200)
			if len(fields) != 8 || fields["native"] == nil || fields["version"] == nil || fields["database"] == nil || fields["storage"] == nil {
				t.Fatalf("public status shape changed: %s", status.Body.String())
			}
		}
		resourcePrivateFieldsAbsent(t, resourceRequest(handler, "GET", "/api/v1/status", nil, bearer, nil), 200)
	})
	t.Run("absent resource service", func(t *testing.T) {
		for _, without := range []*uploads.Service{nil, {DB: db, Manager: manager, Deployment: deployment}} {
			h := HandlerWithUploads("0.0.6-dev", manager, db, capability, authentication, without)
			resourcePrivateFieldsAbsent(t, resourceRequest(h, "GET", "/api/v1/account", &admin, "", nil), 200)
			resourcePrivateFieldsAbsent(t, resourceRequest(h, "POST", "/api/v1/login", nil, "", bytes.NewReader(credentials)), 200)
		}
	})
	t.Run("cooldown refuses admission without reading body", func(t *testing.T) {
		for _, authorization := range []struct {
			session *auth.Session
			token   string
		}{{&admin, ""}, {nil, bearer}} {
			body := &resourceRequestBody{}
			response := resourceRequest(handler, "POST", "/api/v1/uploads", authorization.session, authorization.token, body)
			value := resourceJSON(t, response, 429)
			var code string
			if err := json.Unmarshal(value["code"], &code); err != nil || code != "resource_cooldown" {
				t.Fatalf("missing cooldown code: %s %v", response.Body.String(), err)
			}
			if body.reads != 0 {
				t.Fatalf("rejected body read %d times", body.reads)
			}
			var count int
			if err := db.DB.QueryRow("SELECT count(*) FROM images").Scan(&count); err != nil || count != 0 {
				t.Fatalf("cooldown created an image record: %d %v", count, err)
			}
			resourcePrivateFieldsAbsent(t, response, 429)
		}
	})
	t.Run("accepted receipts and public images remain accessible", func(t *testing.T) {
		queuedID, err := uploads.NewID()
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AcceptUpload(queuedID, admin.User.ID, manager.Snapshot()); err != nil {
			t.Fatal(err)
		}
		if err := db.Received(queuedID); err != nil {
			t.Fatal(err)
		}
		receipt := resourceRequest(handler, "GET", "/api/v1/uploads/"+queuedID, &admin, "", nil)
		value := resourceJSON(t, receipt, 200)
		var gotState string
		if err := json.Unmarshal(value["state"], &gotState); err != nil || gotState != "received" {
			t.Fatalf("accepted receipt blocked: %s %v", receipt.Body.String(), err)
		}
		id, err := uploads.NewID()
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AcceptUpload(id, admin.User.ID, manager.Snapshot()); err != nil {
			t.Fatal(err)
		}
		if err := db.Received(id); err != nil {
			t.Fatal(err)
		}
		content := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>`)
		digest := sha256.Sum256(content)
		hash := hex.EncodeToString(digest[:])
		objects := filepath.Join(deployment.PublicDir, ".objects")
		if err := os.MkdirAll(objects, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(objects, hash), content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveUpload(store.Image{ID: id, State: "completed", Format: "svg", Bytes: int64(len(content)), Width: 1, Height: 1, Frames: 1, Hash: hash}); err != nil {
			t.Fatal(err)
		}
		public := resourceRequest(handler, "GET", "/image/"+id, nil, "", nil)
		if public.Code != 200 || !bytes.Equal(public.Body.Bytes(), content) || public.Header().Get("Content-Type") != "image/svg+xml" {
			t.Fatalf("public content blocked: HTTP%d %s", public.Code, public.Body.String())
		}
		resourcePrivateFieldsAbsent(t, resourceRequest(handler, "GET", "/api/v1/uploads/"+id, &admin, "", nil), 200)
	})
}
