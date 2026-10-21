package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/objectstore"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/testutil"
)

func sourceFixture(t *testing.T, version int) (config.Deployment, [][][]any) {
	t.Helper()
	d, err := config.LoadDeployment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(d.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.DB.Exec("INSERT INTO users(id,username,password,admin,quota) VALUES(1,'owner',?,1,?)", []byte("preserved-verifier"), store.DefaultQuota); err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	if err = png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	objects := filepath.Join(d.PublicDir, ".objects")
	if err = os.MkdirAll(objects, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(objects, hash), data, 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.Snapshot{Revision: 7, Policy: config.Defaults()}
	policy.Policy.StaticFormat = "png"
	for _, id := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		if err = database.AcceptUpload(id, 1, policy); err != nil {
			t.Fatal(err)
		}
		if err = database.Received(id); err != nil {
			t.Fatal(err)
		}
		if err = database.SaveUpload(store.Image{ID: id, UserID: 1, State: "queued", Hash: hash, Bytes: int64(len(data)), Format: "png", Width: 1, Height: 1, Frames: 1}); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(id, "a") {
			image, err := database.ClaimConversion(id)
			if err != nil {
				t.Fatal(err)
			}
			original := store.FileRole(image.Files, "original")
			result := original
			result.Role = "result"
			if err = database.FinishConversion(image, []store.ImageFile{original, result}); err != nil {
				t.Fatal(err)
			}
		}
	}
	received := strings.Repeat("c", 32)
	if err = database.AcceptUpload(received, 1, policy); err != nil {
		t.Fatal(err)
	}
	if err = database.Received(received); err != nil {
		t.Fatal(err)
	}
	incoming := filepath.Join(d.DataDir, ".incoming", received)
	os.MkdirAll(incoming, 0700)
	os.WriteFile(filepath.Join(incoming, "input"), data, 0600)
	if _, err = database.DB.Exec("INSERT INTO sessions(hash,user_id,csrf,expires) VALUES('session-hash',1,'csrf',9007199254740999)"); err != nil {
		t.Fatal(err)
	}
	if _, err = database.DB.Exec("INSERT INTO upload_tokens(user_id,hash) VALUES(1,'token-hash')"); err != nil {
		t.Fatal(err)
	}
	if version < 5 {
		if _, err = database.DB.Exec("ALTER TABLE deployment DROP COLUMN storage_backend; DROP TABLE object_keys; DELETE FROM schema_migrations WHERE version=5"); err != nil {
			t.Fatal(err)
		}
	}
	if version == 3 {
		if _, err = database.DB.Exec("ALTER TABLE images DROP COLUMN delivered; DROP TABLE deployment; DELETE FROM schema_migrations WHERE version=4"); err != nil {
			t.Fatal(err)
		}
	}
	database.Close()
	source, version, err := store.OpenMigrationSource(d.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := readTables(source.DB, version)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	return d, records
}
func s3Target(t *testing.T) (config.Deployment, config.Storage, *testutil.S3Fixture) {
	t.Helper()
	fixture, server := testutil.NewS3(t)
	transport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = transport })
	d, err := config.LoadDeployment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d, config.Storage{Backend: "s3", S3: &config.S3{Endpoint: server.URL, Bucket: "images", Region: "test-region", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Addressing: "path", Prefix: "migration/"}}, fixture
}
func TestOfflineMigrationPreservesSourceRecordsAndReruns(t *testing.T) {
	for _, version := range []int{3, 4, 5} {
		t.Run(fmt.Sprintf("schema%d", version), func(t *testing.T) {
			source, expected := sourceFixture(t, version)
			target, storage, _ := s3Target(t)
			path := filepath.Join(source.DBDir, "pebril.db")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			options := Options{Source: source, Target: target, SourceStorage: config.Storage{Backend: "local"}, TargetStorage: storage}
			if err = Run(context.Background(), options); err != nil {
				t.Fatal(err)
			}
			if err = Run(context.Background(), options); err != nil {
				t.Fatal("Completed migration did not verify/rerun safely", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("Stopped source database was modified")
			}
			database, err := store.OpenDeployment(target)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := readTables(database.DB, 4)
			if err != nil || digest(expected) != digest(actual) {
				t.Fatal("Record identities, credentials, snapshots or charges changed", err)
			}
			selected, err := objectstore.New(context.Background(), filepath.Join(target.PublicDir, ".objects"), storage, database.Namespace(), database)
			if err != nil {
				t.Fatal(err)
			}
			image, err := database.Image(strings.Repeat("a", 32))
			if err != nil || image.URL != "/image/"+strings.Repeat("a", 32) {
				t.Fatal("Stable record link changed", err)
			}
			file, err := selected.Open(context.Background(), image.Hash, image.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			file.Close()
			raw, _ := os.ReadFile(filepath.Join(target.Root, ".pebril-migration.json"))
			var state receipt
			if json.Unmarshal(raw, &state) != nil || state.Phase != "completed" || strings.Contains(string(raw), "preserved-verifier") {
				t.Fatal("Unsafe or false-success migration receipt")
			}
			input := filepath.Join(target.DataDir, ".incoming", strings.Repeat("c", 32), "input")
			if _, err = os.Stat(input); err != nil {
				t.Fatal("Fully received input was not staged", err)
			}
			if _, err = database.DB.Exec("UPDATE users SET quota=1 WHERE id=1"); err != nil {
				t.Fatal(err)
			}
			database.Close()
			if err = Run(context.Background(), options); err == nil {
				t.Fatal("Conflicting target was overwritten")
			}
			raw, _ = os.ReadFile(filepath.Join(target.Root, ".pebril-migration.json"))
			if json.Unmarshal(raw, &state) != nil || state.Phase != "failed" {
				t.Fatal("Failed rerun retained a completed receipt")
			}
		})
	}
}
func TestInterruptedAndForeignTargetMigration(t *testing.T) {
	source, _ := sourceFixture(t, 5)
	target, storage, fixture := s3Target(t)
	options := Options{Source: source, Target: target, SourceStorage: config.Storage{Backend: "local"}, TargetStorage: storage}
	fixture.Mu.Lock()
	fixture.Denied = true
	fixture.Mu.Unlock()
	if err := Run(context.Background(), options); err == nil {
		t.Fatal("Unavailable target was reported successful")
	}
	fixture.Mu.Lock()
	fixture.Denied = false
	fixture.Mu.Unlock()
	if err := Run(context.Background(), options); err != nil {
		t.Fatal("Interrupted migration could not resume", err)
	}
	sourceDB, _, err := store.OpenMigrationSource(source.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB.Close()
	other := options
	other.Target = source
	if err = Run(context.Background(), other); err == nil {
		t.Fatal("Overlapping target accepted")
	}
}
func TestReceivingSourceAndCancelledTransferStayUnchanged(t *testing.T) {
	source, _ := sourceFixture(t, 5)
	target, storage, _ := s3Target(t)
	db, err := store.Open(source.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AcceptUpload(strings.Repeat("d", 32), 1, config.Snapshot{Revision: 1, Policy: config.Defaults()}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = Run(context.Background(), Options{Source: source, Target: target, SourceStorage: config.Storage{Backend: "local"}, TargetStorage: storage}); err == nil {
		t.Fatal("Incomplete reception migrated without resolution")
	}
	database, err := store.Open(target.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	records := [][][]any{{{int64(1), "user", []byte("verifier"), int64(0), "active", int64(1), int64(0)}}, nil, nil, nil, nil}
	if err = transfer(ctx, database.DB, records); err == nil {
		t.Fatal("Cancelled record transfer committed")
	}
	var count int
	if err = database.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil || count != 0 {
		t.Fatal("Cancelled transfer left partial records", err)
	}
}
