package store

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kserksi/Pebril/internal/config"
)

func TestGalleryOrderingPaginationAndLiveAuthority(t *testing.T) {
	db, first := conversionFixture(t, config.Defaults())
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota) VALUES('other',x'00',1024)")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := result.LastInsertId()
	result, err = db.DB.Exec("INSERT INTO users(username,password,quota,admin) VALUES('admin',x'00',1024,1)")
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := result.LastInsertId()
	for _, item := range []struct {
		id   string
		user int64
	}{{"b", first.UserID}, {"c", first.UserID}, {"d", other}} {
		if err = db.AcceptUpload(item.id, item.user, first.Policy); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.DB.Exec("UPDATE images SET created_at=10"); err != nil {
		t.Fatal(err)
	}
	images, total, err := db.ListImages(first.UserID, false, 1, 1)
	if err != nil || total != 3 || len(images) != 1 || images[0].ID != "b" {
		t.Fatal("Owner page did not use stable upload-time ordering", images, total, err)
	}
	images, total, err = db.ListImages(admin, true, 10, 0)
	if err != nil || total != 4 || len(images) != 4 || images[0].ID != "d" {
		t.Fatal("Administrator cannot list all records", images, total, err)
	}
	images, total, err = db.ListImages(first.UserID, false, 10, 100)
	if err != nil || total != 3 || images == nil || len(images) != 0 {
		t.Fatal("Empty page lost total or JSON list semantics", images, total, err)
	}
	if _, _, err = db.ListImages(other, true, 10, 0); !errors.Is(err, ErrPermission) {
		t.Fatal("Ordinary account widened gallery scope", err)
	}
	if _, err = db.GalleryImage(first.ID, other, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("Forged administrator flag exposed a record", err)
	}
	if _, err = db.DeleteImage(first.ID, other, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("Forged administrator flag deleted a record", err)
	}
	if image, err := db.GalleryImage(first.ID, admin, true); err != nil || len(image.Files) != 1 || image.TotalBytes != 42 {
		t.Fatal("Administrator detail lost file roles", image, err)
	}
	if _, err = db.DB.Exec("UPDATE users SET status='banned' WHERE id=?", first.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.GalleryImage(first.ID, first.UserID, false); err != nil {
		t.Fatal("Ban blocked existing owner management", err)
	}
	if _, err = db.DeleteImage("d", admin, true); err != nil {
		t.Fatal("Administrator could not delete another owner's record", err)
	}
	if _, err = db.DB.Exec("UPDATE users SET status='deleted' WHERE id=?", admin); err != nil {
		t.Fatal(err)
	}
	if _, err = db.GalleryImage(first.ID, admin, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("Deleted administrator retained detail access", err)
	}
	if _, err = db.DeleteImage(first.ID, admin, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("Deleted administrator retained delete access", err)
	}
}

func TestDeleteImageReleasesPerRecordDistinctFiles(t *testing.T) {
	db, image := conversionFixture(t, config.Defaults())
	original := FileRole(image.Files, "original")
	result := original
	result.Role = "result"
	cover := ImageFile{Role: "cover", Hash: strings.Repeat("c", 64), Bytes: 16, Format: "webp", Width: 1, Height: 1, Frames: 1}
	if err := db.FinishConversion(image, []ImageFile{original, result, cover}); err != nil {
		t.Fatal(err)
	}
	second := image
	second.ID = "second"
	second.State = "queued"
	if err := db.AcceptUpload(second.ID, image.UserID, image.Policy); err != nil {
		t.Fatal(err)
	}
	if err := db.Received(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveUpload(second); err != nil {
		t.Fatal(err)
	}
	hashes, err := db.DeleteImage(image.ID, image.UserID, false)
	if err != nil || !reflect.DeepEqual(hashes, []string{cover.Hash, original.Hash}) {
		t.Fatal("Deletion did not return each distinct role object", hashes, err)
	}
	user, err := db.User(image.UserID)
	if err != nil || user.Used != 42 {
		t.Fatal("Deletion released another record's charge or charged duplicate roles", user, err)
	}
	if !db.ObjectReferenced(original.Hash) || db.ObjectReferenced(cover.Hash) {
		t.Fatal("Shared/unshared reference accounting is incorrect")
	}
	if _, err = db.Image(image.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("Deleted record remains addressable", err)
	}
	if files, err := db.Files(image.ID); err != nil || len(files) != 0 {
		t.Fatal("Deleted record retained role rows", files, err)
	}
	if _, err = db.DeleteImage(image.ID, image.UserID, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("Repeated deletion succeeded", err)
	}
	if _, err = db.DeleteImage(second.ID, image.UserID, false); err != nil {
		t.Fatal(err)
	}
	user, _ = db.User(image.UserID)
	if user.Used != 0 || db.ObjectReferenced(original.Hash) {
		t.Fatal("Last reference deletion retained its charge/object reference", user)
	}
}

func TestDeleteImageInvalidatesEveryInFlightState(t *testing.T) {
	for _, state := range []string{"receiving", "received", "queued", "processing", "retry_wait", "failed"} {
		t.Run(state, func(t *testing.T) {
			db, stale := conversionFixture(t, config.Defaults())
			if _, err := db.DB.Exec("UPDATE images SET state=? WHERE id=?", state, stale.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DeleteImage(stale.ID, stale.UserID, false); err != nil {
				t.Fatal(err)
			}
			if err := db.FinishConversion(stale, stale.Files); err == nil {
				t.Fatal("Late conversion republished deleted record")
			}
			if err := db.SaveUpload(stale); err == nil {
				t.Fatal("Late reception republished deleted record")
			}
			if err := db.ConversionFailed(stale, "Late failure"); err != nil {
				t.Fatal(err)
			}
			user, err := db.User(stale.UserID)
			if err != nil || user.Used != 0 {
				t.Fatal("Late task mutated quota", user, err)
			}
			if err = db.AcceptUpload("replacement", stale.UserID, stale.Policy); err != nil {
				t.Fatal("Deleted record retained an active slot", err)
			}
		})
	}
}

func TestDeleteImageRollbackPreservesRecordAndUsage(t *testing.T) {
	db, image := conversionFixture(t, config.Defaults())
	if _, err := db.DB.Exec("CREATE TRIGGER reject_usage BEFORE UPDATE OF used ON users BEGIN SELECT RAISE(ABORT,'fixture quota failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteImage(image.ID, image.UserID, false); err == nil {
		t.Fatal("Deletion ignored quota update failure")
	}
	stored, err := db.Image(image.ID)
	if err != nil || len(stored.Files) != 1 || stored.Generation != image.Generation {
		t.Fatal("Failed deletion changed the record or references", stored, err)
	}
	user, err := db.User(image.UserID)
	if err != nil || user.Used != 42 {
		t.Fatal("Failed deletion changed quota", user, err)
	}
}
