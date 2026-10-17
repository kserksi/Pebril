package store

import (
	"database/sql"
	"errors"
)

var ErrAccountInput = errors.New("Invalid account status or quota")
var ErrAccountPermission = errors.New("This account operation is not permitted")
var ErrAccountNotFound = errors.New("Account not found")

func (s *Store) Users() ([]User, error) {
	rows, err := s.DB.Query("SELECT id,username,admin,status,quota,used FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var user User
		if err = rows.Scan(&user.ID, &user.Username, &user.Admin, &user.Status, &user.Quota, &user.Used); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// ManagedUser checks both identities in the same transaction as the account mutation.
func ManagedUser(tx *sql.Tx, actor, target int64) (User, error) {
	var admin bool
	var status string
	err := tx.QueryRow("SELECT admin,status FROM users WHERE id=?", actor).Scan(&admin, &status)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (!admin || status != "active") {
		return User{}, ErrAccountPermission
	}
	if err != nil {
		return User{}, err
	}
	var user User
	err = tx.QueryRow("SELECT id,username,admin,status,quota,used FROM users WHERE id=?", target).Scan(&user.ID, &user.Username, &user.Admin, &user.Status, &user.Quota, &user.Used)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrAccountNotFound
	}
	if err != nil {
		return User{}, err
	}
	if user.Admin || user.Status == "deleted" {
		return User{}, ErrAccountPermission
	}
	return user, nil
}

func (s *Store) UpdateUser(actor, target int64, status *string, quota *int64) (User, error) {
	if status == nil && quota == nil || status != nil && *status != "active" && *status != "banned" || quota != nil && *quota < 0 {
		return User{}, ErrAccountInput
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	user, err := ManagedUser(tx, actor, target)
	if err != nil {
		return User{}, err
	}
	if status != nil {
		user.Status = *status
	}
	if quota != nil {
		user.Quota = *quota
	}
	if _, err = tx.Exec("UPDATE users SET status=?,quota=? WHERE id=?", user.Status, user.Quota, target); err != nil {
		return User{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}
