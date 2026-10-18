package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

func TestUploadAdmissionCodes(t *testing.T) {
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
	username, err := auth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	password, err := auth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	user, err := authentication.Initialize(username, password)
	if err != nil {
		t.Fatal(err)
	}
	session, err := authentication.NewSession(user)
	if err != nil {
		t.Fatal(err)
	}
	bearer, err := authentication.IssueToken(session)
	if err != nil {
		t.Fatal(err)
	}
	upload, err := uploads.New(db, manager, deployment, "/bin/false", nativeprobe.Result{})
	if err != nil {
		t.Fatal(err)
	}
	defer upload.Close()
	handler := HandlerWithUploads("test", manager, db, nativeprobe.Result{Ready: true}, authentication, upload)
	for n := range 10 {
		if err := db.AcceptUpload(fmt.Sprintf("%032x", n), user.ID, manager.Snapshot()); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range []string{"upload_capacity", "storage_quota"} {
		if code == "storage_quota" {
			if _, err := db.DB.Exec("UPDATE users SET quota=0 WHERE id=?", user.ID); err != nil {
				t.Fatal(err)
			}
		}
		for _, token := range []string{"", bearer} {
			body := &resourceRequestBody{}
			value := resourceJSON(t, resourceRequest(handler, "POST", "/api/v1/uploads", &session, token, body), 429)
			var got string
			if err := json.Unmarshal(value["code"], &got); err != nil || got != code {
				t.Fatalf("Admission code %q, want %q: %v", got, code, err)
			}
			if body.reads != 0 {
				t.Fatal("Rejected admission read the upload body")
			}
			var count int
			if err := db.DB.QueryRow("SELECT count(*) FROM images").Scan(&count); err != nil || count != 10 {
				t.Fatal("Rejected admission created an image record", count, err)
			}
		}
	}
}
