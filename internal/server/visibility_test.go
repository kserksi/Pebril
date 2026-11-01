package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

const visibilityOrigin = "https://example.test"

type visibilityHTTPEnv struct {
	handler        http.Handler
	db             *store.Store
	manager        *config.Manager
	deployment     config.Deployment
	authentication *auth.Service
	upload         *uploads.Service
	admin          auth.Session
	member         auth.Session
	other          auth.Session
}

func newVisibilityHTTPEnv(t *testing.T) *visibilityHTTPEnv {
	t.Helper()
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
	secret, err := auth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := authentication.Initialize("administrator", secret)
	if err != nil {
		t.Fatal(err)
	}
	member, err := authentication.Register("member", secret)
	if err != nil {
		t.Fatal(err)
	}
	other, err := authentication.Register("other", secret)
	if err != nil {
		t.Fatal(err)
	}
	session := func(user store.User) auth.Session {
		t.Helper()
		value, e := authentication.Login(user.Username, secret)
		if e != nil {
			t.Fatal(e)
		}
		return value
	}
	adminSession := session(admin)
	memberSession := session(member)
	otherSession := session(other)
	upload, err := uploads.New(db, manager, deployment, "/bin/false", nativeprobe.Result{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upload.Close() })
	return &visibilityHTTPEnv{
		handler:        HandlerWithUploads("0.0.11-test", manager, db, nativeprobe.Result{Ready: true}, authentication, upload),
		db:             db,
		manager:        manager,
		deployment:     deployment,
		authentication: authentication,
		upload:         upload,
		admin:          adminSession,
		member:         memberSession,
		other:          otherSession,
	}
}

func (env *visibilityHTTPEnv) request(method, path, body string, session *auth.Session, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, visibilityOrigin+path, strings.NewReader(body))
	if session != nil {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		req.Header.Set("X-CSRF-Token", session.CSRF)
	}
	req.Header.Set("Origin", visibilityOrigin)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	env.handler.ServeHTTP(recorder, req)
	return recorder
}

func (env *visibilityHTTPEnv) seed(t *testing.T, owner int64, content string) (string, string) {
	t.Helper()
	id, err := uploads.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.AcceptUpload(id, owner, env.manager.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if err := env.db.Received(id); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(digest[:])
	objects := filepath.Join(env.deployment.PublicDir, ".objects")
	if err := os.MkdirAll(objects, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objects, hash), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	image := store.Image{ID: id, UserID: owner, State: "queued", Format: "png", Bytes: int64(len(content)), Width: 2, Height: 2, Frames: 1, Hash: hash}
	if err := env.db.SaveUpload(image); err != nil {
		t.Fatal(err)
	}
	claimed, err := env.db.ClaimConversion(id)
	if err != nil {
		t.Fatal(err)
	}
	file := store.ImageFile{Role: "original", Hash: hash, Format: "png", Bytes: int64(len(content)), Width: 2, Height: 2, Frames: 1}
	result := file
	result.Role = "result"
	if err := env.db.FinishConversion(claimed, []store.ImageFile{file, result}); err != nil {
		t.Fatal(err)
	}
	return id, hash
}

func (env *visibilityHTTPEnv) keyCount(t *testing.T, id string) int {
	t.Helper()
	var count int
	if err := env.db.DB.QueryRow("SELECT COUNT(*) FROM image_keys WHERE image_id=?", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// visibilityCheck requires the response status and the project-wide no-store
// policy, then returns the body for leak assertions.
func visibilityCheck(t *testing.T, recorder *httptest.ResponseRecorder, status int) []byte {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", recorder.Code, status, recorder.Body.String())
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cache)
	}
	return recorder.Body.Bytes()
}

func visibilityStatus(t *testing.T, recorder *httptest.ResponseRecorder, status int) {
	t.Helper()
	body := visibilityCheck(t, recorder, status)
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("Error response is not JSON: %s", body)
	}
	if _, ok := value["error"]; !ok {
		t.Fatalf("Error response has no error envelope: %s", body)
	}
}

func visibilityNoContent(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != 204 {
		t.Fatalf("HTTP %d, want 204: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("204 response has a body: %s", recorder.Body.String())
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cache)
	}
}

type visibilityKeyHTTP struct {
	Generation int64 `json:"generation"`
	ExpiresAt  int64 `json:"expires_at"`
	LifetimeMS int64 `json:"lifetime_ms"`
	Issued     bool  `json:"issued"`
}

