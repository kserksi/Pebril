package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/kserksi/Pebril/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const PasswordCost = 12
const SessionLifetime = 7 * 24 * time.Hour

var ErrCredentials = errors.New("Invalid username or password")
var ErrInitialized = errors.New("Site is already initialized")
var ErrInput = errors.New("Username must contain 1 to 64 bytes without surrounding whitespace or control characters; password must contain 1 to 72 bytes")

type Service struct {
	DB    *store.Store
	dummy []byte
}
type Session struct {
	User    store.User
	CSRF    string
	Token   string
	Expires time.Time
}

func New(db *store.Store) (*Service, error) {
	random, err := Secret()
	if err != nil {
		return nil, err
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte(random), PasswordCost)
	return &Service{DB: db, dummy: dummy}, err
}
func Secret() (string, error) {
	var bytes [32]byte
	_, err := rand.Read(bytes[:])
	return base64.RawURLEncoding.EncodeToString(bytes[:]), err
}
func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func valid(username, password string) bool {
	if len(username) < 1 || len(username) > 64 || strings.TrimSpace(username) != username || len(password) < 1 || len(password) > 72 {
		return false
	}
	for _, r := range username {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (s *Service) Initialized() (bool, error) {
	var n int
	err := s.DB.DB.QueryRow("SELECT count(*) FROM users WHERE admin=1").Scan(&n)
	return n != 0, err
}
func (s *Service) Initialize(username, password string) (store.User, error) {
	if !valid(username, password) {
		return store.User{}, ErrInput
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), PasswordCost)
	if err != nil {
		return store.User{}, err
	}
	tx, err := s.DB.DB.Begin()
	if err != nil {
		return store.User{}, err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM users").Scan(&n); err != nil {
		return store.User{}, err
	}
	if n != 0 {
		return store.User{}, ErrInitialized
	}
	result, err := tx.Exec("INSERT INTO users(username,password,admin,quota) VALUES(?,?,1,?)", username, hash, store.DefaultQuota)
	if err != nil {
		return store.User{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return store.User{}, err
	}
	if err = tx.Commit(); err != nil {
		return store.User{}, err
	}
	return s.DB.User(id)
}
func (s *Service) Login(username, password string) (Session, error) {
	var id int64
	var hash []byte
	var status string
	err := s.DB.DB.QueryRow("SELECT id,password,status FROM users WHERE username=?", username).Scan(&id, &hash, &status)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(s.dummy, []byte(password))
		return Session{}, ErrCredentials
	}
	if err != nil {
		return Session{}, err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || status != "active" {
		return Session{}, ErrCredentials
	}
	return s.newSession(id, hash)
}
func (s *Service) NewSession(user store.User) (Session, error) {
	var hash []byte
	if err := s.DB.DB.QueryRow("SELECT password FROM users WHERE id=?", user.ID).Scan(&hash); err != nil {
		return Session{}, err
	}
	return s.newSession(user.ID, hash)
}
func (s *Service) newSession(id int64, expectedHash []byte) (Session, error) {
	token, err := Secret()
	if err != nil {
		return Session{}, err
	}
	csrf, err := Secret()
	if err != nil {
		return Session{}, err
	}
	expires := time.Now().Add(SessionLifetime)
	tx, err := s.DB.DB.Begin()
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	var user store.User
	var hash []byte
	err = tx.QueryRow("SELECT id,username,admin,status,quota,used,password FROM users WHERE id=?", id).Scan(&user.ID, &user.Username, &user.Admin, &user.Status, &user.Quota, &user.Used, &hash)
	if err != nil {
		return Session{}, err
	}
	// Recheck the verifier atomically so a concurrent reset cannot resurrect an old login.
	if user.Status != "active" || !bytes.Equal(hash, expectedHash) {
		return Session{}, ErrCredentials
	}
	if _, err = tx.Exec("DELETE FROM sessions WHERE expires<=?", time.Now().UnixMilli()); err != nil {
		return Session{}, err
	}
	if _, err = tx.Exec("INSERT INTO sessions(hash,user_id,csrf,expires) VALUES(?,?,?,?)", Digest(token), user.ID, csrf, expires.UnixMilli()); err != nil {
		return Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, err
	}
	return Session{User: user, CSRF: csrf, Token: token, Expires: expires}, nil
}
func (s *Service) Session(token string) (Session, error) {
	if len(token) != 43 {
		return Session{}, ErrCredentials
	}
	var id, expires int64
	var csrf, status string
	err := s.DB.DB.QueryRow("SELECT s.user_id,s.csrf,s.expires,u.status FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.hash=? AND s.expires>?", Digest(token), time.Now().UnixMilli()).Scan(&id, &csrf, &expires, &status)
	if err != nil || status == "deleted" {
		return Session{}, ErrCredentials
	}
	user, err := s.DB.User(id)
	return Session{User: user, CSRF: csrf, Token: token, Expires: time.UnixMilli(expires)}, err
}
func (s *Service) Logout(token string) error {
	_, err := s.DB.DB.Exec("DELETE FROM sessions WHERE hash=?", Digest(token))
	return err
}
func (s *Service) IssueToken(session Session) (string, error) {
	token, err := Secret()
	if err != nil {
		return "", err
	}
	result, err := s.DB.DB.Exec("INSERT INTO upload_tokens(user_id,hash) SELECT s.user_id,? FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.hash=? AND s.user_id=? AND s.expires>? AND u.status!='deleted' ON CONFLICT(user_id) DO UPDATE SET hash=excluded.hash", Digest(token), Digest(session.Token), session.User.ID, time.Now().UnixMilli())
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return "", ErrCredentials
	}
	return token, err
}
func (s *Service) RevokeToken(session Session) error {
	_, err := s.DB.DB.Exec("DELETE FROM upload_tokens WHERE user_id=? AND EXISTS(SELECT 1 FROM sessions WHERE hash=? AND user_id=? AND expires>?)", session.User.ID, Digest(session.Token), session.User.ID, time.Now().UnixMilli())
	return err
}
func (s *Service) UploadUser(token string) (store.User, error) {
	if len(token) != 43 {
		return store.User{}, ErrCredentials
	}
	var id int64
	err := s.DB.DB.QueryRow("SELECT t.user_id FROM upload_tokens t JOIN users u ON u.id=t.user_id WHERE t.hash=? AND u.status='active'", Digest(token)).Scan(&id)
	if err != nil {
		return store.User{}, ErrCredentials
	}
	return s.DB.User(id)
}
func CheckCSRF(actual, expected string) bool {
	return len(actual) == 43 && subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}
