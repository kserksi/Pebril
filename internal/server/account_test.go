package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
)

func TestAccountOriginCSRFAndTokenScopes(t *testing.T) {
	d, err := config.LoadDeployment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := config.New(d, true)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	db, err := store.Open(d.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := Handler("test", m, db, nativeprobe.Result{Ready: true})
	var secret [16]byte
	if _, err = rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	credential := hex.EncodeToString(secret[:])
	body, _ := json.Marshal(map[string]string{"username": credential, "password": credential})
	request := func(method, path, origin, csrf, bearer string, cookie *http.Cookie, payload []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://example.test"+path, bytes.NewReader(payload))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		return r
	}
	if r := request("POST", "/api/v1/initialize", "https://attacker.test", "", "", nil, body); r.Code != 403 {
		t.Fatal("Cross-origin initialization accepted")
	}
	r := request("POST", "/api/v1/initialize", "https://example.test", "", "", nil, body)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	cookies := r.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("Cookie protections missing")
	}
	cookie := cookies[0]
	var result struct {
		CSRF string `json:"csrf"`
	}
	if err = json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if r = request("POST", "/api/v1/upload-token", "https://example.test", "", "", cookie, nil); r.Code != 403 {
		t.Fatal("Missing CSRF accepted")
	}
	r = request("POST", "/api/v1/upload-token", "https://example.test", result.CSRF, "", cookie, nil)
	if r.Code != 201 {
		t.Fatal(r.Code)
	}
	var token struct {
		Token string `json:"token"`
	}
	json.Unmarshal(r.Body.Bytes(), &token)
	for _, path := range []string{"/api/v1/account", "/api/v1/upload-token"} {
		method := "GET"
		if strings.HasSuffix(path, "upload-token") {
			method = "POST"
		}
		if r = request(method, path, "https://example.test", result.CSRF, token.Token, cookie, nil); r.Code != 403 {
			t.Fatal("Upload token escaped its scope", path, r.Code)
		}
	}
	if r = request("POST", "/api/v1/logout", "https://example.test", result.CSRF, "", cookie, nil); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if r = request("POST", "/api/v1/upload-token", "https://example.test", result.CSRF, "", cookie, nil); r.Code != 401 {
		t.Fatal("Logged-out cookie accepted")
	}
}
