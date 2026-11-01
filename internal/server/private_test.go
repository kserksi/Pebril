package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/store"
)

func privateKeyBody(key string) string {
	encoded, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func (env *visibilityHTTPEnv) verifyKey(t *testing.T, id, key string) *httptest.ResponseRecorder {
	t.Helper()
	return env.request("POST", "/api/v1/private/"+id+"/verify", privateKeyBody(key), nil, nil)
}

func (env *visibilityHTTPEnv) contentRequest(t *testing.T, method, path, grant string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	merged := map[string]string{}
	if grant != "" {
		merged[privateGrantHeader] = grant
	}
	for name, value := range headers {
		merged[name] = value
	}
	return env.request(method, path, "", nil, merged)
}

type privateGrantHTTP struct {
	Grant     string `json:"grant"`
	ExpiresAt int64  `json:"expires_at"`
}

func privateGrantResponse(t *testing.T, recorder *httptest.ResponseRecorder) (privateGrantHTTP, string) {
	t.Helper()
	body := visibilityCheck(t, recorder, 200)
	var value privateGrantHTTP
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("Grant response is not JSON: %s", body)
	}
	if len(value.Grant) != 64 {
		t.Fatalf("Grant %q is not 64 hex characters", value.Grant)
	}
	if _, err := hex.DecodeString(value.Grant); err != nil {
		t.Fatalf("Grant %q is not hex: %v", value.Grant, err)
	}
	if value.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("Grant already expired at %d", value.ExpiresAt)
	}
	return value, string(body)
}

func privateError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	body := visibilityCheck(t, recorder, status)
	var value map[string]string
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("Error response is not a JSON object: %s", body)
	}
	if value["error"] != code {
		t.Fatalf("Error = %q, want %q: %s", value["error"], code, body)
	}
}

// privateContentCheck requires the status, project-wide no-store policy and the
// additional private delivery cache and variation headers.
func privateContentCheck(t *testing.T, recorder *httptest.ResponseRecorder, status int) []byte {
	t.Helper()
	body := visibilityCheck(t, recorder, status)
	for _, header := range []string{"CDN-Cache-Control", "Cloudflare-CDN-Cache-Control"} {
		if value := recorder.Header().Get(header); value != "no-store" {
			t.Fatalf("%s = %q, want no-store", header, value)
		}
	}
	if vary := recorder.Header().Get("Vary"); !strings.Contains(vary, privateGrantHeader) {
		t.Fatalf("Vary = %q, want %s", vary, privateGrantHeader)
	}
	return body
}

func (env *visibilityHTTPEnv) addCover(t *testing.T, id, content string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(digest[:])
	objects := filepath.Join(env.deployment.PublicDir, ".objects")
	if err := os.MkdirAll(objects, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objects, hash), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.DB.Exec("INSERT INTO image_files(image_id,role,hash,bytes,format,width,height,frames) VALUES(?,?,?,?,?,?,?,?)", id, "cover", hash, len(content), "webp", 2, 2, 1); err != nil {
		t.Fatal(err)
	}
	return hash
}

func (env *visibilityHTTPEnv) privateSeed(t *testing.T, content string) (string, string) {
	t.Helper()
	id, hash := env.seed(t, env.member.User.ID, content)
	if err := env.db.SetVisibility(context.Background(), id, store.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}
	return id, hash
}

func (env *visibilityHTTPEnv) issuePrivateKey(t *testing.T, id string) visibilityIssuedHTTP {
	t.Helper()
	issued, _ := visibilityIssued(t, env.request("POST", "/api/v1/images/"+id+"/key", "", &env.member, nil), 201)
	return issued
}

func (env *visibilityHTTPEnv) setVisibility(t *testing.T, id string, visibility store.Visibility) {
	t.Helper()
	body := `{"visibility":"` + string(visibility) + `"}`
	visibilityCheck(t, env.request("POST", "/api/v1/images/"+id+"/visibility", body, &env.member, nil), 200)
}

