package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func randomText(t *testing.T) string {
	t.Helper()
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(bytes[:])
}
func TestInitializationAndCredentialBoundaries(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	username, password := randomText(t), randomText(t)
	var group sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for range 3 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, e := service.Initialize(username, password)
			if e == nil {
				mu.Lock()
				success++
				mu.Unlock()
			} else if !errors.Is(e, ErrInitialized) {
				t.Error(e)
			}
		}()
	}
	group.Wait()
	if success != 1 {
		t.Fatalf("Concurrent initialization created %d administrators", success)
	}
	session, err := service.Login(username, password)
	if err != nil {
		t.Fatal(err)
	}
	var hash []byte
	if err = db.DB.QueryRow("SELECT password FROM users WHERE id=?", session.User.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if string(hash) == password {
		t.Fatal("Password persisted as plaintext")
	}
	if cost, e := bcrypt.Cost(hash); e != nil || cost != PasswordCost {
		t.Fatal("Wrong password cost", e)
	}
	if _, err = service.Login(username, randomText(t)); !errors.Is(err, ErrCredentials) {
		t.Fatal("Wrong password accepted")
	}
	recovered, err := service.Session(session.Token)
	if err != nil || recovered.User.ID != session.User.ID {
		t.Fatal("Session lookup failed", err)
	}
	if !CheckCSRF(session.CSRF, session.CSRF) || CheckCSRF(randomText(t), session.CSRF) {
		t.Fatal("CSRF comparison failed")
	}
	first, err := service.IssueToken(session.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.IssueToken(session.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.UploadUser(first); err == nil {
		t.Fatal("Replaced token remains valid")
	}
	if _, err = service.UploadUser(second); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec("UPDATE users SET status='banned' WHERE id=?", session.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Session(session.Token); err != nil {
		t.Fatal("Ban unexpectedly revoked an existing browsing session")
	}
	if _, err = service.UploadUser(second); err == nil {
		t.Fatal("Banned account can upload")
	}
	if _, err = service.Login(username, password); err == nil {
		t.Fatal("Banned account can log in")
	}
	if _, err = db.DB.Exec("UPDATE users SET status='active' WHERE id=?", session.User.ID); err != nil {
		t.Fatal(err)
	}
	if err = service.RevokeToken(session.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.UploadUser(second); err == nil {
		t.Fatal("Revoked token remains valid")
	}
	if _, err = db.DB.Exec("UPDATE sessions SET expires=? WHERE hash=?", time.Now().Add(-time.Second).UnixMilli(), Digest(session.Token)); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Session(session.Token); err == nil {
		t.Fatal("Expired session remains valid")
	}
	newSession, err := service.Login(username, password)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Logout(newSession.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Session(newSession.Token); err == nil {
		t.Fatal("Logged-out session remains valid")
	}
}