type visibilityStateHTTP struct {
	Visibility string             `json:"visibility"`
	Key        *visibilityKeyHTTP `json:"key"`
}

func visibilityState(t *testing.T, recorder *httptest.ResponseRecorder, status int) (visibilityStateHTTP, string) {
	t.Helper()
	body := visibilityCheck(t, recorder, status)
	var state visibilityStateHTTP
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("Visibility response is not JSON: %s", body)
	}
	return state, string(body)
}

type visibilityIssuedHTTP struct {
	Generation int64  `json:"generation"`
	ExpiresAt  int64  `json:"expires_at"`
	LifetimeMS int64  `json:"lifetime_ms"`
	Key        string `json:"key"`
}

func visibilityIssued(t *testing.T, recorder *httptest.ResponseRecorder, status int) (visibilityIssuedHTTP, string) {
	t.Helper()
	body := visibilityCheck(t, recorder, status)
	var issued visibilityIssuedHTTP
	if err := json.Unmarshal(body, &issued); err != nil {
		t.Fatalf("Key response is not JSON: %s", body)
	}
	if !visibilityKeyShape(issued.Key) {
		t.Fatalf("Key %q is not a 26-character Crockford base32 value", issued.Key)
	}
	return issued, string(body)
}

func visibilityKeyShape(key string) bool {
	if len(key) != 26 {
		return false
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, character := range key {
		if !strings.ContainsRune(alphabet, character) {
			return false
		}
	}
	return true
}

func visibilityDeadlineClose(t *testing.T, expiresAt, lifetimeMS int64) {
	t.Helper()
	delta := expiresAt - (time.Now().UnixMilli() + lifetimeMS)
	if delta < -5000 || delta > 5000 {
		t.Fatalf("expires_at %d is not close to now+%dms (delta %d)", expiresAt, lifetimeMS, delta)
	}
}

func TestVisibilityHTTPTransitionsAndAuthorization(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	const content = "visibility transition payload bytes"
	id, hash := env.seed(t, env.member.User.ID, content)
	path := "/api/v1/images/" + id
	visibility := path + "/visibility"

	state, raw := visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if state.Visibility != string(store.VisibilityPublic) || state.Key != nil {
		t.Fatalf("Initial visibility state = %+v", state)
	}
	if strings.Contains(raw, hash) {
		t.Fatalf("Visibility response leaked the stored hash: %s", raw)
	}
	image, err := env.db.Image(id)
	if err != nil {
		t.Fatal(err)
	}
	if image.Visibility != store.VisibilityPublic || image.Bytes != int64(len(content)) || image.Hash != hash || store.FileRole(image.Files, "original").Hash != hash || store.FileRole(image.Files, "result").Hash != hash {
		t.Fatalf("Seeded image = %+v", image)
	}

	visibilityStatus(t, env.request("GET", visibility, "", nil, nil), 401)
	visibilityStatus(t, env.request("GET", visibility, "", &env.other, nil), 403)
	adminState, _ := visibilityState(t, env.request("GET", visibility, "", &env.admin, nil), 200)
	if adminState.Visibility != string(store.VisibilityPublic) || adminState.Key != nil {
		t.Fatalf("Administrator visibility state = %+v", adminState)
	}
	visibilityStatus(t, env.request("GET", "/api/v1/images/missing/visibility", "", &env.member, nil), 404)
	visibilityStatus(t, env.request("POST", "/api/v1/images/missing/visibility", `{"visibility":"private"}`, &env.member, nil), 404)

	visibilityStatus(t, env.request("POST", visibility, `{"visibility":"private"}`, nil, nil), 401)
	visibilityStatus(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.other, nil), 403)
	visibilityStatus(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.member, map[string]string{"X-CSRF-Token": ""}), 403)
	visibilityStatus(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.member, map[string]string{"Origin": "https://attacker.test"}), 403)
	bearer, err := env.authentication.IssueToken(env.member)
	if err != nil {
		t.Fatal(err)
	}
	visibilityStatus(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.member, map[string]string{"Authorization": "Bearer " + bearer}), 403)

	for _, body := range []string{
		``, `null`, `[]`, `{}`, `{"visibility":null}`, `{"visibility":true}`, `{"visibility":1}`,
		`{"visibility":"shared"}`, `{"visibility":"Public"}`, `{"Visibility":"private"}`,
		`{"visibility":"private","visibility":"public"}`, `{"visibility":"private","extra":true}`,
		`{"visibility":"private"} {}`, `{"visibility":"private"} null`,
	} {
		visibilityStatus(t, env.request("POST", visibility, body, &env.member, nil), 400)
	}
	state, _ = visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if state.Visibility != string(store.VisibilityPublic) {
		t.Fatalf("Rejected request changed visibility to %q", state.Visibility)
	}

	changed, changedRaw := visibilityStateRequest(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.member, nil), 200)
	if changed.Visibility != string(store.VisibilityPrivate) || changed.Key != nil {
		t.Fatalf("Private state = %+v", changed)
	}
	if strings.Contains(changedRaw, hash) {
		t.Fatalf("Visibility change leaked the stored hash: %s", changedRaw)
	}
	state, _ = visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if state.Visibility != string(store.VisibilityPrivate) || state.Key != nil {
		t.Fatalf("Private response without key = %+v", state)
	}
	detail, detailRaw := visibilityDetail(t, env.request("GET", path, "", &env.member, nil))
	if detail.ID != id || detail.Visibility != store.VisibilityPrivate || detail.Bytes != int64(len(content)) || strings.Contains(detailRaw, hash) {
		t.Fatalf("Private detail = %+v", detail)
	}
	visibilityCheck(t, env.request("GET", "/image/"+id, "", nil, nil), 404)
	visibilityCheck(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.member, nil), 200)

	visibilityCheck(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.member, nil), 200)
	state, _ = visibilityState(t, env.request("GET", visibility, "", &env.admin, nil), 200)
	if state.Visibility != string(store.VisibilityPrivate) || state.Key != nil {
		t.Fatalf("Administrator private state = %+v", state)
	}
	visibilityCheck(t, env.request("POST", visibility, `{"visibility":"public"}`, &env.admin, nil), 200)
	visibilityStatus(t, env.request("GET", visibility, "", &env.other, nil), 403)
	state, _ = visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if state.Visibility != string(store.VisibilityPublic) {
		t.Fatalf("Administrator restored visibility to %q", state.Visibility)
	}

	for _, step := range []struct {
		session *auth.Session
		value   string
	}{
		{&env.member, `{"visibility":"private"}`},
		{&env.member, `{"visibility":"private"}`},
		{&env.admin, `{"visibility":"public"}`},
		{&env.member, `{"visibility":"private"}`},
		{&env.admin, `{"visibility":"public"}`},
	} {
		visibilityCheck(t, env.request("POST", visibility, step.value, step.session, nil), 200)
		if step.value == `{"visibility":"private"}` {
			// A private record is unreachable on every public content path.
			visibilityCheck(t, env.request("GET", "/image/"+id, "", nil, nil), 404)
		} else {
			visibilityPublicBytes(t, env.request("GET", "/image/"+id, "", nil, nil), content)
		}
	}
	image, err = env.db.Image(id)
	if err != nil {
		t.Fatal(err)
	}
	if image.Visibility != store.VisibilityPublic || image.Bytes != int64(len(content)) || image.Hash != hash || store.FileRole(image.Files, "original").Hash != hash || store.FileRole(image.Files, "result").Hash != hash {
		t.Fatalf("Image after public/private round trips = %+v", image)
	}
	if _, err := os.Stat(filepath.Join(env.deployment.PublicDir, ".objects", hash)); err != nil {
		t.Fatalf("Stored object disappeared: %v", err)
	}
}