func TestPrivatePublicPathHidesContent(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	const content = "private path payload bytes"
	id, hash := env.seed(t, env.member.User.ID, content)
	const coverContent = "private cover payload bytes"
	coverHash := env.addCover(t, id, coverContent)

	public := env.request("GET", "/image/"+id, "", nil, nil)
	visibilityPublicBytes(t, public, content)
	if tag := public.Header().Get("ETag"); tag != `"`+hash+`"` {
		t.Fatalf("Public ETag = %q, want quoted hash", tag)
	}
	visibilityPublicBytes(t, env.request("GET", "/image/"+id+"/cover", "", nil, nil), coverContent)
	if err := env.db.SetVisibility(context.Background(), id, store.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/image/" + id, "/image/" + id + "/cover"} {
		for _, method := range []string{"GET", "HEAD"} {
			recorder := env.request(method, path, "", nil, nil)
			visibilityCheck(t, recorder, 404)
			if recorder.Body.Len() != 0 {
				t.Fatalf("%s %s returned a body for a private image: %q", method, path, recorder.Body.String())
			}
			for _, header := range []string{"ETag", "Content-Range", "Content-Disposition", "Content-Length"} {
				if value := recorder.Header().Get(header); value != "" {
					t.Fatalf("%s %s leaked %s = %q", method, path, header, value)
				}
			}
		}
	}
	conditional := env.request("GET", "/image/"+id, "", nil, map[string]string{"Range": "bytes=0-3", "If-None-Match": `"` + hash + `"`})
	visibilityCheck(t, conditional, 404)
	if conditional.Header().Get("Content-Range") != "" || conditional.Body.Len() != 0 {
		t.Fatalf("Conditional private request answered: %q %q", conditional.Header().Get("Content-Range"), conditional.Body.String())
	}
	unknown := env.request("GET", "/image/"+strings.Repeat("0", 32), "", nil, nil)
	visibilityCheck(t, unknown, 404)
	if unknown.Body.Len() != 0 {
		t.Fatalf("Unknown image returned a body: %q", unknown.Body.String())
	}

	landing := env.request("GET", "/p/"+id, "", nil, nil)
	body := visibilityCheck(t, landing, 200)
	if !strings.Contains(string(body), "Private image") {
		t.Fatalf("Landing shell = %s", body)
	}
	for _, secret := range []string{content, coverContent, hash, coverHash} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("Landing shell leaked %q: %s", secret, body)
		}
	}
	if landing.Header().Get("Referrer-Policy") != "no-referrer" || landing.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("Landing privacy headers = %v", landing.Header())
	}

	unknownLanding := env.request("GET", "/p/"+strings.Repeat("0", 32), "", nil, nil)
	unknownBody := visibilityCheck(t, unknownLanding, 404)
	publicID, _ := env.seed(t, env.member.User.ID, "public landing bytes")
	publicLanding := env.request("GET", "/p/"+publicID, "", nil, nil)
	publicBody := visibilityCheck(t, publicLanding, 404)
	if string(unknownBody) != string(publicBody) {
		t.Fatalf("Unknown and public landing responses differ: %q vs %q", unknownBody, publicBody)
	}

	env.setVisibility(t, id, store.VisibilityPublic)
	visibilityPublicBytes(t, env.request("GET", "/image/"+id, "", nil, nil), content)
	visibilityPublicBytes(t, env.request("GET", "/image/"+id+"/cover", "", nil, nil), coverContent)
}

