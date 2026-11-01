package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

const tagTestOrigin = "https://example.test"

const tagDayMilliseconds = int64(24 * 60 * 60 * 1000)

type tagHTTPEnv struct {
	handler    http.Handler
	db         *store.Store
	manager    *config.Manager
	deployment config.Deployment
	admin      auth.Session
	member     auth.Session
	other      auth.Session
}

func newTagHTTPEnv(t *testing.T) *tagHTTPEnv {
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
	login := func(user store.User) auth.Session {
		t.Helper()
		session, e := authentication.Login(user.Username, secret)
		if e != nil {
			t.Fatal(e)
		}
		return session
	}
	upload, err := uploads.New(db, manager, deployment, "/bin/false", nativeprobe.Result{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upload.Close() })
	return &tagHTTPEnv{
		handler:    HandlerWithUploads("0.0.11-test", manager, db, nativeprobe.Result{Ready: true}, authentication, upload),
		db:         db,
		manager:    manager,
		deployment: deployment,
		admin:      login(admin),
		member:     login(member),
		other:      login(other),
	}
}

func (env *tagHTTPEnv) request(method, path, body string, session *auth.Session, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, tagTestOrigin+path, strings.NewReader(body))
	if session != nil {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		req.Header.Set("X-CSRF-Token", session.CSRF)
	}
	req.Header.Set("Origin", tagTestOrigin)
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

// uploadImage seeds one converted image with an original and a result role.
func (env *tagHTTPEnv) uploadImage(t *testing.T, owner int64, content string) string {
	t.Helper()
	id, err := uploads.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err = env.db.AcceptUpload(id, owner, env.manager.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if err = env.db.Received(id); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(digest[:])
	objects := filepath.Join(env.deployment.PublicDir, ".objects")
	if err = os.MkdirAll(objects, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(objects, hash), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	image := store.Image{ID: id, UserID: owner, State: "queued", Format: "png", Bytes: int64(len(content)), Width: 2, Height: 2, Frames: 1, Hash: hash}
	if err = env.db.SaveUpload(image); err != nil {
		t.Fatal(err)
	}
	claimed, err := env.db.ClaimConversion(id)
	if err != nil {
		t.Fatal(err)
	}
	file := store.ImageFile{Role: "original", Hash: hash, Format: "png", Bytes: int64(len(content)), Width: 2, Height: 2, Frames: 1}
	result := file
	result.Role = "result"
	if err = env.db.FinishConversion(claimed, []store.ImageFile{file, result}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (env *tagHTTPEnv) setCreated(t *testing.T, id string, createdAt int64) {
	t.Helper()
	if _, err := env.db.DB.Exec("UPDATE images SET created_at=? WHERE id=?", createdAt, id); err != nil {
		t.Fatal(err)
	}
}

func (env *tagHTTPEnv) setVisibility(t *testing.T, id, visibility string) {
	t.Helper()
	if _, err := env.db.DB.Exec("UPDATE images SET visibility=? WHERE id=?", visibility, id); err != nil {
		t.Fatal(err)
	}
}

func tagCheck(t *testing.T, recorder *httptest.ResponseRecorder, status int) []byte {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", recorder.Code, status, recorder.Body.String())
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cache)
	}
	return recorder.Body.Bytes()
}

func tagErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error == "" {
		t.Fatalf("Invalid error envelope: %s", body)
	}
	return envelope.Code
}

func decodeTagSet(t *testing.T, body []byte) []string {
	t.Helper()
	var value struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &value); err != nil || value.Tags == nil {
		t.Fatalf("Invalid tag set: %s", body)
	}
	return value.Tags
}

type tagLibraryValue struct {
	Name      string `json:"name"`
	Count     int    `json:"count"`
	CreatedAt int64  `json:"created_at"`
}

func decodeTagLibrary(t *testing.T, body []byte) []tagLibraryValue {
	t.Helper()
	var value []tagLibraryValue
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("Invalid tag library: %s", body)
	}
	return value
}

func tagNames(values []tagLibraryValue) []string {
	names := make([]string, 0, len(values))
	for _, value := range values {
		names = append(names, value.Name)
	}
	return names
}

func expectTags(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tags = %v, want %v", got, want)
		}
	}
}

