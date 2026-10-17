package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/kserksi/Pebril/internal/config"
)

func TestAccountBoundsAndAcceptedWorkSurviveBanQuotaAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if _, err = db.DB.Exec("INSERT INTO users(id,username,password,admin,quota) VALUES(1,'admin',x'00',1,?),(2,'ordinary',x'00',0,?)", DefaultQuota, DefaultQuota); err != nil {
		t.Fatal(err)
	}
	zero, negative := int64(0), int64(-1)
	banned, active := "banned", "active"
	for _, c := range []struct {
		actor, target int64
		status        *string
		quota         *int64
		want          error
	}{
		{1, 2, nil, nil, ErrAccountInput},
		{1, 2, nil, &negative, ErrAccountInput},
		{1, 1, nil, &zero, ErrAccountPermission},
		{1, 1, &banned, nil, ErrAccountPermission},
		{2, 2, nil, &zero, ErrAccountPermission},
		{1, 99, &banned, nil, ErrAccountNotFound},
	} {
		if _, err = db.UpdateUser(c.actor, c.target, c.status, c.quota); !errors.Is(err, c.want) {
			t.Fatal("Account mutation boundary", err, c.want)
		}
	}
	invalid := "deleted"
	if _, err = db.UpdateUser(1, 2, &invalid, nil); !errors.Is(err, ErrAccountInput) {
		t.Fatal("Deletion status entered ordinary administration", err)
	}
	if _, err = db.UpdateUser(1, 2, nil, &zero); err != nil {
		t.Fatal(err)
	}
	policy := config.Snapshot{Revision: 1, Policy: config.Defaults()}
	if err = db.AcceptUpload("zero-empty", 2, policy); !errors.Is(err, ErrQuota) {
		t.Fatal("Zero quota accepted an upload at zero usage", err)
	}
	quota := DefaultQuota
	if _, err = db.UpdateUser(1, 2, nil, &quota); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"receiving", "queued", "processing"} {
		if err = db.AcceptUpload(id, 2, policy); err != nil {
			t.Fatal(err)
		}
		if id != "receiving" {
			if err = db.Received(id); err != nil {
				t.Fatal(err)
			}
			if err = db.SaveUpload(Image{ID: id, State: "queued", Format: "png", Bytes: 10, Hash: id, Width: 2, Height: 2, Frames: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	processing, err := db.ClaimConversion("processing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.UpdateUser(1, 2, &banned, &zero); err != nil {
		t.Fatal(err)
	}
	if err = db.AcceptUpload("after-ban", 2, policy); !errors.Is(err, ErrPermission) {
		t.Fatal("Banned user accepted new work", err)
	}
	if err = db.Received("receiving"); err != nil {
		t.Fatal(err)
	}
	if err = db.SaveUpload(Image{ID: "receiving", State: "completed", Format: "svg", Bytes: 5, Hash: "svg", Width: 2, Height: 2, Frames: 1}); err != nil {
		t.Fatal("Ban/quota cancelled accepted reception", err)
	}
	processing.Hash, processing.Format, processing.Bytes = "converted", "webp", 12
	if err = db.FinishConversion(processing, []ImageFile{{Role: "result", Hash: "converted", Format: "webp", Bytes: 12, Width: 2, Height: 2, Frames: 1}}); err != nil {
		t.Fatal("Ban/quota cancelled running conversion", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if next, e := db.NextConversion(0, false); e != nil || next != "queued" {
		t.Fatal("Pre-ban queue lost eligibility after restart", next, e)
	}
	if _, err = db.UpdateUser(1, 2, &active, nil); err != nil {
		t.Fatal(err)
	}
	if err = db.AcceptUpload("zero-used", 2, policy); !errors.Is(err, ErrQuota) {
		t.Fatal("Unban bypassed zero quota", err)
	}
	users, err := db.Users()
	if err != nil || len(users) != 2 || users[0].ID != 1 || users[1].Used != 27 || users[1].Status != "active" || users[1].Quota != 0 {
		t.Fatal("Account listing or accepted-work accounting changed", users, err)
	}
	for _, id := range []string{"receiving", "processing"} {
		image, e := db.Image(id)
		if e != nil || image.State != "completed" || image.URL == "" {
			t.Fatal("Existing image availability changed", id, e)
		}
	}
}
