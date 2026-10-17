package auth

import (
	"database/sql"
	"errors"

	"github.com/kserksi/Pebril/internal/store"
	"github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

var ErrNotInitialized = errors.New("Initialize the administrator before registering")
var ErrUsernameTaken = errors.New("Username is already registered")

func (s *Service) Register(username, password string) (store.User, error) {
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
	var admins int
	if err = tx.QueryRow("SELECT count(*) FROM users WHERE admin=1").Scan(&admins); err != nil {
		return store.User{}, err
	}
	if admins != 1 {
		return store.User{}, ErrNotInitialized
	}
	result, err := tx.Exec("INSERT INTO users(username,password,quota) VALUES(?,?,?)", username, hash, store.DefaultQuota)
	if err != nil {
		var constraint sqlite3.Error
		if errors.As(err, &constraint) && constraint.ExtendedCode == sqlite3.ErrConstraintUnique {
			return store.User{}, ErrUsernameTaken
		}
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

func passwordHash(password string) ([]byte, error) {
	if len(password) < 1 || len(password) > 72 {
		return nil, ErrInput
	}
	return bcrypt.GenerateFromPassword([]byte(password), PasswordCost)
}

func (s *Service) ResetPassword(actor, target int64, password string) (store.User, error) {
	hash, err := passwordHash(password)
	if err != nil {
		return store.User{}, err
	}
	tx, err := s.DB.DB.Begin()
	if err != nil {
		return store.User{}, err
	}
	defer tx.Rollback()
	user, err := store.ManagedUser(tx, actor, target)
	if err != nil {
		return store.User{}, err
	}
	if err = replacePassword(tx, user.ID, hash); err != nil {
		return store.User{}, err
	}
	if err = tx.Commit(); err != nil {
		return store.User{}, err
	}
	return user, nil
}

// ResetAdministrator is only called by the offline, database-locked recovery command.
func (s *Service) ResetAdministrator(password string) (store.User, error) {
	hash, err := passwordHash(password)
	if err != nil {
		return store.User{}, err
	}
	tx, err := s.DB.DB.Begin()
	if err != nil {
		return store.User{}, err
	}
	defer tx.Rollback()
	var user store.User
	err = tx.QueryRow("SELECT id,username,admin,status,quota,used FROM users WHERE admin=1").Scan(&user.ID, &user.Username, &user.Admin, &user.Status, &user.Quota, &user.Used)
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, store.ErrAccountNotFound
	}
	if err != nil {
		return store.User{}, err
	}
	if err = replacePassword(tx, user.ID, hash); err != nil {
		return store.User{}, err
	}
	if err = tx.Commit(); err != nil {
		return store.User{}, err
	}
	return user, nil
}

func replacePassword(tx *sql.Tx, user int64, hash []byte) error {
	if _, err := tx.Exec("UPDATE users SET password=? WHERE id=?", hash, user); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE user_id=?", user); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM upload_tokens WHERE user_id=?", user)
	return err
}
