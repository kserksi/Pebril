package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/objectstore"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/testutil"
	"github.com/kserksi/Pebril/internal/uploads"
)

func TestS3HTTPDeliveryAuthorizationAndSharedDeletion(t *testing.T) {
	fixture, s3Server := testutil.NewS3(t)
	previous := http.DefaultTransport
	http.DefaultTransport = s3Server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	d, err := config.LoadDeployment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	connection := config.Storage{Backend: "s3", S3: &config.S3{Endpoint: s3Server.URL, Bucket: "images", Region: "test-region", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Addressing: "path", Prefix: "delivery/"}}
	data, _ := json.Marshal(struct {
		config.Runtime
		Storage config.Storage `json:"storage"`
	}{config.Defaults(), connection})
	if err = os.WriteFile(filepath.Join(d.Root, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.New(d, false)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	db, err := store.OpenDeployment(d)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	authentication, err := auth.New(db)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := authentication.Initialize("owner", "password")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := authentication.Register("stranger", "password")
	if err != nil {
		t.Fatal(err)
	}
	ownSession, err := authentication.NewSession(owner)
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := authentication.NewSession(stranger)
	if err != nil {
		t.Fatal(err)
	}
	service, err := uploads.New(db, manager, d, "/bin/false", nativeprobe.Result{Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	selected, err := objectstore.New(context.Background(), filepath.Join(d.PublicDir, ".objects"), connection, db.Namespace())
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	if err = png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	body := buffer.Bytes()
	input := filepath.Join(d.Root, "input")
	os.WriteFile(input, body, 0600)
	hash, size, err := selected.Publish(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		if err = db.AcceptUpload(id, owner.ID, manager.Snapshot()); err != nil {
			t.Fatal(err)
		}
		if err = db.Received(id); err != nil {
			t.Fatal(err)
		}
		if err = db.SaveUpload(store.Image{ID: id, UserID: owner.ID, State: "queued", Hash: hash, Bytes: size, Format: "png", Width: 2, Height: 2, Frames: 1}); err != nil {
			t.Fatal(err)
		}
		image, err := db.ClaimConversion(id)
		if err != nil {
			t.Fatal(err)
		}
		original := store.FileRole(image.Files, "original")
		result := original
		result.Role = "result"
		if err = db.FinishConversion(image, []store.ImageFile{original, result}); err != nil {
			t.Fatal(err)
		}
	}
	handler := HandlerWithUploads("0.0.10-dev", manager, db, nativeprobe.Result{Ready: true}, authentication, service)
	id := strings.Repeat("a", 32)
	url := "/image/" + id
	request := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	response := request("GET", url, nil)
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), body) || response.Header().Get("Last-Modified") == "" {
		t.Fatal("Proxied public bytes/validators failed", response.Code)
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	if response.Header().Get("ETag") != etag {
		t.Fatal("S3 ETag replaced the application content hash")
	}
	for _, method := range []string{"HEAD", "GET"} {
		headers := map[string]string{"Range": "bytes=3-8"}
		response = request(method, url, headers)
		if response.Code != 206 || response.Header().Get("Content-Range") != "bytes 3-8/"+strconv.Itoa(len(body)) {
			t.Fatal("Range delivery changed", response.Code)
		}
		if method == "HEAD" && response.Body.Len() != 0 {
			t.Fatal("HEAD delivered a body")
		}
		if method == "GET" && !bytes.Equal(response.Body.Bytes(), body[3:9]) {
			t.Fatal("Range bytes changed")
		}
	}
	if response = request("GET", url, map[string]string{"If-None-Match": etag}); response.Code != 304 {
		t.Fatal("Conditional request was not honored", response.Code)
	}
	if response = request("GET", url, map[string]string{"Range": "bytes=3-8", "If-Range": "different"}); response.Code != 200 || !bytes.Equal(response.Body.Bytes(), body) {
		t.Fatal("If-Range mismatch did not return complete representation")
	}
	original := "/api/v1/images/" + id + "/original"
	if response = resourceRequest(handler, "GET", original, &otherSession, "", nil); response.Code != 404 {
		t.Fatal("Cross-owner original was exposed", response.Code)
	}
	if response = resourceRequest(handler, "HEAD", original, &ownSession, "", nil); response.Code != 200 || response.Body.Len() != 0 || !strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("Authenticated original HEAD failed", response.Code)
	}
	if response = resourceRequest(handler, "GET", original, &ownSession, "", nil); response.Code != 200 || !bytes.Equal(response.Body.Bytes(), body) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("Original forwarding/cache rule failed", response.Code)
	}
	if err = service.Delete(id, owner.ID, false); err != nil {
		t.Fatal(err)
	}
	if response = request("GET", url, nil); response.Code != 404 {
		t.Fatal("Deleted origin link remained available", response.Code)
	}
	fixture.Mu.Lock()
	_, exists := fixture.Objects["delivery/objects/"+hash]
	fixture.Mu.Unlock()
	if !exists {
		t.Fatal("Another record's shared object was deleted")
	}
	if err = service.Delete(strings.Repeat("b", 32), owner.ID, false); err != nil {
		t.Fatal(err)
	}
	fixture.Mu.Lock()
	_, exists = fixture.Objects["delivery/objects/"+hash]
	fixture.Mu.Unlock()
	if exists {
		t.Fatal("Last-reference object was not collected")
	}
	fixture.Mu.Lock()
	fixture.Denied = true
	fixture.Mu.Unlock()
	if response = request("GET", "/healthz", nil); response.Code != 503 {
		t.Fatal("Unavailable selected S3 backend remained ready", response.Code)
	}
}
