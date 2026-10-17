package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

type accountClient struct {
	User   store.User `json:"user"`
	CSRF   string     `json:"csrf"`
	cookie *http.Cookie
}

func TestRegistrationAndAdministration(t *testing.T) {
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
	service, err := auth.New(db)
	if err != nil {
		t.Fatal(err)
	}
	upload, err := uploads.New(db, manager, deployment, "/bin/false", nativeprobe.Result{})
	if err != nil {
		t.Fatal(err)
	}
	defer upload.Close()
	handler := HandlerWithUploads("test", manager, db, nativeprobe.Result{Ready: true}, service, upload)
	password, err := auth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	username := "Member" + password[:8]
	credentials := func(name, secret string) string {
		body, e := json.Marshal(map[string]string{"username": name, "password": secret})
		if e != nil {
			t.Fatal(e)
		}
		return string(body)
	}
	request := func(method, path, body string, client accountClient, origin, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://example.test"+path, strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if client.CSRF != "" {
			req.Header.Set("X-CSRF-Token", client.CSRF)
		}
		if client.cookie != nil {
			req.AddCookie(client.cookie)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, req)
		return r
	}
	check := func(r *httptest.ResponseRecorder, want int) {
		t.Helper()
		if r.Code != want {
			t.Fatalf("HTTP %d, want %d: %s", r.Code, want, r.Body.String())
		}
	}
	clientFrom := func(r *httptest.ResponseRecorder) accountClient {
		t.Helper()
		check(r, 200)
		var client accountClient
		if e := json.Unmarshal(r.Body.Bytes(), &client); e != nil {
			t.Fatal(e)
		}
		cookies := r.Result().Cookies()
		if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode || client.CSRF == "" {
			t.Fatal("Missing protected registration/login session")
		}
		client.cookie = cookies[0]
		return client
	}
	const origin = "https://example.test"
	anonymous := accountClient{}
	check(request("POST", "/api/v1/register", credentials(username, password), anonymous, origin, ""), 409)
	users, err := db.Users()
	if err != nil || len(users) != 0 {
		t.Fatal("Registration created an account before initialization", users, err)
	}
	for _, body := range []string{
		`null`, `[]`, `{}`, `{"username":null,"password":"x"}`, `{"username":"x","password":null}`,
		`{"username":"x","username":"y","password":"z"}`, `{"username":"x","password":"y","password":"z"}`,
		`{"Username":"x","password":"y"}`, `{"username":"x","password":"y","admin":true}`,
		`{"username":"x","password":123}`, `{"username":"x","password":"y"} {}`, `{"username":"x","password":"y"} false`,
		credentials("x", strings.Repeat("x", 8192)),
	} {
		check(request("POST", "/api/v1/initialize", body, anonymous, origin, ""), 400)
	}
	admin := clientFrom(request("POST", "/api/v1/initialize", credentials("Admin"+password[:8], password), anonymous, origin, ""))
	check(request("POST", "/api/v1/register", credentials(username, password), anonymous, "https://attacker.test", ""), 403)
	check(request("POST", "/api/v1/register", credentials(username, password), anonymous, "", ""), 403)
	check(request("POST", "/api/v1/register", credentials(username, password), anonymous, origin, password), 403)
	member := clientFrom(request("POST", "/api/v1/register", credentials(username, password), anonymous, origin, ""))
	if member.User.Admin || member.User.Status != "active" || member.User.Quota != store.DefaultQuota || member.User.Used != 0 {
		t.Fatal("Incorrect ordinary registration defaults", member.User)
	}
	check(request("POST", "/api/v1/register", credentials(username, password), anonymous, origin, ""), 409)
	other := clientFrom(request("POST", "/api/v1/register", credentials(strings.ToLower(username), password), anonymous, origin, ""))
	if other.User.ID == member.User.ID {
		t.Fatal("Case-sensitive identities collapsed")
	}
	for _, body := range []string{`{"username":"x","password":"y","quota":0}`, `{"username":"x","password":"y","admin":true}`, `{"username":"x","username":"y","password":"z"}`, `{"username":null,"password":"x"}`} {
		check(request("POST", "/api/v1/register", body, anonymous, origin, ""), 400)
	}
	list := request("GET", "/api/v1/admin/users", "", admin, "", "")
	check(list, 200)
	var listed struct {
		Users []struct {
			ID    int64  `json:"id"`
			Quota string `json:"quota"`
			Used  string `json:"used"`
		} `json:"users"`
	}
	if err = json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Users) != 3 || listed.Users[0].ID != admin.User.ID || listed.Users[1].ID != member.User.ID || listed.Users[2].ID != other.User.ID {
		t.Fatal("User listing omitted accounts or changed ID order", listed.Users)
	}
	if listed.Users[1].Quota != fmt.Sprint(store.DefaultQuota) || listed.Users[1].Used != "0" {
		t.Fatal("Administrator listing must preserve exact decimal byte strings", listed.Users[1])
	}
	if strings.Contains(list.Body.String(), `"password"`) || strings.Contains(list.Body.String(), `"csrf"`) {
		t.Fatal("Listing exposed credentials")
	}
	memberPath := fmt.Sprintf("/api/v1/admin/users/%d", member.User.ID)
	adminPath := fmt.Sprintf("/api/v1/admin/users/%d", admin.User.ID)
	passwordBody := fmt.Sprintf(`{"password":%q}`, password)
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/users", ""}, {"PATCH", memberPath, `{"quota":0}`}, {"POST", memberPath + "/password", passwordBody},
	} {
		check(request(route.method, route.path, route.body, anonymous, origin, ""), 401)
		check(request(route.method, route.path, route.body, member, origin, ""), 403)
		check(request(route.method, route.path, route.body, admin, origin, password), 403)
		if route.method != "GET" {
			noCSRF := admin
			noCSRF.CSRF = ""
			check(request(route.method, route.path, route.body, noCSRF, origin, ""), 403)
			check(request(route.method, route.path, route.body, admin, "https://attacker.test", ""), 403)
		}
	}
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"quota":null}`, `{"status":null}`, `{"quota":-1}`, `{"quota":1.5}`, `{"quota":9223372036854775808}`,
		`{"quota":"-1"}`, `{"quota":"-0"}`, `{"quota":"01"}`, `{"quota":"+1"}`, `{"quota":"1e3"}`, `{"quota":"1.0"}`, `{"quota":" 1"}`, `{"quota":"9223372036854775808"}`, `{"quota":-0}`, `{"quota":1e3}`, `{"status":"disabled"}`, `{"status":true}`, `{"quota":0,"quota":1}`, `{"status":"active","status":"banned"}`,
		`{"admin":true}`, `{"role":"admin"}`, `{"quota":0,"admin":false}`, `{"Quota":0}`, `{"quota":0} {}`,
	} {
		check(request("PATCH", memberPath, body, admin, origin, ""), 400)
	}
	for _, id := range []string{"0", "-1", "x", "+1", "01", "9223372036854775808"} {
		path := "/api/v1/admin/users/" + id
		check(request("PATCH", path, `{"quota":0}`, admin, origin, ""), 400)
		check(request("POST", path+"/password", passwordBody, admin, origin, ""), 400)
	}
	check(request("PATCH", "/api/v1/admin/users/999999", `{"quota":0}`, admin, origin, ""), 404)
	check(request("POST", "/api/v1/admin/users/999999/password", passwordBody, admin, origin, ""), 404)
	for _, body := range []string{`{"quota":0}`, `{"status":"banned"}`, `{"status":"active"}`, `{"quota":100,"status":"banned"}`} {
		check(request("PATCH", adminPath, body, admin, origin, ""), 403)
	}
	check(request("POST", adminPath+"/password", passwordBody, admin, origin, ""), 403)
	for _, body := range []string{`{}`, `{"password":null}`, `{"password":""}`, `{"password":"x","password":"y"}`, `{"password":"x","email":true}`, fmt.Sprintf(`{"password":%q}`, strings.Repeat("x", 73))} {
		check(request("POST", memberPath+"/password", body, admin, origin, ""), 400)
	}
	issueToken := func(client accountClient) string {
		r := request("POST", "/api/v1/upload-token", "", client, origin, "")
		check(r, 201)
		var result struct {
			Token string `json:"token"`
		}
		if e := json.Unmarshal(r.Body.Bytes(), &result); e != nil || result.Token == "" {
			t.Fatal("Missing upload token", e)
		}
		return result.Token
	}
	token := issueToken(member)
	check(request("POST", "/api/v1/uploads", "", anonymous, "", token), 400)
	check(request("PATCH", memberPath, `{"quota":0}`, admin, origin, ""), 200)
	check(request("POST", "/api/v1/uploads", "", anonymous, "", token), 429)
	check(request("POST", "/api/v1/uploads", "", member, origin, ""), 429)
	check(request("PATCH", memberPath, `{"quota":0,"status":"banned"}`, admin, origin, ""), 200)
	check(request("POST", "/api/v1/login", credentials(username, password), anonymous, origin, ""), 401)
	check(request("POST", "/api/v1/uploads", "", anonymous, "", token), 401)
	check(request("POST", "/api/v1/uploads", "", member, origin, ""), 403)
	browse := request("GET", "/api/v1/account", "", member, "", "")
	check(browse, 200)
	var state accountClient
	if err = json.Unmarshal(browse.Body.Bytes(), &state); err != nil || state.User.ID != member.User.ID || state.User.Status != "banned" || state.User.Quota != 0 {
		t.Fatal("Ban revoked existing browsing session", state, err)
	}
	patch := request("PATCH", memberPath, `{"status":"active","quota":"9223372036854775807"}`, admin, origin, "")
	check(patch, 200)
	var changed struct {
		User struct {
			Quota string `json:"quota"`
			Used  string `json:"used"`
		} `json:"user"`
	}
	if err = json.Unmarshal(patch.Body.Bytes(), &changed); err != nil || changed.User.Quota != "9223372036854775807" || changed.User.Used != "0" {
		t.Fatal("Administrator mutation rounded quota or used", changed, err)
	}
	check(request("PATCH", memberPath, `{"quota":9007199254740993}`, admin, origin, ""), 200)
	check(request("PATCH", memberPath, `{"quota":"0"}`, admin, origin, ""), 200)
	check(request("PATCH", memberPath, `{"quota":"9223372036854775807"}`, admin, origin, ""), 200)
	check(request("POST", "/api/v1/uploads", "", anonymous, "", token), 400)
	token = issueToken(member)
	secondSession := clientFrom(request("POST", "/api/v1/login", credentials(username, password), anonymous, origin, ""))
	newPassword, err := auth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	reset := request("POST", memberPath+"/password", fmt.Sprintf(`{"password":%q}`, newPassword), admin, origin, "")
	check(reset, 200)
	if err = json.Unmarshal(reset.Body.Bytes(), &changed); err != nil || changed.User.Quota != "9223372036854775807" || changed.User.Used != "0" {
		t.Fatal("Administrator reset rounded quota or used", changed, err)
	}
	for _, client := range []accountClient{member, secondSession} {
		check(request("POST", "/api/v1/upload-token", "", client, origin, ""), 401)
		check(request("POST", "/api/v1/uploads", "", client, origin, ""), 401)
	}
	check(request("POST", "/api/v1/uploads", "", anonymous, "", token), 401)
	check(request("POST", "/api/v1/login", credentials(username, password), anonymous, origin, ""), 401)
	updated := clientFrom(request("POST", "/api/v1/login", credentials(username, newPassword), anonymous, origin, ""))
	if updated.User.Quota != 9223372036854775807 || updated.User.Status != "active" {
		t.Fatal("Updated account state was not persisted", updated.User)
	}
	check(request("POST", "/api/v1/upload-token", "", other, origin, ""), 201)
	check(request("GET", "/api/v1/admin/users", "", admin, "", ""), 200)
}
