package store_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/store"
)

// This explicit real-server lane requires owned empty disposable databases.
func TestMySQLBusinessAndConnectionLoss(t *testing.T) {
	file := os.Getenv("PEBRIL_TEST_MYSQL_ENV")
	if file == "" {
		t.Skip("Real MySQL lane not configured; this is not interoperability acceptance")
	}
	values, err := godotenv.Read(file)
	if err != nil {
		t.Fatal("Cannot read private MySQL test configuration")
	}
	c, err := mysql.ParseDSN(values["PEBRIL_MYSQL_DSN"])
	if err != nil || !strings.HasPrefix(c.DBName, "pebril_test_") {
		t.Fatal("Real MySQL test requires a pebril_test_ database")
	}
	control, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	var tables int
	if err = control.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("Real MySQL fixture must start with an empty owned database", err)
	}
	d := config.Deployment{DatabaseBackend: "mysql", MySQLDSN: c.FormatDSN(), DBDir: t.TempDir()}
	db, err := store.OpenDeployment(d)
	if err != nil {
		t.Fatal(err)
	}
	namespace := db.Namespace()
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	defer func() {
		var actual string
		if e := control.QueryRow("SELECT namespace FROM deployment WHERE id=1").Scan(&actual); e != nil || actual != namespace {
			t.Error("Refusing fixture cleanup after identity mismatch")
			return
		}
		for _, table := range []string{"image_files", "images", "sessions", "upload_tokens", "users", "object_keys", "deployment", "schema_migrations"} {
			if _, e := control.Exec("DROP TABLE " + table); e != nil {
				t.Error(e)
			}
		}
	}()
	if db.DB.Stats().MaxOpenConnections != 1 {
		t.Fatal("Business transactions are not serialized")
	}
	var version string
	if err = db.DB.QueryRow("SELECT VERSION()").Scan(&version); err != nil || !strings.HasPrefix(version, "8.4.") {
		t.Fatal(version, err)
	}
	t.Log("Real MySQL version:", version)
	keyHash := strings.Repeat("f", 64)
	if generation, e := db.AcquireObjectKey(keyHash); e != nil || generation != 1 {
		t.Fatal("MySQL object generation allocation failed", generation, e)
	}
	if retired, e := db.RetireObjectKey(keyHash, 1); e != nil || !retired {
		t.Fatal("MySQL object retirement failed", retired, e)
	}
	if generation, e := db.AcquireObjectKey(keyHash); e != nil || generation != 2 {
		t.Fatal("MySQL retired key was reused", generation, e)
	}

	other := d
	other.DBDir = t.TempDir()
	if second, e := store.OpenDeployment(other); e == nil {
		second.Close()
		t.Fatal("Cross-directory instance bypassed database lock")
	}
	independent := *c
	independent.DBName += "_other"
	independentControl, e := sql.Open("mysql", independent.FormatDSN())
	if e != nil {
		t.Fatal(e)
	}
	defer independentControl.Close()
	independentDeployment := d
	independentDeployment.DBDir = t.TempDir()
	independentDeployment.MySQLDSN = independent.FormatDSN()
	second, e := store.OpenDeployment(independentDeployment)
	if e != nil {
		t.Fatal("Independent database lock interfered", e)
	}
	if second.Namespace() == namespace {
		t.Fatal("Unrelated databases share namespace identity")
	}
	second.Close()
	defer func() {
		for _, table := range []string{"image_files", "images", "sessions", "upload_tokens", "users", "object_keys", "deployment", "schema_migrations"} {
			if _, e := independentControl.Exec("DROP TABLE " + table); e != nil {
				t.Error(e)
			}
		}
	}()
	authentication, err := auth.New(db)
	if err != nil {
		t.Fatal(err)
	}
	var initialized atomic.Int64
	var adminID atomic.Int64
	var group sync.WaitGroup
	for n := 0; n < 6; n++ {
		group.Add(1)
		go func(n int) {
			defer group.Done()
			user, e := authentication.Initialize(fmt.Sprintf("Admin%d", n), "initial-password")
			if e == nil {
				initialized.Add(1)
				adminID.Store(user.ID)
			} else if !errors.Is(e, auth.ErrInitialized) {
				t.Error(e)
			}
		}(n)
	}
	group.Wait()
	if initialized.Load() != 1 {
		t.Fatal("Concurrent administrator initialization was not unique")
	}
	admin, err := db.User(adminID.Load())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := authentication.Register("Case", "password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = authentication.Register("case", "password"); err != nil {
		t.Fatal("Case-sensitive uniqueness was lost", err)
	}
	if _, err = authentication.Register("Case", "password"); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatal("Duplicate username mapping failed", err)
	}
	if _, err = db.DB.Exec("INSERT INTO users(username,password,admin,quota) VALUES('extra-admin',x'00',1,1)"); !store.IsUnique(err) {
		t.Fatal("Schema allowed a second administrator", err)
	}
	session, err := authentication.Login(owner.Username, "password")
	if err != nil {
		t.Fatal(err)
	}
	token, err := authentication.IssueToken(session)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := authentication.IssueToken(session)
	if err != nil {
		t.Fatal("Token rotation affected-row semantics changed", err)
	}
	if _, err = authentication.UploadUser(token); !errors.Is(err, auth.ErrCredentials) {
		t.Fatal("Rotated token remained valid")
	}
	if _, err = authentication.UploadUser(replacement); err != nil {
		t.Fatal(err)
	}
	exact := int64(9007199254740999)
	updated, err := db.UpdateUser(admin.ID, owner.ID, nil, &exact)
	if err != nil || updated.Quota != exact {
		t.Fatal("Exact int64 quota changed", err)
	}
	quota := int64(1)
	if _, err = db.UpdateUser(admin.ID, owner.ID, nil, &quota); err != nil {
		t.Fatal(err)
	}
	policy := config.Snapshot{Revision: 1, Policy: config.Defaults()}
	id := strings.Repeat("1", 32)
	if err = db.AcceptUpload(id, owner.ID, policy); err != nil {
		t.Fatal(err)
	}
	if err = db.Received(id); err != nil {
		t.Fatal(err)
	}
	image := store.Image{ID: id, UserID: owner.ID, State: "queued", Format: "png", Bytes: 8, Width: 1, Height: 1, Frames: 1, Hash: strings.Repeat("a", 64)}
	if err = db.SaveUpload(image); err != nil {
		t.Fatal(err)
	}
	if got, e := db.NextConversion(0, false); e != nil || got != id {
		t.Fatal("Policy snapshot query failed", e)
	}
	image, err = db.ClaimConversion(id)
	if err != nil {
		t.Fatal(err)
	}
	image.Hash = strings.Repeat("b", 64)
	image.Bytes = 16
	image.Format = "webp"
	files := []store.ImageFile{{Role: "result", Hash: image.Hash, Bytes: 16, Format: "webp", Width: 1, Height: 1, Frames: 1}}
	if err = db.FinishConversion(image, files); err != nil {
		t.Fatal("Accepted output was rejected after exceeding quota", err)
	}
	user, err := db.User(owner.ID)
	if err != nil || user.Used != 16 {
		t.Fatal("Publication accounting changed", err)
	}
	if _, err = db.DeleteImage(id, owner.ID, false); err != nil {
		t.Fatal(err)
	}
	user, err = db.User(owner.ID)
	if err != nil || user.Used != 0 {
		t.Fatal("Deletion accounting changed", err)
	}
	var accepted atomic.Int64
	for n := 0; n < 20; n++ {
		group.Add(1)
		go func(n int) {
			defer group.Done()
			e := db.AcceptUpload(fmt.Sprintf("%032x", n+100), owner.ID, policy)
			if e == nil {
				accepted.Add(1)
			} else if !errors.Is(e, store.ErrCapacity) {
				t.Error(e)
			}
		}(n)
	}
	group.Wait()
	if accepted.Load() != 10 {
		t.Fatal("Concurrent owner-slot limit changed", accepted.Load())
	}
	if _, err = db.DB.Exec("DELETE FROM images"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare("INSERT INTO images(id,user_id,state,policy,created_at,error) VALUES(?,?,'queued','{}',0,'')")
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 10000; n++ {
		if _, err = statement.Exec(fmt.Sprintf("%032x", n+1000), owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	statement.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = db.AcceptUpload(strings.Repeat("2", 32), admin.ID, policy); !errors.Is(err, store.ErrCapacity) {
		t.Fatal("Site-wide slot limit changed", err)
	}
	if _, err = db.DB.Exec("DELETE FROM images"); err != nil {
		t.Fatal(err)
	}
	if _, err = authentication.ResetPassword(admin.ID, owner.ID, "new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err = authentication.Session(session.Token); !errors.Is(err, auth.ErrCredentials) {
		t.Fatal("Password reset retained a session")
	}
	if _, err = authentication.UploadUser(replacement); !errors.Is(err, auth.ErrCredentials) {
		t.Fatal("Password reset retained upload credentials")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db, err = store.OpenDeployment(d)
	if err != nil || db.Namespace() != namespace {
		t.Fatal("Clean restart changed identity or left owner marker", err)
	}
	var connection int64
	if err = db.DB.QueryRow("SELECT CONNECTION_ID()").Scan(&connection); err != nil {
		t.Fatal(err)
	}
	if _, err = control.Exec(fmt.Sprintf("KILL CONNECTION %d", connection)); err != nil {
		t.Fatal(err)
	}
	if err = db.Ping(); err == nil {
		t.Fatal("Lost connection still ready")
	}
	select {
	case <-db.Lost():
	case <-time.After(time.Second):
		t.Fatal("Loss did not fence service")
	}
	if _, err = db.DB.Exec("UPDATE users SET used=1 WHERE id=?", owner.ID); err == nil {
		t.Fatal("Lost physical session was automatically replaced")
	}
	db.Close()
	db = nil
	if again, e := store.OpenDeployment(other); !errors.Is(e, store.ErrDatabaseOwned) {
		if again != nil {
			again.Close()
		}
		t.Fatal("Crash marker did not prevent a competing host", e)
	}
	refused := errors.New("operator refused")
	if err = store.RecoverDatabase(d, func() error { return refused }); !errors.Is(err, refused) {
		t.Fatal("Recovery ignored refusal", err)
	}
	if err = store.RecoverDatabase(d, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	db, err = store.OpenDeployment(d)
	if err != nil {
		t.Fatal("Explicit stopped-instance recovery failed", err)
	}
	if _, err = os.Stat(filepath.Join(d.DBDir, "pebril.db")); !os.IsNotExist(err) {
		t.Fatal("MySQL created a SQLite database")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
}
