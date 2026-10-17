package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestGalleryHTTPPermissionsAndOriginals(t *testing.T) {
	deployment, err := config.LoadDeployment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := config.New(deployment, true)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	db, err := store.Open(deployment.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
	clients := map[int64]auth.Session{}
	for _, user := range []store.User{admin, member, other} {
		session, e := authentication.Login(user.Username, secret)
		if e != nil {
			t.Fatal(e)
		}
		clients[user.ID] = session
	}
	upload, err := uploads.NewWithResources(db, manager, deployment, "/bin/false", nativeprobe.Result{}, &httpResourceState{state: resources.State{Cooldown: true, Concurrency: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer upload.Close()
	handler := HandlerWithUploads("0.0.8-dev", manager, db, nativeprobe.Result{Ready: true}, authentication, upload)
	request := func(method, path string, user int64, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://example.test"+path, nil)
		if session, ok := clients[user]; ok {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
			req.Header.Set("X-CSRF-Token", session.CSRF)
		}
		req.Header.Set("Origin", "https://example.test")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, req)
		return r
	}
	check := func(r *httptest.ResponseRecorder, status int) {
		t.Helper()
		if r.Code != status {
			t.Fatalf("HTTP %d, want %d: %s", r.Code, status, r.Body.String())
		}
		if r.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("Gallery response is cacheable")
		}
	}
	seed := func(owner int64, content string, keep bool) (string, string) {
		id, e := uploads.NewID()
		if e != nil {
			t.Fatal(e)
		}
		policy := manager.Snapshot()
		if e = db.AcceptUpload(id, owner, policy); e != nil {
			t.Fatal(e)
		}
		if e = db.Received(id); e != nil {
			t.Fatal(e)
		}
		digest := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(digest[:])
		if e = os.WriteFile(filepath.Join(deployment.PublicDir, ".objects", hash), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		image := store.Image{ID: id, UserID: owner, State: "queued", Hash: hash, Format: "png", Bytes: int64(len(content)), Width: 2, Height: 2, Frames: 1}
		if e = db.SaveUpload(image); e != nil {
			t.Fatal(e)
		}
		image, e = db.ClaimConversion(id)
		if e != nil {
			t.Fatal(e)
		}
		files := []store.ImageFile{{Role: "result", Hash: hash, Format: "png", Bytes: image.Bytes, Width: 2, Height: 2, Frames: 1}}
		if keep {
			original := files[0]
			original.Role = "original"
			files = append(files, original)
		}
		if e = db.FinishConversion(image, files); e != nil {
			t.Fatal(e)
		}
		return id, hash
	}
	first, hash := seed(member.ID, "shared original bytes", true)
	second, _ := seed(member.ID, "shared original bytes", true)
	missing, _ := seed(other.ID, "converted result only", false)
	for _, path := range []string{"/api/v1/images", "/api/v1/images/" + first, "/api/v1/images/" + first + "/original"} {
		check(request("GET", path, 0, nil), 401)
		check(request("GET", path, member.ID, map[string]string{"Authorization": "Bearer upload-only"}), 403)
	}
	check(request("GET", "/api/v1/images?scope=all", member.ID, nil), 403)
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=01", "offset=-1", "offset=1.5", "limit=1&limit=2", "scope=private", "unknown=1"} {
		check(request("GET", "/api/v1/images?"+query, member.ID, nil), 400)
	}
	page := request("GET", "/api/v1/images?limit=1&offset=1", member.ID, nil)
	check(page, 200)
	var list struct {
		Images []galleryImage `json:"images"`
		Total  int            `json:"total"`
	}
	if err = json.Unmarshal(page.Body.Bytes(), &list); err != nil || list.Total != 2 || len(list.Images) != 1 {
		t.Fatal("Owner pagination", list, err)
	}
	if list.Images[0].OwnerID != member.ID || !list.Images[0].OriginalAvailable || len(list.Images[0].Files) != 2 {
		t.Fatal("Missing gallery metadata", list)
	}
	for _, forbidden := range []string{hash, `"policy"`, `"generation"`, `"Hash"`} {
		if strings.Contains(page.Body.String(), forbidden) {
			t.Fatal("Private storage metadata exposed", forbidden)
		}
	}
	all := request("GET", "/api/v1/images?scope=all", admin.ID, nil)
	check(all, 200)
	if err = json.Unmarshal(all.Body.Bytes(), &list); err != nil || list.Total != 3 {
		t.Fatal("Administrator gallery", err)
	}
	check(request("GET", "/api/v1/images/"+first, other.ID, nil), 404)
	check(request("DELETE", "/api/v1/images/"+first, other.ID, nil), 404)
	check(request("GET", "/api/v1/images/"+first+"/original", other.ID, nil), 404)
	check(request("GET", "/api/v1/images/"+missing+"/original", other.ID, nil), 404)
	check(request("GET", "/api/v1/images/"+first, admin.ID, nil), 200)
	download := request("GET", "/api/v1/images/"+first+"/original", member.ID, nil)
	check(download, 200)
	if download.Body.String() != "shared original bytes" || download.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(download.Header().Get("Content-Disposition"), "attachment;") || download.Header().Get("CDN-Cache-Control") != "no-store" {
		t.Fatal("Original response bytes/headers", download.Header())
	}
	partial := request("GET", "/api/v1/images/"+first+"/original", admin.ID, map[string]string{"Range": "bytes=0-5"})
	check(partial, 206)
	if partial.Body.String() != "shared" {
		t.Fatal("Original range response", partial.Body.String())
	}
	policy := manager.Snapshot().Policy
	policy.AllowOriginalDownload = false
	encoded, _ := json.Marshal(policy)
	if err = os.WriteFile(filepath.Join(deployment.Root, "config.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err = manager.Reload(); err != nil {
		t.Fatal(err)
	}
	check(request("GET", "/api/v1/images/"+first+"/original", admin.ID, nil), 403)
	detail := request("GET", "/api/v1/images/"+first, member.ID, nil)
	check(detail, 200)
	var value galleryImage
	if err = json.Unmarshal(detail.Body.Bytes(), &value); err != nil || value.OriginalAvailable {
		t.Fatal("Disabled original advertised", err)
	}
	check(request("DELETE", "/api/v1/images/"+first, member.ID, map[string]string{"Origin": "https://foreign.test"}), 403)
	check(request("DELETE", "/api/v1/images/"+first, member.ID, map[string]string{"X-CSRF-Token": "invalid"}), 403)
	check(request("DELETE", "/api/v1/images/"+first, member.ID, map[string]string{"Authorization": "Bearer upload-only"}), 403)
	check(request("DELETE", "/api/v1/images/"+first, member.ID, nil), 200)
	check(request("GET", "/api/v1/images/"+first, admin.ID, nil), 404)
	if _, err = os.Stat(filepath.Join(deployment.PublicDir, ".objects", hash)); err != nil {
		t.Fatal("Shared object removed", err)
	}
	owner, err := db.User(member.ID)
	if err != nil || owner.Used != int64(len("shared original bytes")) {
		t.Fatal("Distinct-role charge not released correctly", owner, err)
	}
	check(request("DELETE", "/api/v1/images/"+second, admin.ID, nil), 200)
	if _, err = os.Stat(filepath.Join(deployment.PublicDir, ".objects", hash)); !os.IsNotExist(err) {
		t.Fatal("Last unreferenced object retained", err)
	}
	check(request("DELETE", "/api/v1/images/"+first, member.ID, nil), 404)
}
