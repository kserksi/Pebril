package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kserksi/Pebril/internal/store"
)

func accountService(t *testing.T) (*Service, store.User, string) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	service, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	password := randomText(t)
	admin, err := service.Initialize(randomText(t), password)
	if err != nil {
		t.Fatal(err)
	}
	return service, admin, password
}

func TestRegistrationRequiresAdministratorAndPreservesCase(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	password := randomText(t)
	if _, err = service.Register(randomText(t), password); !errors.Is(err, ErrNotInitialized) {
		t.Fatal("Registration before initialization", err)
	}
	var group sync.WaitGroup
	for range 3 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, e := service.Register(randomText(t), password)
			if e != nil && !errors.Is(e, ErrNotInitialized) {
				t.Error(e)
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		if _, e := service.Initialize(randomText(t), password); e != nil {
			t.Error(e)
		}
	}()
	group.Wait()
	var admins int
	if err = db.DB.QueryRow("SELECT count(*) FROM users WHERE admin=1").Scan(&admins); err != nil || admins != 1 {
		t.Fatal("Registration changed the unique administrator", admins, err)
	}
	username := "a" + randomText(t)
	var success int
	var mu sync.Mutex
	for range 3 {
		group.Add(1)
		go func() {
			defer group.Done()
			user, e := service.Register(username, password)
			if e == nil {
				if user.Admin || user.Status != "active" || user.Quota != store.DefaultQuota || user.Used != 0 {
					t.Error("Unexpected ordinary-user defaults", user)
				}
				mu.Lock()
				success++
				mu.Unlock()
			} else if !errors.Is(e, ErrUsernameTaken) {
				t.Error(e)
			}
		}()
	}
	group.Wait()
	if success != 1 {
		t.Fatal("Concurrent duplicate registration successes", success)
	}
	upper := strings.ToUpper(username)
	otherPassword := randomText(t)
	other, err := service.Register(upper, otherPassword)
	if err != nil || other.Username != upper {
		t.Fatal("Case-sensitive registration", err)
	}
	if _, err = service.Login(username, otherPassword); !errors.Is(err, ErrCredentials) {
		t.Fatal("Login ignored username case", err)
	}
	if _, err = service.Login(upper, otherPassword); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ username, password string }{
		{"", password}, {" " + username, password}, {username + "\n", password},
		{strings.Repeat("u", 65), password}, {username, ""}, {username, strings.Repeat("p", 73)},
	} {
		if _, err = service.Register(c.username, c.password); !errors.Is(err, ErrInput) {
			t.Fatal("Invalid credentials accepted", err)
		}
	}
	if _, err = service.Register(strings.Repeat("u", 64), strings.Repeat("p", 72)); err != nil {
		t.Fatal("Boundary-length credentials rejected", err)
	}
}

func TestResetRevokesAllCredentialsAndRejectsStaleLoginAndTokenIssuance(t *testing.T) {
	service, admin, adminPassword := accountService(t)
	username, password := randomText(t), randomText(t)
	user, err := service.Register(username, password)
	if err != nil {
		t.Fatal(err)
	}
	adminSession, err := service.Login(admin.Username, adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Login(username, password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Login(username, password)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.IssueToken(first)
	if err != nil {
		t.Fatal(err)
	}
	var verifiedHash []byte
	if err = service.DB.DB.QueryRow("SELECT password FROM users WHERE id=?", user.ID).Scan(&verifiedHash); err != nil {
		t.Fatal(err)
	}
	newPassword := randomText(t)
	for _, denied := range [][2]int64{{user.ID, user.ID}, {user.ID, admin.ID}, {admin.ID, admin.ID}} {
		if _, err = service.ResetPassword(denied[0], denied[1], newPassword); !errors.Is(err, store.ErrAccountPermission) {
			t.Fatal("Protected reset accepted", err)
		}
	}
	if _, err = service.ResetPassword(admin.ID, user.ID, ""); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if _, err = service.ResetPassword(admin.ID, user.ID, newPassword); err != nil {
		t.Fatal(err)
	}
	for _, session := range []Session{first, second} {
		if _, err = service.Session(session.Token); !errors.Is(err, ErrCredentials) {
			t.Fatal("Reset preserved a session", err)
		}
		if _, err = service.IssueToken(session); !errors.Is(err, ErrCredentials) {
			t.Fatal("In-flight old session issued a new token", err)
		}
	}
	if _, err = service.UploadUser(token); !errors.Is(err, ErrCredentials) {
		t.Fatal("Reset preserved upload token", err)
	}
	status := "banned"
	if _, err = service.DB.UpdateUser(admin.ID, user.ID, &status, nil); err != nil {
		t.Fatal(err)
	}
	// A reset followed by a ban must not reveal status to a stale password verifier.
	if _, err = service.newSession(user.ID, verifiedHash); !errors.Is(err, ErrCredentials) {
		t.Fatal("Stale password verification resurrected a session or revealed a ban", err)
	}
	if _, err = service.Login(username, newPassword); !errors.Is(err, ErrBanned) {
		t.Fatal("Current password did not report banned status", err)
	}
	status = "active"
	if _, err = service.DB.UpdateUser(admin.ID, user.ID, &status, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Login(username, password); !errors.Is(err, ErrCredentials) {
		t.Fatal("Old password accepted", err)
	}
	current, err := service.Login(username, newPassword)
	if err != nil {
		t.Fatal(err)
	}
	currentToken, err := service.IssueToken(current)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RevokeToken(first); err != nil {
		t.Fatal(err)
	}
	if _, err = service.UploadUser(currentToken); err != nil {
		t.Fatal("Stale revocation changed a replacement token", err)
	}
	if _, err = service.Session(adminSession.Token); err != nil {
		t.Fatal("Ordinary reset revoked administrator", err)
	}
	if _, err = service.ResetAdministrator(randomText(t)); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Session(adminSession.Token); !errors.Is(err, ErrCredentials) {
		t.Fatal("Local reset preserved administrator session", err)
	}
	if _, err = service.Session(current.Token); err != nil {
		t.Fatal("Local administrator reset changed ordinary credentials", err)
	}
}

func TestResetRollbackPreservesPasswordSessionsAndToken(t *testing.T) {
	service, admin, _ := accountService(t)
	username, password := randomText(t), randomText(t)
	user, err := service.Register(username, password)
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Login(username, password)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.IssueToken(session)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.DB.DB.Exec("CREATE TRIGGER fail_token_revoke BEFORE DELETE ON upload_tokens BEGIN SELECT RAISE(ABORT,'controlled reset failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ResetPassword(admin.ID, user.ID, randomText(t)); err == nil {
		t.Fatal("Reset unexpectedly succeeded through a storage failure")
	}
	if _, err = service.Login(username, password); err != nil {
		t.Fatal("Failed reset changed password", err)
	}
	if _, err = service.Session(session.Token); err != nil {
		t.Fatal("Failed reset revoked session", err)
	}
	if _, err = service.UploadUser(token); err != nil {
		t.Fatal("Failed reset revoked token", err)
	}
}
