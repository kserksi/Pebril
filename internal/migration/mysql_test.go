package migration

import (
	"bytes"
	"context"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/objectstore"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/testutil"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealMySQLStoppedMigration(t *testing.T) {
	for _, backend := range []string{"local", "s3"} {
		t.Run(backend, func(t *testing.T) {
			source, expected := sourceFixture(t, 3)
			target, e := config.LoadDeployment(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			storage := config.Storage{Backend: "local"}
			if backend == "s3" {
				target, storage, _ = s3Target(t)
			}
			target = testutil.MySQLDeployment(t, target, "migration"+backend)
			path := filepath.Join(source.DBDir, "pebril.db")
			before, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			options := Options{Source: source, Target: target, SourceStorage: config.Storage{Backend: "local"}, TargetStorage: storage}
			if e = Run(context.Background(), options); e != nil {
				t.Fatal(e)
			}
			if e = Run(context.Background(), options); e != nil {
				t.Fatal("Migration rerun failed", e)
			}
			after, e := os.ReadFile(path)
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("Source changed", e)
			}
			db, e := store.OpenDeployment(target)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			actual, e := readTables(db.DB, 5)
			if e != nil || digest(expected) != digest(actual) {
				t.Fatal("MySQL record inventory changed", e)
			}
			objects, e := objectstore.New(context.Background(), filepath.Join(target.PublicDir, ".objects"), storage, db.Namespace(), db)
			if e != nil {
				t.Fatal(e)
			}
			image, e := db.Image(strings.Repeat("a", 32))
			if e != nil {
				t.Fatal(e)
			}
			file, e := objects.Open(context.Background(), image.Hash, image.Bytes)
			if e != nil {
				t.Fatal(e)
			}
			data, e := io.ReadAll(file)
			file.Close()
			if e != nil || int64(len(data)) != image.Bytes {
				t.Fatal("Migrated object unreadable", e)
			}
			original, e := os.ReadFile(filepath.Join(source.PublicDir, ".objects", image.Hash))
			if e != nil || !bytes.Equal(data, original) {
				t.Fatal("Migrated bytes changed", e)
			}
		})
	}
}