// visibilityStateRequest reads the POST visibility response, whose only field
// is visibility and which must never carry key material.
func visibilityStateRequest(t *testing.T, recorder *httptest.ResponseRecorder, status int) (visibilityStateHTTP, string) {
	t.Helper()
	body := visibilityCheck(t, recorder, status)
	var state visibilityStateHTTP
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("Visibility change is not JSON: %s", body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 {
		t.Fatalf("Visibility change returned unexpected fields: %s", body)
	}
	return state, string(body)
}

func visibilityDetail(t *testing.T, recorder *httptest.ResponseRecorder) (store.Image, string) {
	t.Helper()
	body := visibilityCheck(t, recorder, 200)
	var image store.Image
	if err := json.Unmarshal(body, &image); err != nil {
		t.Fatalf("Image detail is not JSON: %s", body)
	}
	return image, string(body)
}

func visibilityPublicBytes(t *testing.T, recorder *httptest.ResponseRecorder, content string) {
	t.Helper()
	if recorder.Code != 200 {
		t.Fatalf("Public delivery HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != content {
		t.Fatalf("Public delivery changed bytes: %q", recorder.Body.String())
	}
}

func TestVisibilityHTTPKeyIssueRotateRevoke(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	id, hash := env.seed(t, env.member.User.ID, "visibility key payload bytes")
	path := "/api/v1/images/" + id
	keyPath := path + "/key"
	visibility := path + "/visibility"

	visibilityStatus(t, env.request("POST", keyPath, "", &env.member, nil), 409)
	if env.keyCount(t, id) != 0 {
		t.Fatal("Public image received a key")
	}
	visibilityStatus(t, env.request("POST", keyPath, "", nil, nil), 401)
	visibilityStatus(t, env.request("POST", keyPath, "", &env.other, nil), 403)
	visibilityStatus(t, env.request("POST", keyPath, "", &env.admin, nil), 403)
	visibilityStatus(t, env.request("POST", keyPath, "", &env.member, map[string]string{"X-CSRF-Token": ""}), 403)
	visibilityStatus(t, env.request("POST", keyPath, "", &env.member, map[string]string{"Origin": "https://attacker.test"}), 403)
	visibilityStatus(t, env.request("DELETE", keyPath, "", nil, nil), 401)
	visibilityStatus(t, env.request("DELETE", keyPath, "", &env.other, nil), 403)
	visibilityStatus(t, env.request("DELETE", keyPath, "", &env.admin, nil), 403)
	visibilityStatus(t, env.request("DELETE", keyPath, "", &env.member, map[string]string{"Origin": "https://attacker.test"}), 403)
	visibilityStatus(t, env.request("POST", "/api/v1/images/missing/key", "", &env.member, nil), 404)
	visibilityStatus(t, env.request("DELETE", "/api/v1/images/missing/key", "", &env.member, nil), 404)

	visibilityCheck(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.member, nil), 200)
	state, _ := visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if state.Visibility != string(store.VisibilityPrivate) || state.Key != nil {
		t.Fatalf("Private image without key = %+v", state)
	}

	issued, issuedRaw := visibilityIssued(t, env.request("POST", keyPath, "", &env.member, nil), 201)
	if issued.Generation != 1 || issued.LifetimeMS != 1800000 {
		t.Fatalf("Default key = %+v", issued)
	}
	visibilityDeadlineClose(t, issued.ExpiresAt, 1800000)
	if strings.Contains(issuedRaw, hash) || strings.Contains(issuedRaw, "hash") {
		t.Fatalf("Key response leaked hash material: %s", issuedRaw)
	}
	state, stateRaw := visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if state.Visibility != string(store.VisibilityPrivate) || state.Key == nil || state.Key.Generation != 1 || !state.Key.Issued || state.Key.LifetimeMS != 1800000 || state.Key.ExpiresAt != issued.ExpiresAt {
		t.Fatalf("Key state = %+v", state)
	}
	if strings.Contains(stateRaw, issued.Key) || strings.Contains(stateRaw, hash) || strings.Contains(stateRaw, "hash") {
		t.Fatalf("Visibility response leaked key material: %s", stateRaw)
	}
	adminState, adminRaw := visibilityState(t, env.request("GET", visibility, "", &env.admin, nil), 200)
	if adminState.Key == nil || adminState.Key.Generation != 1 {
		t.Fatalf("Administrator key state = %+v", adminState)
	}
	if strings.Contains(adminRaw, issued.Key) || strings.Contains(adminRaw, hash) {
		t.Fatalf("Administrator response leaked key material: %s", adminRaw)
	}

	rotated, rotatedRaw := visibilityIssued(t, env.request("POST", keyPath, `{"lifetime_ms":60000}`, &env.member, nil), 201)
	if rotated.Generation != 2 || rotated.LifetimeMS != 60000 || rotated.Key == issued.Key {
		t.Fatalf("Rotated key = %+v", rotated)
	}
	visibilityDeadlineClose(t, rotated.ExpiresAt, 60000)
	if strings.Contains(rotatedRaw, issued.Key) || strings.Contains(rotatedRaw, hash) {
		t.Fatalf("Rotation response leaked old key material: %s", rotatedRaw)
	}
	if err := env.db.VerifyKey(context.Background(), id, issued.Key, time.Now()); !errors.Is(err, store.ErrKeyInvalid) {
		t.Fatalf("Rotated key still verifies, error = %v", err)
	}
	if err := env.db.VerifyKey(context.Background(), id, rotated.Key, time.Now()); err != nil {
		t.Fatalf("Rotated key does not verify: %v", err)
	}

	for _, body := range []string{
		`{"lifetime_ms":59999}`, `{"lifetime_ms":259200001}`, `{"lifetime_ms":0}`, `{"lifetime_ms":-1}`,
		`{"lifetime_ms":1.5}`, `{"lifetime_ms":"60000"}`, `{"lifetime_ms":true}`, `{"lifetime_ms":null}`,
		`{"lifetime_ms":60000,"lifetime_ms":60001}`, `{"lifetime_ms":60000,"extra":1}`,
		`{"lifetime_ms":9223372036854775808}`, `null`, `[]`, `{"lifetime_ms":60000} {}`, `{"lifetime_ms":60000} null`,
	} {
		visibilityStatus(t, env.request("POST", keyPath, body, &env.member, nil), 400)
	}
	record, err := env.db.Key(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Generation != 2 || record.LifetimeMS != 60000 {
		t.Fatalf("Rejected bounds changed the key: %+v", record)
	}

	maximum, _ := visibilityIssued(t, env.request("POST", keyPath, `{"lifetime_ms":259200000}`, &env.member, nil), 201)
	if maximum.Generation != 3 || maximum.LifetimeMS != store.MaxKeyLifetimeMS {
		t.Fatalf("Maximum lifetime key = %+v", maximum)
	}
	visibilityDeadlineClose(t, maximum.ExpiresAt, store.MaxKeyLifetimeMS)
	defaultObject, _ := visibilityIssued(t, env.request("POST", keyPath, `{}`, &env.member, nil), 201)
	if defaultObject.Generation != 4 || defaultObject.LifetimeMS != 1800000 {
		t.Fatalf("Empty object key = %+v", defaultObject)
	}

	if _, err := env.db.DB.Exec("UPDATE image_keys SET expires_at=? WHERE image_id=?", time.Now().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	expiredState, _ := visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if expiredState.Key == nil || expiredState.Key.Issued || expiredState.Key.Generation != 4 {
		t.Fatalf("Expired key state = %+v", expiredState)
	}

	visibilityCheck(t, env.request("POST", visibility, `{"visibility":"public"}`, &env.member, nil), 200)
	publicState, publicRaw := visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if publicState.Visibility != string(store.VisibilityPublic) || publicState.Key == nil || publicState.Key.Generation != 4 {
		t.Fatalf("Public image kept key state = %+v", publicState)
	}
	if strings.Contains(publicRaw, defaultObject.Key) || strings.Contains(publicRaw, hash) {
		t.Fatalf("Public visibility response leaked key material: %s", publicRaw)
	}
	visibilityCheck(t, env.request("POST", visibility, `{"visibility":"private"}`, &env.member, nil), 200)

	visibilityNoContent(t, env.request("DELETE", keyPath, "", &env.member, nil))
	if env.keyCount(t, id) != 0 {
		t.Fatal("Key row survived deletion")
	}
	state, _ = visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if state.Key != nil {
		t.Fatalf("Deleted key state = %+v", state)
	}
	visibilityNoContent(t, env.request("DELETE", keyPath, "", &env.member, nil))

	replacement, replacementRaw := visibilityIssued(t, env.request("POST", keyPath, "", &env.member, nil), 201)
	if !visibilityKeyShape(replacement.Key) || replacement.Key == rotated.Key || replacement.Key == maximum.Key || replacement.Key == defaultObject.Key {
		t.Fatalf("Replacement key = %+v", replacement)
	}
	if strings.Contains(replacementRaw, defaultObject.Key) || strings.Contains(replacementRaw, hash) {
		t.Fatalf("Replacement response leaked old key material: %s", replacementRaw)
	}
	if err := env.db.VerifyKey(context.Background(), id, rotated.Key, time.Now()); !errors.Is(err, store.ErrKeyInvalid) {
		t.Fatalf("Revoked key still verifies, error = %v", err)
	}
	if err := env.db.VerifyKey(context.Background(), id, replacement.Key, time.Now()); err != nil {
		t.Fatalf("Replacement key does not verify: %v", err)
	}
	state, stateRaw = visibilityState(t, env.request("GET", visibility, "", &env.member, nil), 200)
	if state.Key == nil || state.Key.Generation != replacement.Generation {
		t.Fatalf("Replacement key state = %+v", state)
	}
	if strings.Contains(stateRaw, replacement.Key) || strings.Contains(stateRaw, hash) || strings.Contains(stateRaw, "hash") {
		t.Fatalf("Visibility response leaked key material after rotation: %s", stateRaw)
	}
}
