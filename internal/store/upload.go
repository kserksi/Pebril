package store

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/kserksi/Pebril/internal/config"
)

const DefaultQuota int64 = 1 << 30

var ErrCapacity = errors.New("Active upload limit reached")
var ErrQuota = errors.New("Storage quota reached")
var ErrPermission = errors.New("Account cannot upload")

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Admin    bool   `json:"admin"`
	Quota    int64  `json:"quota"`
	Used     int64  `json:"used"`
}
type Image struct {
	ID      string          `json:"id"`
	UserID  int64           `json:"-"`
	State   string          `json:"state"`
	Format  string          `json:"format,omitempty"`
	Bytes   int64           `json:"bytes"`
	Width   int             `json:"width,omitempty"`
	Height  int             `json:"height,omitempty"`
	Frames  int             `json:"frames,omitempty"`
	Created int64           `json:"created_at"`
	URL     string          `json:"url,omitempty"`
	Error   string          `json:"error,omitempty"`
	Hash    string          `json:"-"`
	Policy  config.Snapshot `json:"-"`
}

func (s *Store) AcceptUpload(id string, user int64, policy config.Snapshot) error {
	snapshot, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var quota, used int64
	if err = tx.QueryRow("SELECT status,quota,used FROM users WHERE id=?", user).Scan(&status, &quota, &used); err != nil {
		return err
	}
	if status != "active" {
		return ErrPermission
	}
	if used >= quota {
		return ErrQuota
	}
	var own, total int
	if err = tx.QueryRow("SELECT COALESCE(SUM(user_id=?),0),count(*) FROM images WHERE state IN ('receiving','received','queued')", user).Scan(&own, &total); err != nil {
		return err
	}
	if own >= 10 || total >= 10000 {
		return ErrCapacity
	}
	_, err = tx.Exec("INSERT INTO images(id,user_id,state,policy,created_at) VALUES(?,?,'receiving',?,?)", id, user, snapshot, time.Now().UTC().UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Received(id string) error {
	_, err := s.DB.Exec("UPDATE images SET state='received' WHERE id=? AND state='receiving'", id)
	return err
}
func (s *Store) FailUpload(id, message string) error {
	_, err := s.DB.Exec("UPDATE images SET state='failed',error=? WHERE id=? AND state IN ('receiving','received')", message, id)
	return err
}
func (s *Store) SaveUpload(image Image) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE images SET state=?,format=?,bytes=?,width=?,height=?,frames=?,hash=?,error='' WHERE id=? AND state='received'", image.State, image.Format, image.Bytes, image.Width, image.Height, image.Frames, image.Hash, image.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("Upload is no longer eligible for persistence")
	}
	_, err = tx.Exec("UPDATE users SET used=used+? WHERE id=(SELECT user_id FROM images WHERE id=?)", image.Bytes, image.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Image(id string) (Image, error) {
	var i Image
	var policy []byte
	err := s.DB.QueryRow("SELECT id,user_id,state,format,bytes,width,height,frames,created_at,error,hash,policy FROM images WHERE id=?", id).Scan(&i.ID, &i.UserID, &i.State, &i.Format, &i.Bytes, &i.Width, &i.Height, &i.Frames, &i.Created, &i.Error, &i.Hash, &policy)
	if err != nil {
		return i, err
	}
	err = json.Unmarshal(policy, &i.Policy)
	if i.State == "completed" && i.Format == "svg" {
		i.URL = "/image/" + i.ID
	}
	return i, err
}
func (s *Store) ReceivedUploads() ([]string, error) {
	rows, err := s.DB.Query("SELECT id FROM images WHERE state='received' ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) User(id int64) (User, error) {
	var u User
	err := s.DB.QueryRow("SELECT id,username,admin,quota,used FROM users WHERE id=?", id).Scan(&u.ID, &u.Username, &u.Admin, &u.Quota, &u.Used)
	return u, err
}
func (s *Store) ObjectReferenced(hash string) bool {
	var n int
	err := s.DB.QueryRow("SELECT count(*) FROM images WHERE hash=?", hash).Scan(&n)
	return err != nil || n > 0
}