func TestPrivateVerifyGrantsContent(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	const content = "verified private content bytes"
	id, hash := env.privateSeed(t, content)
	const coverContent = "verified cover content bytes"
	env.addCover(t, id, coverContent)
	issued := env.issuePrivateKey(t, id)
	key := issued.Key

	for _, body := range []string{
		``, `null`, `[]`, `{}`, `{"key":null}`, `{"key":1}`, `{"key":true}`, `{"key":""}`,
		`{"key":"AAAA"}`, `{"key":"` + key + `","extra":1}`, `{"key":"` + key + `"} {}`, `{"key":"` + strings.Repeat("A", 129) + `"}`,
	} {
		privateError(t, env.request("POST", "/api/v1/private/"+id+"/verify", body, nil, nil), 400, "invalid_request")
	}
	wrong := "Z" + strings.Repeat("0", 25)
	privateError(t, env.verifyKey(t, id, wrong), 401, "key_invalid")
	unknownID := strings.Repeat("0", 32)
	unknown := env.verifyKey(t, unknownID, wrong)
	privateError(t, unknown, 401, "key_invalid")
	if mismatched := env.verifyKey(t, id, wrong); mismatched.Body.String() != unknown.Body.String() {
		t.Fatalf("Wrong key and unknown record are distinguishable: %q vs %q", mismatched.Body.String(), unknown.Body.String())
	}
	privateError(t, env.request("POST", "/api/v1/private/"+id+"/verify", privateKeyBody(key), nil, map[string]string{"Authorization": "Bearer value"}), 403, "forbidden")
	privateError(t, env.request("POST", "/api/v1/private/"+id+"/verify", privateKeyBody(key), nil, map[string]string{"Origin": "https://attacker.test"}), 403, "forbidden")

	grantRecorder := env.verifyKey(t, id, key)
	grant, grantRaw := privateGrantResponse(t, grantRecorder)
	digest := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(digest[:])
	for _, secret := range []string{key, keyHash, hash, "hash"} {
		if strings.Contains(grantRaw, secret) {
			t.Fatalf("Verify response leaked %q: %s", secret, grantRaw)
		}
	}
	if cookie := grantRecorder.Header().Get("Set-Cookie"); cookie != "" {
		t.Fatalf("Verify response set a cookie: %s", cookie)
	}
	if location := grantRecorder.Header().Get("Location"); location != "" {
		t.Fatalf("Verify response redirected with a grant: %s", location)
	}
	if deadline := time.Now().UnixMilli() + int64(privateGrantLifetime/time.Millisecond); grant.ExpiresAt > deadline+5000 {
		t.Fatalf("Grant expiry %d exceeds the 30 minute lifetime", grant.ExpiresAt)
	}
	if grant.ExpiresAt > issued.ExpiresAt {
		t.Fatalf("Grant expiry %d outlives key expiry %d", grant.ExpiresAt, issued.ExpiresAt)
	}

	path := "/p/" + id + "/content"
	privateError(t, env.contentRequest(t, "GET", path, "", nil), 401, "grant_required")
	privateError(t, env.contentRequest(t, "GET", path, "short", nil), 401, "grant_required")
	privateError(t, env.contentRequest(t, "GET", path, strings.Repeat("a", 64), nil), 401, "grant_required")

	served := env.contentRequest(t, "GET", path, grant.Grant, nil)
	body := privateContentCheck(t, served, 200)
	if string(body) != content {
		t.Fatalf("Private bytes = %q, want %q", body, content)
	}
	if got := served.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := served.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if disposition := served.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "inline") {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	if served.Header().Get("ETag") != "" || served.Header().Get("Last-Modified") != "" {
		t.Fatalf("Private content has validators: %v", served.Header())
	}
	if cookie := served.Header().Get("Set-Cookie"); cookie != "" {
		t.Fatalf("Private content set a cookie: %s", cookie)
	}
	if referrer := served.Header().Get("Referrer-Policy"); referrer != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", referrer)
	}

	head := env.contentRequest(t, "HEAD", path, grant.Grant, nil)
	privateContentCheck(t, head, 200)
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD returned a body: %q", head.Body.String())
	}
	if got := head.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("HEAD Accept-Ranges = %q", got)
	}

	ranged := env.contentRequest(t, "GET", path, grant.Grant, map[string]string{"Range": "bytes=0-3"})
	rangedBody := privateContentCheck(t, ranged, 206)
	if string(rangedBody) != content[:4] {
		t.Fatalf("Range bytes = %q", rangedBody)
	}
	if expected := fmt.Sprintf("bytes 0-3/%d", len(content)); ranged.Header().Get("Content-Range") != expected {
		t.Fatalf("Content-Range = %q, want %q", ranged.Header().Get("Content-Range"), expected)
	}
	conditional := env.contentRequest(t, "GET", path, grant.Grant, map[string]string{"If-None-Match": `"` + hash + `"`})
	if string(privateContentCheck(t, conditional, 200)) != content {
		t.Fatalf("Conditional private request did not serve bytes")
	}

	cover := env.contentRequest(t, "GET", "/p/"+id+"/content/cover", grant.Grant, nil)
	if string(privateContentCheck(t, cover, 200)) != coverContent {
		t.Fatalf("Private cover bytes = %q", cover.Body.String())
	}
	if got := cover.Header().Get("Content-Type"); got != "image/webp" {
		t.Fatalf("Cover Content-Type = %q", got)
	}

	otherID, _ := env.privateSeed(t, "other private content")
	otherKey := env.issuePrivateKey(t, otherID)
	otherGrant, _ := privateGrantResponse(t, env.verifyKey(t, otherID, otherKey.Key))
	privateContentCheck(t, env.contentRequest(t, "GET", "/p/"+otherID+"/content", otherGrant.Grant, nil), 200)
	privateError(t, env.contentRequest(t, "GET", "/p/"+otherID+"/content", grant.Grant, nil), 401, "grant_required")
	privateError(t, env.verifyKey(t, otherID, key), 401, "key_invalid")

	privateError(t, env.contentRequest(t, "GET", path, "", map[string]string{"Cookie": sessionCookie + "=" + env.member.Token}), 401, "grant_required")
}

