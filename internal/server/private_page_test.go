package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// privatePageCheck requires the private landing privacy headers and returns the
// page so callers can assert its content.
func privatePageCheck(t *testing.T, recorder *httptest.ResponseRecorder, status int) string {
	t.Helper()
	body := string(visibilityCheck(t, recorder, status))
	for _, header := range []string{"CDN-Cache-Control", "Cloudflare-CDN-Cache-Control"} {
		if value := recorder.Header().Get(header); value != "no-store" {
			t.Fatalf("%s = %q, want no-store", header, value)
		}
	}
	if value := recorder.Header().Get("Referrer-Policy"); value != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", value)
	}
	if value := recorder.Header().Get("X-Robots-Tag"); value != "noindex" {
		t.Fatalf("X-Robots-Tag = %q, want noindex", value)
	}
	if value := recorder.Header().Get("X-Content-Type-Options"); value != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", value)
	}
	if value := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(value, "default-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", value)
	}
	return body
}

func TestPrivatePageShell(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	const content = "private page shell payload"
	id, hash := env.privateSeed(t, content)

	recorder := env.request("GET", "/p/"+id, "", nil, nil)
	body := privatePageCheck(t, recorder, http.StatusOK)
	if value := recorder.Header().Get("Content-Type"); value != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", value)
	}
	csp := recorder.Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "style-src 'self'", "img-src 'self' blob:", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("Content-Security-Policy = %q is missing %q", csp, want)
		}
	}
	for _, banned := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(csp, banned) {
			t.Fatalf("Content-Security-Policy = %q contains %q", csp, banned)
		}
	}
	for _, want := range []string{
		`data-image-id="` + id + `"`,
		`id="private-owner">member<`,
		`id="private-form"`,
		`method="post"`,
		`id="key"`,
		`id="private-submit"`,
		`<noscript>`,
		`src="/private.js"`,
		`href="/style.css"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Private page is missing %q: %s", want, body)
		}
	}
	if strings.Count(body, "<script") != 1 {
		t.Fatalf("Private page has an inline or extra script: %s", body)
	}
	hiddenForm := regexp.MustCompile(`(?s)<form id="private-form"[^>]*hidden`)
	if !hiddenForm.MatchString(body) {
		t.Fatalf("Private page form is not hidden before the script runs: %s", body)
	}
	for _, secret := range []string{content, hash, "X-Pebril-Grant"} {
		if strings.Contains(body, secret) {
			t.Fatalf("Private page leaked %q: %s", secret, body)
		}
	}
}

func TestPrivatePageUnavailable(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	unknownID := strings.Repeat("0", 32)
	publicID, _ := env.seed(t, env.member.User.ID, "public page payload")
	privateID, _ := env.privateSeed(t, "private page payload")
	deletedID, _ := env.privateSeed(t, "deleted page payload")
	if _, err := env.db.DB.Exec("DELETE FROM images WHERE id=?", deletedID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.DB.Exec("UPDATE users SET status='disabled' WHERE id=?", env.member.User.ID); err != nil {
		t.Fatal(err)
	}

	bodies := map[string]string{}
	for name, id := range map[string]string{
		"unknown":        unknownID,
		"public":         publicID,
		"deleted":        deletedID,
		"inactive owner": privateID,
	} {
		body := privatePageCheck(t, env.request("GET", "/p/"+id, "", nil, nil), http.StatusNotFound)
		if strings.Contains(body, id) || strings.Contains(body, "member") {
			t.Fatalf("%s landing echoed record data: %s", name, body)
		}
		if !strings.Contains(body, "This page is not available.") {
			t.Fatalf("%s landing is not the generic page: %s", name, body)
		}
		bodies[name] = body
	}
	want := bodies["unknown"]
	for name, body := range bodies {
		if body != want {
			t.Fatalf("%s and unknown landing responses differ: %q vs %q", name, body, want)
		}
	}
}

func TestPrivatePageEscaping(t *testing.T) {
	var page bytes.Buffer
	data := privatePageData{
		ImageID: `"><script>alert(1)</script>`,
		Owner:   `<img src=x onerror=alert(2)>`,
	}
	if err := privatePageTemplate.ExecuteTemplate(&page, "private.html", data); err != nil {
		t.Fatal(err)
	}
	body := page.String()
	for _, raw := range []string{"<script>alert(1)</script>", "<img src=x onerror=alert(2)>"} {
		if strings.Contains(body, raw) {
			t.Fatalf("Template emitted unescaped %q: %s", raw, body)
		}
	}
	for _, escaped := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;img src=x onerror=alert(2)&gt;"} {
		if !strings.Contains(body, escaped) {
			t.Fatalf("Template is missing escaped %q: %s", escaped, body)
		}
	}

	env := newVisibilityHTTPEnv(t)
	id, _ := env.privateSeed(t, "escaping payload")
	const username = `<script>alert(9)</script>`
	if _, err := env.db.DB.Exec("UPDATE users SET username=? WHERE id=?", username, env.member.User.ID); err != nil {
		t.Fatal(err)
	}
	rendered := privatePageCheck(t, env.request("GET", "/p/"+id, "", nil, nil), http.StatusOK)
	if strings.Contains(rendered, username) {
		t.Fatalf("Private page did not escape the owner name: %s", rendered)
	}
	if !strings.Contains(rendered, "&lt;script&gt;alert(9)&lt;/script&gt;") {
		t.Fatalf("Private page did not escape the owner name: %s", rendered)
	}
}

func TestPrivateScriptAsset(t *testing.T) {
	env := newVisibilityHTTPEnv(t)
	recorder := env.request("GET", "/private.js", "", nil, nil)
	body := visibilityCheck(t, recorder, http.StatusOK)
	if value := recorder.Header().Get("Content-Type"); value != "text/javascript; charset=utf-8" {
		t.Fatalf("Content-Type = %q", value)
	}
	if value := recorder.Header().Get("X-Robots-Tag"); value != "noindex" {
		t.Fatalf("X-Robots-Tag = %q", value)
	}
	script := string(body)
	for _, want := range []string{"'use strict'", "X-Pebril-Grant", "credentials: 'omit'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("Private script is missing %q", want)
		}
	}
	for _, banned := range []string{"localStorage", "sessionStorage", "document.cookie", "indexedDB", "innerHTML", "requestSubmit", "form.submit", "eval("} {
		if strings.Contains(script, banned) {
			t.Fatalf("Private script contains %q", banned)
		}
	}
}
