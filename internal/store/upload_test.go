package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kserksi/Pebril/internal/config"
)

func TestUploadAdmissionAndCharging(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota) VALUES('fixture',x'00',?)", DefaultQuota)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	policy := config.Snapshot{Revision: 1, Policy: config.Defaults()}
	for n := range 10 {
		if err = db.AcceptUpload(fmt.Sprint(n), id, policy); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.AcceptUpload("overflow", id, policy); !errors.Is(err, ErrCapacity) {
		t.Fatal("Per-user task limit failed", err)
	}
	if err = db.FailUpload("0", "rejected"); err != nil {
		t.Fatal(err)
	}
	if err = db.AcceptUpload("replacement", id, policy); err != nil {
		t.Fatal(err)
	}
	if err = db.Received("replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec("UPDATE users SET status='banned',quota=1 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	image := Image{ID: "replacement", State: "queued", Format: "png", Bytes: 42, Width: 2, Height: 2, Frames: 1, Hash: "object"}
	if err = db.SaveUpload(image); err != nil {
		t.Fatal("Accepted pre-ban task was not retained", err)
	}
	if err = db.SaveUpload(image); err == nil {
		t.Fatal("Upload was charged twice")
	}
	user, err := db.User(id)
	if err != nil || user.Used != 42 {
		t.Fatal("Logical charging failed", user, err)
	}
	if err = db.AcceptUpload("banned", id, policy); !errors.Is(err, ErrPermission) {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec("UPDATE users SET status='active' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err = db.AcceptUpload("overquota", id, policy); !errors.Is(err, ErrQuota) {
		t.Fatal("Over-quota upload accepted", err)
	}
	stored, err := db.Image("replacement")
	if err != nil || stored.Policy.Revision != 1 || stored.Policy.Policy.StaticQuality != 80 {
		t.Fatal("Acceptance policy was not persisted", err)
	}
}