func TestPrivateGrantLifetimeAndInvalidation(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	grants := pageGrantsFor(env.upload)
	now := time.Now()
	long, expires, err := grants.issue(store.KeyGrant{ImageID: "first", Generation: 1, ExpiresAt: now.Add(2 * time.Hour).UnixMilli(), Fingerprint: "first-fingerprint"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if expected := now.Add(privateGrantLifetime).UnixMilli(); expires != expected {
		t.Fatalf("Grant expiry = %d, want grant lifetime %d", expires, expected)
	}
	short, shortExpires, err := grants.issue(store.KeyGrant{ImageID: "second", Generation: 1, ExpiresAt: now.Add(time.Minute).UnixMilli(), Fingerprint: "second-fingerprint"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if expected := now.Add(time.Minute).UnixMilli(); shortExpires != expected {
		t.Fatalf("Grant expiry = %d, want key expiry %d", shortExpires, expected)
	}
	if _, ok := grants.use(long, "second", now); ok {
		t.Fatal("Grant is usable for another record")
	}
	third, _, err := grants.issue(store.KeyGrant{ImageID: "third", Generation: 1, ExpiresAt: now.Add(time.Hour).UnixMilli(), Fingerprint: "third-fingerprint"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := grants.use(third, "third", now.Add(privateGrantLifetime)); ok {
		t.Fatal("Expired grant is usable")
	}
	grants.invalidate("second")
	if _, ok := grants.use(short, "second", now); ok {
		t.Fatal("Grant survived visibility invalidation")
	}
	if _, ok := grants.use(long, "first", now); !ok {
		t.Fatal("Live grant was invalidated")
	}
}

func TestPrivateGrantLifecycle(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	const content = "lifecycle content bytes"
	id, _ := env.privateSeed(t, content)
	contentPath := "/p/" + id + "/content"
	first := env.issuePrivateKey(t, id)
	firstGrant, _ := privateGrantResponse(t, env.verifyKey(t, id, first.Key))
	privateContentCheck(t, env.contentRequest(t, "GET", contentPath, firstGrant.Grant, nil), 200)

	rotated := env.issuePrivateKey(t, id)
	if rotated.Generation != 2 {
		t.Fatalf("Rotated generation = %d", rotated.Generation)
	}
	privateError(t, env.contentRequest(t, "GET", contentPath, firstGrant.Grant, nil), 401, "grant_stale")
	privateError(t, env.verifyKey(t, id, first.Key), 401, "key_invalid")
	secondGrant, _ := privateGrantResponse(t, env.verifyKey(t, id, rotated.Key))
	privateContentCheck(t, env.contentRequest(t, "GET", contentPath, secondGrant.Grant, nil), 200)

	visibilityNoContent(t, env.request("DELETE", "/api/v1/images/"+id+"/key", "", &env.member, nil))
	privateError(t, env.contentRequest(t, "GET", contentPath, secondGrant.Grant, nil), 401, "grant_required")

	reissued := env.issuePrivateKey(t, id)
	if reissued.Generation != 1 {
		t.Fatalf("Replacement generation = %d", reissued.Generation)
	}
	thirdGrant, _ := privateGrantResponse(t, env.verifyKey(t, id, reissued.Key))
	privateContentCheck(t, env.contentRequest(t, "GET", contentPath, thirdGrant.Grant, nil), 200)
	// The replacement reused generation 1: a grant from the previous generation 2
	// is reported stale, while a generation 1 grant from the revoked key is
	// rejected because the key hash no longer matches.
	privateError(t, env.contentRequest(t, "GET", contentPath, secondGrant.Grant, nil), 401, "grant_stale")
	privateError(t, env.contentRequest(t, "GET", contentPath, firstGrant.Grant, nil), 401, "grant_required")

	otherID, _ := env.privateSeed(t, "other lifecycle content")
	otherKey := env.issuePrivateKey(t, otherID)
	otherGrant, _ := privateGrantResponse(t, env.verifyKey(t, otherID, otherKey.Key))
	privateContentCheck(t, env.contentRequest(t, "GET", "/p/"+otherID+"/content", otherGrant.Grant, nil), 200)
	privateError(t, env.contentRequest(t, "GET", "/p/"+otherID+"/content", thirdGrant.Grant, nil), 401, "grant_required")
	privateError(t, env.verifyKey(t, otherID, reissued.Key), 401, "key_invalid")
	visibilityNoContent(t, env.request("DELETE", "/api/v1/images/"+otherID+"/key", "", &env.member, nil))
	privateError(t, env.verifyKey(t, otherID, otherKey.Key), 403, "key_revoked")
	privateError(t, env.contentRequest(t, "GET", "/p/"+otherID+"/content", otherGrant.Grant, nil), 401, "grant_required")

	// The verify endpoint limits attempts per record, so deleted/public record
	// checks use separate records rather than exhausting this record's budget.
	publicID, _ := env.seed(t, env.member.User.ID, "public lifecycle content")
	privateError(t, env.verifyKey(t, publicID, first.Key), 401, "key_invalid")

	env.setVisibility(t, id, store.VisibilityPublic)
	privateError(t, env.contentRequest(t, "GET", contentPath, thirdGrant.Grant, nil), 404, "not_found")
	env.setVisibility(t, id, store.VisibilityPrivate)
	privateError(t, env.contentRequest(t, "GET", contentPath, thirdGrant.Grant, nil), 401, "grant_required")
	fourthGrant, _ := privateGrantResponse(t, env.verifyKey(t, id, reissued.Key))
	privateContentCheck(t, env.contentRequest(t, "GET", contentPath, fourthGrant.Grant, nil), 200)

	if _, err := env.db.DeleteImage(id, env.member.User.ID, false); err != nil {
		t.Fatal(err)
	}
	privateError(t, env.contentRequest(t, "GET", contentPath, fourthGrant.Grant, nil), 404, "not_found")
	if _, err := env.db.DeleteImage(otherID, env.member.User.ID, false); err != nil {
		t.Fatal(err)
	}
	privateError(t, env.verifyKey(t, otherID, otherKey.Key), 401, "key_invalid")
}

func TestPrivateContentAuthorizationBeforeRange(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	const content = "authorization ordering bytes"
	id, hash := env.privateSeed(t, content)
	key := env.issuePrivateKey(t, id)
	grant, _ := privateGrantResponse(t, env.verifyKey(t, id, key.Key))
	path := "/p/" + id + "/content"

	unauthorized := env.contentRequest(t, "GET", path, "", map[string]string{"Range": "bytes=0-3", "If-None-Match": `"` + hash + `"`})
	privateError(t, unauthorized, 401, "grant_required")
	if unauthorized.Header().Get("Content-Range") != "" {
		t.Fatalf("Unauthorized range response has Content-Range = %q", unauthorized.Header().Get("Content-Range"))
	}
	authorized := env.contentRequest(t, "GET", path, grant.Grant, map[string]string{"Range": "bytes=0-3"})
	if body := privateContentCheck(t, authorized, 206); string(body) != content[:4] {
		t.Fatalf("Authorized range bytes = %q", body)
	}

	if _, err := env.db.DB.Exec("UPDATE image_keys SET expires_at=? WHERE image_id=?", time.Now().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	privateError(t, env.contentRequest(t, "GET", path, grant.Grant, nil), 401, "grant_required")
	privateError(t, env.verifyKey(t, id, key.Key), 403, "key_expired")

	replacement := env.issuePrivateKey(t, id)
	replacementGrant, _ := privateGrantResponse(t, env.verifyKey(t, id, replacement.Key))
	privateContentCheck(t, env.contentRequest(t, "GET", "/p/"+id+"/content/original", replacementGrant.Grant, nil), 200)
	if disposition := env.contentRequest(t, "GET", "/p/"+id+"/content/original", replacementGrant.Grant, nil).Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") {
		t.Fatalf("Original Content-Disposition = %q", disposition)
	}
	image, err := env.db.Image(id)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := image.Policy
	snapshot.Policy.AllowOriginalDownload = false
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.DB.Exec("UPDATE images SET policy=? WHERE id=?", encoded, id); err != nil {
		t.Fatal(err)
	}
	privateError(t, env.contentRequest(t, "GET", "/p/"+id+"/content/original", replacementGrant.Grant, nil), 404, "not_found")
	privateContentCheck(t, env.contentRequest(t, "GET", path, replacementGrant.Grant, nil), 200)

	if _, err := env.db.DB.Exec("UPDATE users SET status='banned' WHERE id=?", env.member.User.ID); err != nil {
		t.Fatal(err)
	}
	privateError(t, env.contentRequest(t, "GET", path, replacementGrant.Grant, nil), 404, "not_found")
	privateError(t, env.verifyKey(t, id, replacement.Key), 401, "key_invalid")
}

func TestPrivateVerifyRateLimits(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	unknown := strings.Repeat("1", 32)
	wrong := "Z" + strings.Repeat("0", 25)
	for attempt := 1; attempt <= privateVerifyRecordLimit; attempt++ {
		privateError(t, env.verifyKey(t, unknown, wrong), 401, "key_invalid")
	}
	blocked := env.verifyKey(t, unknown, wrong)
	privateError(t, blocked, 429, "too_many_attempts")
	seconds, err := strconv.Atoi(blocked.Header().Get("Retry-After"))
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q", blocked.Header().Get("Retry-After"))
	}
	privateError(t, env.verifyKey(t, unknown, wrong), 429, "too_many_attempts")

	env = newVisibilityHTTPEnv(t)
	for attempt := 1; attempt <= privateVerifyIPLimit; attempt++ {
		privateError(t, env.verifyKey(t, fmt.Sprintf("%032x", attempt), wrong), 401, "key_invalid")
	}
	privateError(t, env.verifyKey(t, fmt.Sprintf("%032x", privateVerifyIPLimit+1), wrong), 429, "too_many_attempts")
}

func TestPrivateNoSecretsInResponses(t *testing.T) {
	var logged bytes.Buffer
	previous := gin.DefaultWriter
	gin.DefaultWriter = &logged
	t.Cleanup(func() { gin.DefaultWriter = previous })
	env := newVisibilityHTTPEnv(t)
	const content = "secret response bytes"
	id, hash := env.privateSeed(t, content)
	key := env.issuePrivateKey(t, id)
	grant, grantRaw := privateGrantResponse(t, env.verifyKey(t, id, key.Key))
	digest := sha256.Sum256([]byte(key.Key))
	keyHash := hex.EncodeToString(digest[:])
	if !strings.Contains(grantRaw, grant.Grant) {
		t.Fatalf("Grant response does not carry its grant: %s", grantRaw)
	}
	responses := []*httptest.ResponseRecorder{
		env.request("GET", "/p/"+id, "", nil, nil),
		env.verifyKey(t, id, key.Key),
		env.contentRequest(t, "GET", "/p/"+id+"/content", grant.Grant, nil),
		env.contentRequest(t, "GET", "/p/"+id+"/content", "", nil),
		env.verifyKey(t, id, "Z"+strings.Repeat("0", 25)),
	}
	for _, recorder := range responses {
		headers := strings.Builder{}
		for name, values := range recorder.Header() {
			headers.WriteString(strings.ToLower(name + "=" + strings.Join(values, ",") + "\n"))
		}
		combined := strings.ToLower(recorder.Body.String() + headers.String())
		for label, secret := range map[string]string{
			"plaintext key": strings.ToLower(key.Key),
			"key hash":      keyHash,
			"stored hash":   hash,
		} {
			if strings.Contains(combined, secret) {
				t.Fatalf("Response leaked %s: body=%q headers=%q", label, recorder.Body.String(), headers.String())
			}
		}
		if cookies := recorder.Header().Values("Set-Cookie"); len(cookies) != 0 {
			t.Fatalf("Response set cookies: %v", cookies)
		}
	}
	for _, secret := range []string{content, hash, keyHash, key.Key} {
		if strings.Contains(grantRaw, secret) {
			t.Fatalf("Grant response leaked %q: %s", secret, grantRaw)
		}
	}
	if logged.Len() != 0 {
		t.Fatalf("Private delivery logged output: %s", logged.String())
	}
}

// TestPrivateResultFallsBackToOriginal covers the frozen private-page fallback:
// a completed record whose converted result is gone serves the stored original
// under the same authorized visit.
func TestPrivateResultFallsBackToOriginal(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	const content = "private fallback bytes"
	id, _ := env.privateSeed(t, content)
	key := env.issuePrivateKey(t, id)
	grant, _ := privateGrantResponse(t, env.verifyKey(t, id, key.Key))
	if _, err := env.db.DB.Exec("DELETE FROM image_files WHERE image_id=? AND role='result'", id); err != nil {
		t.Fatal(err)
	}
	recorder := env.contentRequest(t, "GET", "/p/"+id+"/content", grant.Grant, nil)
	if body := privateContentCheck(t, recorder, 200); string(body) != content {
		t.Fatalf("Fallback bytes = %q", body)
	}
}