func containsTag(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func (env *tagHTTPEnv) library(t *testing.T, session *auth.Session) []tagLibraryValue {
	t.Helper()
	return decodeTagLibrary(t, tagCheck(t, env.request("GET", "/api/v1/tags", "", session, nil), 200))
}

func (env *tagHTTPEnv) imageTags(t *testing.T, session *auth.Session, id string) []string {
	t.Helper()
	return decodeTagSet(t, tagCheck(t, env.request("GET", "/api/v1/images/"+id+"/tags", "", session, nil), 200))
}

func (env *tagHTTPEnv) replaceTags(t *testing.T, session *auth.Session, id string, names []string) []string {
	t.Helper()
	payload, err := json.Marshal(map[string][]string{"tags": names})
	if err != nil {
		t.Fatal(err)
	}
	return decodeTagSet(t, tagCheck(t, env.request("PUT", "/api/v1/images/"+id+"/tags", string(payload), session, nil), 200))
}

func (env *tagHTTPEnv) addTags(t *testing.T, session *auth.Session, id string, names []string) []string {
	t.Helper()
	payload, err := json.Marshal(map[string][]string{"tags": names})
	if err != nil {
		t.Fatal(err)
	}
	return decodeTagSet(t, tagCheck(t, env.request("POST", "/api/v1/images/"+id+"/tags", string(payload), session, nil), 200))
}

type galleryListValue struct {
	Images []galleryImage `json:"images"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset *int           `json:"offset"`
	Next   *string        `json:"next"`
}

func decodeGalleryList(t *testing.T, body []byte) galleryListValue {
	t.Helper()
	var value galleryListValue
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("Invalid gallery list: %s", body)
	}
	return value
}

func imageIDs(images []galleryImage) []string {
	ids := make([]string, 0, len(images))
	for _, image := range images {
		ids = append(ids, image.ID)
	}
	return ids
}

func expectImageIDs(t *testing.T, images []galleryImage, want ...string) {
	t.Helper()
	got := imageIDs(images)
	if len(got) != len(want) {
		t.Fatalf("image ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("image ids = %v, want %v", got, want)
		}
	}
}

func (env *tagHTTPEnv) galleryList(t *testing.T, session *auth.Session, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/images"
	if len(values) > 0 {
		path += "?" + values.Encode()
	}
	return env.request("GET", path, "", session, nil)
}

type timelineValue struct {
	Buckets []store.TimelineBucket `json:"buckets"`
	Groups  []store.TimelineBucket `json:"groups"`
}

func decodeTimeline(t *testing.T, body []byte) []store.TimelineBucket {
	t.Helper()
	var value timelineValue
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("Invalid timeline: %s", body)
	}
	if value.Buckets != nil {
		return value.Buckets
	}
	return value.Groups
}

func expectBuckets(t *testing.T, got []store.TimelineBucket, want ...store.TimelineBucket) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("buckets = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bucket %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestTagsLibraryAndImageTagsHTTP covers library isolation, the admin
// namespace rule (D15), replace/add/remove idempotency, limits, normalization,
// and error codes on the tag endpoints.
func TestTagsLibraryAndImageTagsHTTP(t *testing.T) {
	env := newTagHTTPEnv(t)

	tagCheck(t, env.request("GET", "/api/v1/tags", "", nil, nil), 401)
	tagCheck(t, env.request("GET", "/api/v1/tags", "", &env.member, map[string]string{"Authorization": "Bearer upload-only"}), 403)
	body := tagCheck(t, env.request("GET", "/api/v1/tags", "", &env.member, nil), 200)
	if trimmed := strings.TrimSpace(string(body)); trimmed != "[]" {
		t.Fatalf("Empty library = %s, want []", trimmed)
	}

	created := tagCheck(t, env.request("POST", "/api/v1/tags", `{"name":"  Alpha  "}`, &env.member, nil), 200)
	var ensured struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(created, &ensured); err != nil || ensured.Name != "Alpha" || ensured.Count != 0 {
		t.Fatalf("Normalized ensure = %s, %v", created, err)
	}
	if strings.Contains(string(created), "user_id") || strings.Contains(string(created), "created_at") {
		t.Fatalf("Library ensure leaks internal fields: %s", created)
	}
	tagCheck(t, env.request("POST", "/api/v1/tags", `{"name":"Alpha"}`, &env.member, nil), 200)

	libraryBody := tagCheck(t, env.request("GET", "/api/v1/tags", "", &env.member, nil), 200)
	library := decodeTagLibrary(t, libraryBody)
	if len(library) != 1 || library[0].Name != "Alpha" || library[0].Count != 0 || library[0].CreatedAt <= 0 {
		t.Fatalf("Library after ensure = %+v", library)
	}
	for _, forbidden := range []string{`"id"`, `"user_id"`, `"tag_id"`, `"image_id"`} {
		if strings.Contains(string(libraryBody), forbidden) {
			t.Fatalf("Tag library leaks %s: %s", forbidden, libraryBody)
		}
	}

	for _, invalid := range []string{
		`{"name":"ok","user_id":7}`,
		`{"name":null}`,
		`{"name":1}`,
		`{}`,
		`{"name":"a","name":"b"}`,
		`{"name":"ok"} trailing`,
	} {
		tagCheck(t, env.request("POST", "/api/v1/tags", invalid, &env.member, nil), 400)
	}
	for _, name := range []string{"", "#bad", "   ", strings.Repeat("é", 33), "bad\x01tag"} {
		payload, err := json.Marshal(map[string]string{"name": name})
		if err != nil {
			t.Fatal(err)
		}
		body = tagCheck(t, env.request("POST", "/api/v1/tags", string(payload), &env.member, nil), 400)
		if code := tagErrorCode(t, body); code != codeInvalidTagName {
			t.Fatalf("Invalid name %q code = %q", name, code)
		}
	}
	normalized, err := json.Marshal(map[string]string{"name": "a\tb  c"})
	if err != nil {
		t.Fatal(err)
	}
	body = tagCheck(t, env.request("POST", "/api/v1/tags", string(normalized), &env.member, nil), 200)
	if err = json.Unmarshal(body, &ensured); err != nil || ensured.Name != "a b c" {
		t.Fatalf("Collapsed name = %s, %v", body, err)
	}
	maxName, err := json.Marshal(map[string]string{"name": strings.Repeat("é", 32)})
	if err != nil {
		t.Fatal(err)
	}
	tagCheck(t, env.request("POST", "/api/v1/tags", string(maxName), &env.member, nil), 200)

	if got := env.library(t, &env.other); len(got) != 0 {
		t.Fatalf("Other library is not isolated: %+v", got)
	}
	tagCheck(t, env.request("POST", "/api/v1/tags", `{"name":"Alpha"}`, &env.other, nil), 200)
	otherLibrary := env.library(t, &env.other)
	if len(otherLibrary) != 1 || otherLibrary[0].Name != "Alpha" {
		t.Fatalf("Other library = %+v", otherLibrary)
	}
	if got := env.library(t, &env.member); len(got) != 3 {
		t.Fatalf("Member library after other ensure = %+v", got)
	}

	first := env.uploadImage(t, env.member.User.ID, "first tag image")
	foreign := env.uploadImage(t, env.other.User.ID, "foreign tag image")

	expectTags(t, env.replaceTags(t, &env.member, first, []string{"blue", "alpha", "blue"}), "alpha", "blue")
	expectTags(t, env.imageTags(t, &env.member, first), "alpha", "blue")
	expectTags(t, env.replaceTags(t, &env.member, first, []string{"blue", "alpha"}), "alpha", "blue")
	expectTags(t, env.replaceTags(t, &env.member, first, []string{}))
	expectTags(t, env.replaceTags(t, &env.member, first, []string{"red", "blue"}), "blue", "red")
	expectTags(t, env.addTags(t, &env.member, first, []string{"green"}), "blue", "green", "red")
	expectTags(t, env.addTags(t, &env.member, first, []string{"green"}), "blue", "green", "red")

	body = tagCheck(t, env.request("DELETE", "/api/v1/images/"+first+"/tags/green", "", &env.member, nil), 200)
	expectTags(t, decodeTagSet(t, body), "blue", "red")
	body = tagCheck(t, env.request("DELETE", "/api/v1/images/"+first+"/tags/green", "", &env.member, nil), 200)
	expectTags(t, decodeTagSet(t, body), "blue", "red")
	body = tagCheck(t, env.request("DELETE", "/api/v1/images/"+first+"/tags/red", "", &env.member, nil), 200)
	expectTags(t, decodeTagSet(t, body), "blue")
	body = tagCheck(t, env.request("DELETE", "/api/v1/images/"+first+"/tags/red", "", &env.member, nil), 200)
	expectTags(t, decodeTagSet(t, body), "blue")
	body = tagCheck(t, env.request("DELETE", "/api/v1/images/"+first+"/tags/zzz", "", &env.member, nil), 404)
	if code := tagErrorCode(t, body); code != codeTagNotFound {
		t.Fatalf("Unknown removal code = %q", code)
	}

	for _, invalid := range []string{`{"tags":"x"}`, `{}`, `{"tags":["a"],"x":1}`, `{"tags":null}`} {
		tagCheck(t, env.request("PUT", "/api/v1/images/"+first+"/tags", invalid, &env.member, nil), 400)
	}

	payload := `{"tags":["boom"]}`
	tagCheck(t, env.request("PUT", "/api/v1/images/"+first+"/tags", payload, &env.member, map[string]string{"X-CSRF-Token": ""}), 403)
	tagCheck(t, env.request("PUT", "/api/v1/images/"+first+"/tags", payload, &env.member, map[string]string{"Origin": "https://foreign.test"}), 403)
	tagCheck(t, env.request("PUT", "/api/v1/images/"+first+"/tags", payload, &env.member, map[string]string{"Authorization": "Bearer upload-only"}), 403)

	body = tagCheck(t, env.request("GET", "/api/v1/images/"+first+"/tags", "", &env.other, nil), 404)
	if code := tagErrorCode(t, body); code != codeImageNotFound {
		t.Fatalf("Foreign read code = %q", code)
	}
	body = tagCheck(t, env.request("PUT", "/api/v1/images/"+first+"/tags", payload, &env.other, nil), 404)
	if code := tagErrorCode(t, body); code != codeImageNotFound {
		t.Fatalf("Foreign write code = %q", code)
	}
	body = tagCheck(t, env.request("DELETE", "/api/v1/images/"+first+"/tags/blue", "", &env.other, nil), 404)
	if code := tagErrorCode(t, body); code != codeImageNotFound {
		t.Fatalf("Foreign remove code = %q", code)
	}
	body = tagCheck(t, env.request("GET", "/api/v1/images/missing/tags", "", &env.member, nil), 404)
	if code := tagErrorCode(t, body); code != codeImageNotFound {
		t.Fatalf("Missing image code = %q", code)
	}
	body = tagCheck(t, env.request("PUT", "/api/v1/images/missing/tags", payload, &env.member, nil), 404)
	if code := tagErrorCode(t, body); code != codeImageNotFound {
		t.Fatalf("Missing image write code = %q", code)
	}

	// D15: an administrator writes into the image owner's library and keeps an
	// empty library of their own.
	expectTags(t, env.replaceTags(t, &env.admin, first, []string{"admin-choice"}), "admin-choice")
	expectTags(t, env.imageTags(t, &env.member, first), "admin-choice")
	if got := env.library(t, &env.admin); len(got) != 0 {
		t.Fatalf("Admin library gained tags: %+v", got)
	}
	if names := tagNames(env.library(t, &env.member)); !containsTag(names, "admin-choice") {
		t.Fatalf("Member library lacks admin-written tag: %v", names)
	}
	expectTags(t, env.replaceTags(t, &env.admin, foreign, []string{"admin-foreign"}), "admin-foreign")
	if names := tagNames(env.library(t, &env.other)); !containsTag(names, "admin-foreign") {
		t.Fatalf("Other library lacks admin-written tag: %v", names)
	}

	tooMany := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		tooMany = append(tooMany, fmt.Sprintf("over-%02d", i))
	}
	payloadBytes, err := json.Marshal(map[string][]string{"tags": tooMany})
	if err != nil {
		t.Fatal(err)
	}
	body = tagCheck(t, env.request("PUT", "/api/v1/images/"+first+"/tags", string(payloadBytes), &env.member, nil), 409)
	if code := tagErrorCode(t, body); code != codeTagLimitReached {
		t.Fatalf("Image limit code = %q", code)
	}
	if names := tagNames(env.library(t, &env.member)); containsTag(names, "over-20") {
		t.Fatalf("Rejected image limit created a tag: %v", names)
	}
	twenty := append([]string(nil), tooMany[:20]...)
	expectTags(t, env.replaceTags(t, &env.member, first, twenty), twenty...)
	payloadBytes, err = json.Marshal(map[string][]string{"tags": {"extra-rollback"}})
	if err != nil {
		t.Fatal(err)
	}
	body = tagCheck(t, env.request("POST", "/api/v1/images/"+first+"/tags", string(payloadBytes), &env.member, nil), 409)
	if code := tagErrorCode(t, body); code != codeTagLimitReached {
		t.Fatalf("Add image limit code = %q", code)
	}
	if names := tagNames(env.library(t, &env.member)); containsTag(names, "extra-rollback") {
		t.Fatalf("Rejected add created a tag: %v", names)
	}

	for i := 0; ; i++ {
		if len(env.library(t, &env.member)) >= 200 {
			break
		}
		if _, err = env.db.EnsureTag(context.Background(), env.member.User.ID, fmt.Sprintf("fill-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if library = env.library(t, &env.member); len(library) != 200 {
		t.Fatalf("Library size = %d, want 200", len(library))
	}
	body = tagCheck(t, env.request("POST", "/api/v1/tags", `{"name":"overflow"}`, &env.member, nil), 409)
	if code := tagErrorCode(t, body); code != codeTagLimitReached {
		t.Fatalf("Library limit code = %q", code)
	}
	payloadBytes, err = json.Marshal(map[string][]string{"tags": {"brand-new"}})
	if err != nil {
		t.Fatal(err)
	}
	body = tagCheck(t, env.request("PUT", "/api/v1/images/"+first+"/tags", string(payloadBytes), &env.member, nil), 409)
	if code := tagErrorCode(t, body); code != codeTagLimitReached {
		t.Fatalf("Tagged replace at library limit code = %q", code)
	}

	tagCheck(t, env.request("DELETE", "/api/v1/tags/fill-000", "", &env.member, nil), 204)
	body = tagCheck(t, env.request("DELETE", "/api/v1/tags/fill-000", "", &env.member, nil), 404)
	if code := tagErrorCode(t, body); code != codeTagNotFound {
		t.Fatalf("Missing tag delete code = %q", code)
	}
	body = tagCheck(t, env.request("DELETE", "/api/v1/tags/%23bad", "", &env.member, nil), 400)
	if code := tagErrorCode(t, body); code != codeInvalidTagName {
		t.Fatalf("Invalid tag delete code = %q", code)
	}
}

// TestGalleryTagFilterHTTP covers all/any tag matching, unknown names, the
// half-open time range, validation errors, and legacy response compatibility.
func TestGalleryTagFilterHTTP(t *testing.T) {
	env := newTagHTTPEnv(t)
	first := env.uploadImage(t, env.member.User.ID, "filter-one")
	second := env.uploadImage(t, env.member.User.ID, "filter-two")
	third := env.uploadImage(t, env.member.User.ID, "filter-three")
	untagged := env.uploadImage(t, env.member.User.ID, "filter-untagged")
	foreign := env.uploadImage(t, env.other.User.ID, "filter-foreign")
	env.replaceTags(t, &env.member, first, []string{"red", "blue"})
	env.replaceTags(t, &env.member, second, []string{"red"})
	env.replaceTags(t, &env.member, third, []string{"blue", "green"})
	env.replaceTags(t, &env.other, foreign, []string{"red"})

	base := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).UnixMilli()
	env.setCreated(t, first, base-1)
	env.setCreated(t, second, base)
	env.setCreated(t, third, base+tagDayMilliseconds-1)
	env.setCreated(t, untagged, base+tagDayMilliseconds)
	env.setCreated(t, foreign, base+1000)

	// Legacy behavior: no new parameters keeps the previous response shape.
	legacyBody := tagCheck(t, env.galleryList(t, &env.member, nil), 200)
	legacy := decodeGalleryList(t, legacyBody)
	if legacy.Total != 4 || len(legacy.Images) != 4 || legacy.Limit != 20 || legacy.Offset == nil || *legacy.Offset != 0 || legacy.Next != nil {
		t.Fatalf("Legacy gallery = %+v", legacy)
	}
	expectImageIDs(t, legacy.Images, untagged, third, second, first)
	for _, image := range legacy.Images {
		if image.OwnerID != env.member.User.ID {
			t.Fatalf("Legacy gallery leaks owner %d", image.OwnerID)
		}
	}
	if strings.Contains(string(legacyBody), `"next"`) || !strings.Contains(string(legacyBody), `"offset"`) {
		t.Fatalf("Legacy response shape changed: %s", legacyBody)
	}

	query := func(values url.Values) galleryListValue {
		t.Helper()
		return decodeGalleryList(t, tagCheck(t, env.galleryList(t, &env.member, values), 200))
	}
	check := func(values url.Values, wantTotal int, want ...string) {
		t.Helper()
		list := query(values)
		if list.Total != wantTotal {
			t.Fatalf("total for %v = %d, want %d", values, list.Total, wantTotal)
		}
		expectImageIDs(t, list.Images, want...)
	}

	check(url.Values{"tags": {"red,blue"}}, 1, first)
	check(url.Values{"tags": {"red,blue"}, "tags_mode": {"all"}}, 1, first)
	check(url.Values{"tags": {"red,blue"}, "mode": {"any"}}, 3, third, second, first)
	check(url.Values{"tags": {"red,blue"}, "tags_mode": {"any"}}, 3, third, second, first)
	check(url.Values{"tags": {"blue,green"}, "tags_mode": {"any"}}, 2, third, first)
	check(url.Values{"tags": {"blue,green"}}, 1, third)
	check(url.Values{"tags": {"red,green"}}, 0)
	check(url.Values{"tags": {"red,red"}}, 2, second, first)
	check(url.Values{"tags": {"red"}}, 2, second, first)
	check(url.Values{"tags": {"red,missing"}, "tags_mode": {"any"}}, 0)
	check(url.Values{"tags": {"missing"}}, 0)
	check(url.Values{"tags": {"Red"}}, 0)
	check(url.Values{"tags": {""}}, 4, untagged, third, second, first)
	check(url.Values{"tags_mode": {""}, "tags": {"red"}}, 2, second, first)

	cursor := query(url.Values{"tags": {"red"}, "limit": {"1"}})
	if cursor.Next == nil || *cursor.Next == "" || cursor.Offset != nil || cursor.Limit != 1 || cursor.Total != 2 || len(cursor.Images) != 1 {
		t.Fatalf("Cursor metadata = %+v", cursor)
	}

	check(url.Values{"from": {strconv.FormatInt(base, 10)}, "to": {strconv.FormatInt(base+tagDayMilliseconds, 10)}}, 2, third, second)
	check(url.Values{"to": {strconv.FormatInt(base, 10)}}, 1, first)
	check(url.Values{"from": {strconv.FormatInt(base+tagDayMilliseconds, 10)}}, 1, untagged)
	check(url.Values{"from": {strconv.FormatInt(base, 10)}, "to": {strconv.FormatInt(base, 10)}}, 0)
	check(url.Values{"from": {"-1"}}, 4, untagged, third, second, first)
	check(url.Values{"tags": {"blue"}, "from": {strconv.FormatInt(base, 10)}}, 1, third)

	badRequests := []struct {
		values url.Values
		code   string
	}{
		{url.Values{"unknown": {"1"}}, codeInvalidGalleryQuery},
		{url.Values{"limit": {"1", "2"}}, codeInvalidGalleryQuery},
		{url.Values{"tags_mode": {"all"}, "mode": {"any"}}, codeInvalidGalleryQuery},
		{url.Values{"scope": {"private"}}, codeInvalidGalleryScope},
		{url.Values{"limit": {"0"}}, codeInvalidGalleryPagination},
		{url.Values{"limit": {"101"}}, codeInvalidGalleryPagination},
		{url.Values{"limit": {"-1"}}, codeInvalidGalleryPagination},
		{url.Values{"limit": {"01"}}, codeInvalidGalleryPagination},
		{url.Values{"offset": {"-1"}}, codeInvalidGalleryPagination},
		{url.Values{"offset": {"1.5"}}, codeInvalidGalleryPagination},
		{url.Values{"tags": {"red"}, "offset": {"0"}}, codeInvalidGalleryPagination},
		{url.Values{"tags_mode": {"bogus"}}, codeInvalidTagMode},
		{url.Values{"tags": {"#red"}}, codeInvalidTagName},
		{url.Values{"tags": {strings.Repeat("x", 33)}}, codeInvalidTagName},
		{url.Values{"from": {"abc"}}, codeInvalidTimeRange},
		{url.Values{"from": {"01"}}, codeInvalidTimeRange},
		{url.Values{"from": {"1.5"}}, codeInvalidTimeRange},
		{url.Values{"from": {""}}, codeInvalidTimeRange},
		{url.Values{"to": {"99999999999999999999"}}, codeInvalidTimeRange},
		{url.Values{"from": {strconv.FormatInt(base+1, 10)}, "to": {strconv.FormatInt(base, 10)}}, codeInvalidTimeRange},
	}
	for _, test := range badRequests {
		body := tagCheck(t, env.galleryList(t, &env.member, test.values), 400)
		if code := tagErrorCode(t, body); code != test.code {
			t.Fatalf("query %v code = %q, want %q", test.values, code, test.code)
		}
	}

	tagCheck(t, env.galleryList(t, &env.member, url.Values{"scope": {"all"}}), 403)
	all := decodeGalleryList(t, tagCheck(t, env.galleryList(t, &env.admin, url.Values{"scope": {"all"}}), 200))
	if all.Total != 5 {
		t.Fatalf("Admin all total = %d, want 5", all.Total)
	}
	owned := false
	for _, image := range all.Images {
		if image.ID == foreign {
			owned = true
		}
	}
	if !owned {
		t.Fatal("Admin all scope lacks another user's image")
	}
	// Tag names resolve in the caller's own library even for the admin all
	// scope, so an admin library without "red" matches nothing.
	if namespaced := decodeGalleryList(t, tagCheck(t, env.galleryList(t, &env.admin, url.Values{"scope": {"all"}, "tags": {"red"}}), 200)); namespaced.Total != 0 {
		t.Fatalf("Admin tag namespace total = %d, want 0", namespaced.Total)
	}

	env.setVisibility(t, third, "private")
	legacy = decodeGalleryList(t, tagCheck(t, env.galleryList(t, &env.member, nil), 200))
	if legacy.Total != 4 {
		t.Fatalf("Visibility filtered the gallery: total %d", legacy.Total)
	}
	foundPrivate := false
	for _, image := range legacy.Images {
		if image.ID == third {
			foundPrivate = string(image.Visibility) == "private"
		}
	}
	if !foundPrivate {
		t.Fatal("Private image missing or not private in the gallery")
	}
	if filtered := decodeGalleryList(t, tagCheck(t, env.galleryList(t, &env.member, url.Values{"tags": {"blue"}, "tags_mode": {"any"}}), 200)); filtered.Total != 2 {
		t.Fatalf("Visibility filtered tagged gallery: total %d", filtered.Total)
	}
}

// TestGalleryKeysetPaginationHTTP covers cursor metadata, stability across
// inserts between pages, limit bounds, and ordering ties.
func TestGalleryKeysetPaginationHTTP(t *testing.T) {
	env := newTagHTTPEnv(t)
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		id := env.uploadImage(t, env.member.User.ID, fmt.Sprintf("keyset-%d", i))
		env.setCreated(t, id, int64((i+1)*1000))
		ids = append(ids, id)
	}

	page := func(values url.Values) galleryListValue {
		t.Helper()
		return decodeGalleryList(t, tagCheck(t, env.galleryList(t, &env.member, values), 200))
	}
	first := page(url.Values{"limit": {"2"}, "from": {"0"}})
	if first.Next == nil || first.Offset != nil || first.Limit != 2 || first.Total != 5 {
		t.Fatalf("First page = %+v", first)
	}
	expectImageIDs(t, first.Images, ids[4], ids[3])

	newest := env.uploadImage(t, env.member.User.ID, "keyset-newest")
	env.setCreated(t, newest, 6000)
	middle := env.uploadImage(t, env.member.User.ID, "keyset-middle")
	env.setCreated(t, middle, 4500)

	second := page(url.Values{"limit": {"2"}, "from": {"0"}, "before": {*first.Next}})
	if second.Total != 7 || second.Next == nil {
		t.Fatalf("Second page = %+v", second)
	}
	expectImageIDs(t, second.Images, ids[2], ids[1])
	third := page(url.Values{"limit": {"2"}, "from": {"0"}, "before": {*second.Next}})
	if third.Next != nil || third.Total != 7 {
		t.Fatalf("Third page = %+v", third)
	}
	expectImageIDs(t, third.Images, ids[0])

	legacy := page(url.Values{"limit": {"2"}})
	if legacy.Offset == nil || *legacy.Offset != 0 || legacy.Next != nil || legacy.Total != 7 {
		t.Fatalf("Legacy page = %+v", legacy)
	}
	expectImageIDs(t, legacy.Images, newest, ids[4])

	arbitrary := store.EncodeGalleryCursor(store.GalleryCursor{CreatedAt: 2500, ID: "not-an-image"})
	expectImageIDs(t, page(url.Values{"from": {"0"}, "before": {arbitrary}}).Images, ids[1], ids[0])

	malformed := []url.Values{
		{"before": {"abc"}},
		{"before": {""}},
		{"before": {store.EncodeGalleryCursor(store.GalleryCursor{CreatedAt: 3000})}},
		{"before": {"bm90YW51bWJlcjppZA"}},
		{"before": {store.EncodeGalleryCursor(store.GalleryCursor{CreatedAt: 3000, ID: strings.Repeat("a", 129)})}},
	}
	for _, values := range malformed {
		body := tagCheck(t, env.galleryList(t, &env.member, values), 400)
		if code := tagErrorCode(t, body); code != codeInvalidCursor {
			t.Fatalf("cursor %v code = %q", values, code)
		}
	}
	for _, values := range []url.Values{
		{"limit": {"0"}, "from": {"0"}},
		{"limit": {"101"}, "from": {"0"}},
	} {
		body := tagCheck(t, env.galleryList(t, &env.member, values), 400)
		if code := tagErrorCode(t, body); code != codeInvalidGalleryPagination {
			t.Fatalf("pagination %v code = %q", values, code)
		}
	}
	body := tagCheck(t, env.galleryList(t, &env.member, url.Values{"before": {*first.Next}, "offset": {"0"}}), 400)
	if code := tagErrorCode(t, body); code != codeInvalidGalleryPagination {
		t.Fatalf("Cursor with offset code = %q", code)
	}

	// Equal timestamps fall back to the id tie-breaker without gaps or repeats.
	for _, id := range append(append([]string(nil), ids...), newest, middle) {
		env.setCreated(t, id, 3000)
	}
	collected := make([]string, 0, 7)
	values := url.Values{"limit": {"2"}, "tags": {""}}
	for step := 0; step < 10; step++ {
		current := page(values)
		collected = append(collected, imageIDs(current.Images)...)
		if current.Next == nil {
			break
		}
		values.Set("before", *current.Next)
	}
	expected := append(append([]string(nil), ids...), newest, middle)
	sort.Sort(sort.Reverse(sort.StringSlice(expected)))
	if strings.Join(collected, ",") != strings.Join(expected, ",") {
		t.Fatalf("Tie pagination = %v, want %v", collected, expected)
	}
}

// TestGalleryTimelineHTTP covers UTC day buckets, bucket edges and ties,
// realtime counts, filtering, admin scope, and the group=day contract path.
func TestGalleryTimelineHTTP(t *testing.T) {
	env := newTagHTTPEnv(t)
	base := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).UnixMilli()
	m1 := env.uploadImage(t, env.member.User.ID, "timeline-one")
	m2 := env.uploadImage(t, env.member.User.ID, "timeline-two")
	m3 := env.uploadImage(t, env.member.User.ID, "timeline-three")
	m4 := env.uploadImage(t, env.member.User.ID, "timeline-four")
	m5 := env.uploadImage(t, env.member.User.ID, "timeline-five")
	foreign := env.uploadImage(t, env.other.User.ID, "timeline-foreign")
	env.setCreated(t, m1, base-1)
	env.setCreated(t, m2, base)
	env.setCreated(t, m3, base+1000)
	env.setCreated(t, m4, base+tagDayMilliseconds-1)
	env.setCreated(t, m5, base+tagDayMilliseconds)
	env.setCreated(t, foreign, base+2000)
	env.replaceTags(t, &env.member, m2, []string{"x"})
	env.replaceTags(t, &env.member, m3, []string{"x"})
	env.replaceTags(t, &env.member, m4, []string{"y"})
	env.replaceTags(t, &env.other, foreign, []string{"x"})

	dayOne := store.TimelineBucket{Day: "2026-01-01", Start: base - tagDayMilliseconds, End: base, Count: 1}
	dayTwo := store.TimelineBucket{Day: "2026-01-02", Start: base, End: base + tagDayMilliseconds, Count: 3}
	dayThree := store.TimelineBucket{Day: "2026-01-03", Start: base + tagDayMilliseconds, End: base + 2*tagDayMilliseconds, Count: 1}

	standalone := func(values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		path := "/api/v1/images/timeline"
		if len(values) > 0 {
			path += "?" + values.Encode()
		}
		return env.request("GET", path, "", &env.member, nil)
	}
	timeline := func(values url.Values) []store.TimelineBucket {
		t.Helper()
		return decodeTimeline(t, tagCheck(t, standalone(values), 200))
	}

	expectBuckets(t, timeline(nil), dayThree, dayTwo, dayOne)
	expectBuckets(t, timeline(url.Values{"tags": {"x"}}), store.TimelineBucket{Day: "2026-01-02", Start: base, End: base + tagDayMilliseconds, Count: 2})
	if got := timeline(url.Values{"tags": {"x,y"}, "tags_mode": {"any"}}); len(got) != 1 || got[0].Day != "2026-01-02" || got[0].Count != 3 {
		t.Fatalf("Any tag timeline = %+v", got)
	}
	expectBuckets(t, timeline(url.Values{"tags": {"x,y"}}))
	expectBuckets(t, timeline(url.Values{"tags": {"missing"}}))
	expectBuckets(t, timeline(url.Values{"from": {strconv.FormatInt(base, 10)}, "to": {strconv.FormatInt(base+tagDayMilliseconds, 10)}}), dayTwo)
	expectBuckets(t, timeline(url.Values{"from": {strconv.FormatInt(base, 10)}, "to": {strconv.FormatInt(base, 10)}}))

	env.replaceTags(t, &env.member, m5, []string{"x"})
	got := timeline(url.Values{"tags": {"x"}})
	if len(got) != 2 || got[0].Day != "2026-01-03" || got[0].Count != 1 || got[1].Count != 2 {
		t.Fatalf("Realtime timeline = %+v", got)
	}

	for _, values := range []url.Values{
		{"limit": {"2"}},
		{"offset": {"0"}},
		{"before": {"abc"}},
		{"group": {"day"}},
		{"unknown": {"1"}},
		{"tags_mode": {"bogus"}},
		{"tags": {"#x"}},
		{"from": {"abc"}},
		{"from": {strconv.FormatInt(base+1, 10)}, "to": {strconv.FormatInt(base, 10)}},
	} {
		body := tagCheck(t, standalone(values), 400)
		if code := tagErrorCode(t, body); code == "" {
			t.Fatalf("timeline %v has no error code: %s", values, body)
		}
	}
	tagCheck(t, env.request("GET", "/api/v1/images/timeline", "", &env.member, nil), 200)
	tagCheck(t, env.request("GET", "/api/v1/images/timeline?scope=all", "", &env.member, nil), 403)
	adminAll := decodeTimeline(t, tagCheck(t, env.request("GET", "/api/v1/images/timeline?scope=all", "", &env.admin, nil), 200))
	if len(adminAll) != 3 || adminAll[1].Day != "2026-01-02" || adminAll[1].Count != 4 {
		t.Fatalf("Admin timeline = %+v", adminAll)
	}

	env.setVisibility(t, m4, "private")
	expectBuckets(t, timeline(nil), dayThree, dayTwo, dayOne)

	grouped := env.request("GET", "/api/v1/images?group=day", "", &env.member, nil)
	groupedBody := tagCheck(t, grouped, 200)
	if !strings.Contains(string(groupedBody), `"groups"`) || strings.Contains(string(groupedBody), `"buckets"`) {
		t.Fatalf("group=day shape = %s", groupedBody)
	}
	expectBuckets(t, decodeTimeline(t, groupedBody), dayThree, dayTwo, dayOne)
	for _, values := range []url.Values{
		{"group": {"week"}},
		{"group": {"day"}, "limit": {"2"}},
		{"group": {"day"}, "offset": {"0"}},
		{"group": {"day"}, "before": {"abc"}},
	} {
		body := tagCheck(t, env.galleryList(t, &env.member, values), 400)
		if code := tagErrorCode(t, body); code == "" {
			t.Fatalf("group %v has no error code: %s", values, body)
		}
	}
}
