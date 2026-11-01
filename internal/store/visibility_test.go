package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/config"
)

func visibilityFixture(t *testing.T) (*Store, string) {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota) VALUES('fixture',x'00',1024)")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	user, err := result.LastInsertId()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	if err = db.AcceptUpload(id, user, config.Snapshot{Revision: 1, Policy: config.Defaults()}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db, id
}

func TestSchemaSixUpgradePreservesRowsAndDefaultsPublic(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota,used) VALUES('fixture',x'00',1024,7)")
	if err != nil {
		t.Fatal(err)
	}
	user, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	if err = db.AcceptUpload(id, user, config.Snapshot{Revision: 1, Policy: config.Defaults()}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec("UPDATE images SET bytes=12,hash='preserved-hash' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	// Recreate the populated schema-5 state that a 0.0.10 database would have.
	if _, err = db.DB.Exec(`
DROP TABLE image_tags; DROP TABLE tags; DROP TABLE image_keys; DROP INDEX images_user_time;
ALTER TABLE images DROP COLUMN visibility; DELETE FROM schema_migrations WHERE version=6`); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = db.DB.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 5 {
		t.Fatal("Fixture is not at schema version 5", version, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if err = upgraded.DB.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 6 {
		t.Fatal("Schema was not upgraded to version 6", version, err)
	}
	var count, used int64
	var bytes int64
	var hash string
	if err = upgraded.DB.QueryRow("SELECT COUNT(*) FROM images").Scan(&count); err != nil || count != 1 {
		t.Fatal("Upgrade dropped existing images", count, err)
	}
	if err = upgraded.DB.QueryRow("SELECT bytes,hash FROM images WHERE id=?", id).Scan(&bytes, &hash); err != nil || bytes != 12 || hash != "preserved-hash" {
		t.Fatal("Upgrade rewrote an existing image row", bytes, hash, err)
	}
	if err = upgraded.DB.QueryRow("SELECT used FROM users WHERE id=?", user).Scan(&used); err != nil || used != 7 {
		t.Fatal("Upgrade dropped an existing user row", used, err)
	}
	image, err := upgraded.Image(id)
	if err != nil || image.Visibility != VisibilityPublic {
		t.Fatal("Existing record did not default to public", image.Visibility, err)
	}
	for _, table := range []string{"image_keys", "tags", "image_tags"} {
		if err = upgraded.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("New table is missing or not empty", table, count, err)
		}
	}
}

func TestKeyIssueVerifyExpireAndRevoke(t *testing.T) {
	db, id := visibilityFixture(t)
	defer db.Close()
	ctx := context.Background()
	now := time.UnixMilli(1700000000000)
	spare, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	if err = db.VerifyKey(ctx, id, spare, now); !errors.Is(err, ErrKeyRevoked) {
		t.Fatal("Record without a key was not reported as revoked", err)
	}
	plaintext, record, err := db.IssueKey(ctx, id, MinKeyLifetimeMS, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plaintext) != 26 {
		t.Fatal("Key is not a 26-character Crockford base32 value", plaintext)
	}
	if record.ImageID != id || record.Generation != 1 || record.CreatedAt != now.UnixMilli() || record.ExpiresAt != now.UnixMilli()+MinKeyLifetimeMS || record.LifetimeMS != MinKeyLifetimeMS {
		t.Fatal("Issued key metadata is wrong", record)
	}
	grouped := strings.ToLower(plaintext[0:4] + "-" + plaintext[4:8] + "-" + plaintext[8:])
	if err = db.VerifyKey(ctx, id, grouped, now); err != nil {
		t.Fatal("Grouped display form was rejected", err)
	}
	if err = db.VerifyKey(ctx, id, plaintext, time.UnixMilli(record.ExpiresAt-1)); err != nil {
		t.Fatal("Key was rejected before expiry", err)
	}
	replacement := "0"
	if strings.HasPrefix(plaintext, replacement) {
		replacement = "1"
	}
	if err = db.VerifyKey(ctx, id, replacement+plaintext[1:], now); !errors.Is(err, ErrKeyInvalid) {
		t.Fatal("Wrong key was accepted", err)
	}
	for _, malformed := range []string{"", "short", strings.Repeat("I", 26)} {
		if err = db.VerifyKey(ctx, id, malformed, now); !errors.Is(err, ErrKeyInvalid) {
			t.Fatal("Malformed key was not reported as invalid", malformed, err)
		}
	}
	if err = db.VerifyKey(ctx, strings.Repeat("f", 32), plaintext, now); !errors.Is(err, ErrImageNotFound) {
		t.Fatal("Unknown record was not reported", err)
	}
	if err = db.VerifyKey(ctx, id, plaintext, time.UnixMilli(record.ExpiresAt)); !errors.Is(err, ErrKeyExpired) {
		t.Fatal("Expired key was accepted", err)
	}
	if err = db.RevokeKey(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = db.VerifyKey(ctx, id, plaintext, now); !errors.Is(err, ErrKeyRevoked) {
		t.Fatal("Revoked key was accepted", err)
	}
	if err = db.RevokeKey(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("Second revocation did not report a missing key", err)
	}
}

func TestKeyRotationBumpsGenerationAndInvalidatesOldKey(t *testing.T) {
	db, id := visibilityFixture(t)
	defer db.Close()
	ctx := context.Background()
	now := time.UnixMilli(1700000000000)
	first, firstRecord, err := db.IssueKey(ctx, id, 1800000, now)
	if err != nil {
		t.Fatal(err)
	}
	second, secondRecord, err := db.RotateKey(ctx, id, 1800000, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second == first || secondRecord.Generation != firstRecord.Generation+1 || secondRecord.Generation != 2 {
		t.Fatal("Rotation did not replace the key and bump its generation", secondRecord)
	}
	if secondRecord.CreatedAt != now.Add(time.Minute).UnixMilli() || secondRecord.ExpiresAt != now.Add(time.Minute).UnixMilli()+1800000 {
		t.Fatal("Rotated key did not take the new issue time", secondRecord)
	}
	if err = db.VerifyKey(ctx, id, first, now.Add(2*time.Minute)); !errors.Is(err, ErrKeyInvalid) {
		t.Fatal("Old key survived rotation", err)
	}
	if err = db.VerifyKey(ctx, id, second, now.Add(2*time.Minute)); err != nil {
		t.Fatal("Rotated key was rejected", err)
	}
	third, thirdRecord, err := db.RotateKey(ctx, id, MaxKeyLifetimeMS, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if thirdRecord.Generation != 3 {
		t.Fatal("Repeated rotation did not keep increasing the generation", thirdRecord)
	}
	if err = db.VerifyKey(ctx, id, second, now.Add(2*time.Minute)); !errors.Is(err, ErrKeyInvalid) {
		t.Fatal("Previous key survived a second rotation", err)
	}
	if err = db.VerifyKey(ctx, id, third, now.Add(3*time.Minute)); err != nil {
		t.Fatal("Latest key was rejected", err)
	}
	var count int
	if err = db.DB.QueryRow("SELECT COUNT(*) FROM image_keys WHERE image_id=?", id).Scan(&count); err != nil || count != 1 {
		t.Fatal("Rotation left more than one active key", count, err)
	}
}

func TestKeyLifetimeBoundsAndVisibilityValidation(t *testing.T) {
	db, id := visibilityFixture(t)
	defer db.Close()
	ctx := context.Background()
	now := time.UnixMilli(1700000000000)
	for _, lifetime := range []int64{-1, 0, MinKeyLifetimeMS - 1, MaxKeyLifetimeMS + 1} {
		if _, _, err := db.IssueKey(ctx, id, lifetime, now); !errors.Is(err, ErrKeyLifetime) {
			t.Fatal("Out-of-range lifetime was accepted", lifetime, err)
		}
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM image_keys").Scan(&count); err != nil || count != 0 {
		t.Fatal("Rejected lifetime stored a key", count, err)
	}
	if _, _, err := db.IssueKey(ctx, id, MinKeyLifetimeMS, now); err != nil {
		t.Fatal("Minimum lifetime was rejected", err)
	}
	if _, _, err := db.IssueKey(ctx, id, MaxKeyLifetimeMS, now); err != nil {
		t.Fatal("Maximum lifetime was rejected", err)
	}
	if _, _, err := db.IssueKey(ctx, strings.Repeat("f", 32), MinKeyLifetimeMS, now); !errors.Is(err, ErrImageNotFound) {
		t.Fatal("Key was issued for an unknown record", err)
	}
	if err := db.SetVisibility(ctx, id, Visibility("shared")); !errors.Is(err, ErrVisibility) {
		t.Fatal("Unknown visibility value was accepted", err)
	}
	if err := db.SetVisibility(ctx, id, VisibilityPrivate); err != nil {
		t.Fatal(err)
	}
	image, err := db.Image(id)
	if err != nil || image.Visibility != VisibilityPrivate {
		t.Fatal("Visibility change was not readable", image.Visibility, err)
	}
	if err = db.SetVisibility(ctx, id, VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	image, err = db.Image(id)
	if err != nil || image.Visibility != VisibilityPublic {
		t.Fatal("Visibility did not return to public", image.Visibility, err)
	}
	if err = db.SetVisibility(ctx, strings.Repeat("f", 32), VisibilityPrivate); !errors.Is(err, ErrImageNotFound) {
		t.Fatal("Visibility was changed for an unknown record", err)
	}
}

func TestStoredKeyIsOnlyAHash(t *testing.T) {
	db, id := visibilityFixture(t)
	defer db.Close()
	ctx := context.Background()
	now := time.UnixMilli(1700000000000)
	plaintext, _, err := db.IssueKey(ctx, id, MinKeyLifetimeMS, now)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	if err = db.DB.QueryRow("SELECT hash FROM image_keys WHERE image_id=?", id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == plaintext || strings.Contains(hash, plaintext) || strings.Contains(plaintext, hash) {
		t.Fatal("Stored value contains the plaintext key")
	}
	raw, err := hex.DecodeString(hash)
	if err != nil || len(raw) != sha256.Size {
		t.Fatal("Stored value is not a SHA-256 hex digest", hash, err)
	}
	if hash != keyHash(plaintext) {
		t.Fatal("Stored value is not the hash of the issued key")
	}
	if err = db.VerifyKey(ctx, id, plaintext, now); err != nil {
		t.Fatal("Issued key does not verify", err)
	}
}

func TestConcurrentVerifyIsRaceSafe(t *testing.T) {
	db, id := visibilityFixture(t)
	defer db.Close()
	ctx := context.Background()
	now := time.UnixMilli(1700000000000)
	plaintext, _, err := db.IssueKey(ctx, id, MaxKeyLifetimeMS, now)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for n := 0; n < 8; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for attempt := 0; attempt < 8; attempt++ {
				if err := db.VerifyKey(ctx, id, plaintext, now); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal("Concurrent verification failed", err)
	}
}
