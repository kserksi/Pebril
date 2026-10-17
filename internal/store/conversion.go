package store

import (
	"database/sql"
	"errors"
	"time"
)

type ImageFile struct {
	Role, Hash, Format    string
	Bytes                 int64
	Width, Height, Frames int
}

func FileRole(files []ImageFile, role string) ImageFile {
	for _, f := range files {
		if f.Role == role {
			return f
		}
	}
	return ImageFile{}
}
func LogicalBytes(files []ImageFile) int64 {
	seen := map[string]bool{}
	var total int64
	for _, f := range files {
		if !seen[f.Hash] {
			total += f.Bytes
			seen[f.Hash] = true
		}
	}
	return total
}
func (s *Store) Files(id string) ([]ImageFile, error) {
	return readFiles(s.DB, id)
}

func readFiles(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, id string) ([]ImageFile, error) {
	rows, err := q.Query("SELECT role,hash,bytes,format,width,height,frames FROM image_files WHERE image_id=? ORDER BY role", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []ImageFile
	for rows.Next() {
		var f ImageFile
		if err = rows.Scan(&f.Role, &f.Hash, &f.Bytes, &f.Format, &f.Width, &f.Height, &f.Frames); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}
func (s *Store) NextConversion(afterUser int64, restricted bool) (string, error) {
	var id string
	err := s.DB.QueryRow(`SELECT id FROM images WHERE (state='queued' OR (state='retry_wait' AND next_run<=?)) AND (NOT ? OR (format!='avif' AND (frames>1 OR json_extract(policy,'$.policy.static_format')!='avif'))) ORDER BY CASE WHEN state='queued' THEN 0 ELSE 1 END,CASE WHEN user_id>? THEN 0 ELSE 1 END,user_id,created_at,id LIMIT 1`, time.Now().UnixMilli(), restricted, afterUser).Scan(&id)
	return id, err
}
func (s *Store) ClaimConversion(id string) (Image, error) {
	result, err := s.DB.Exec("UPDATE images SET state='processing',generation=generation+1 WHERE id=? AND (state='queued' OR (state='retry_wait' AND next_run<=?))", id, time.Now().UnixMilli())
	if err != nil {
		return Image{}, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return Image{}, sql.ErrNoRows
	}
	return s.Image(id)
}
func (s *Store) RequeueConversion(id string, generation int64) error {
	_, err := s.DB.Exec("UPDATE images SET state='queued',next_run=0 WHERE id=? AND generation=? AND state='processing'", id, generation)
	return err
}
func (s *Store) RestoreConversions(resume bool) error {
	if resume {
		_, err := s.DB.Exec("UPDATE images SET state='queued',generation=generation+1,next_run=0 WHERE state='processing'")
		return err
	}
	_, err := s.DB.Exec("UPDATE images SET state='failed',error='Task restoration is disabled',generation=generation+1,next_run=0 WHERE state IN ('queued','processing','retry_wait')")
	return err
}

func (s *Store) FinishConversion(image Image, files []ImageFile) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old int64
	if err = tx.QueryRow("SELECT COALESCE(SUM(bytes),0) FROM (SELECT MAX(bytes) AS bytes FROM image_files WHERE image_id=? GROUP BY hash)", image.ID).Scan(&old); err != nil {
		return err
	}
	result, err := tx.Exec("UPDATE images SET state='completed',hash=?,format=?,bytes=?,width=?,height=?,frames=?,error='',next_run=0,fallback=0 WHERE id=? AND state='processing' AND generation=?", image.Hash, image.Format, image.Bytes, image.Width, image.Height, image.Frames, image.ID, image.Generation)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("Conversion generation is no longer active")
	}
	if _, err = tx.Exec("DELETE FROM image_files WHERE image_id=?", image.ID); err != nil {
		return err
	}
	for _, f := range files {
		if _, err = tx.Exec("INSERT INTO image_files(image_id,role,hash,bytes,format,width,height,frames) VALUES(?,?,?,?,?,?,?,?)", image.ID, f.Role, f.Hash, f.Bytes, f.Format, f.Width, f.Height, f.Frames); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE users SET used=used+? WHERE id=?", LogicalBytes(files)-old, image.UserID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ConversionFailed(image Image, message string) error {
	attempts := image.Attempts + 1
	state := "failed"
	var next int64
	if image.Policy.Policy.AutomaticRetry && attempts <= 5 {
		state = "retry_wait"
		next = time.Now().Add(30 * time.Second).UnixMilli()
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE images SET state=?,error=?,attempts=?,next_run=?,fallback=? WHERE id=? AND generation=? AND state='processing'", state, message, attempts, next, image.Policy.Policy.OriginalFallback, image.ID, image.Generation)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return nil
	}
	if state == "failed" && !image.Policy.Policy.OriginalFallback {
		if _, err = tx.Exec("DELETE FROM image_files WHERE image_id=?", image.ID); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE users SET used=MAX(0,used-?) WHERE id=?", image.TotalBytes, image.UserID); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE images SET hash='',bytes=0 WHERE id=?", image.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) RetryConversion(id string, user int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var own, total int
	if err = tx.QueryRow("SELECT COALESCE(SUM(user_id=?),0),count(*) FROM images WHERE state IN ('receiving','received','queued','processing','retry_wait')", user).Scan(&own, &total); err != nil {
		return err
	}
	if own >= 10 || total >= 10000 {
		return ErrCapacity
	}
	result, err := tx.Exec("UPDATE images SET state='queued',attempts=0,next_run=0,error='' WHERE id=? AND user_id=? AND state='failed' AND EXISTS(SELECT 1 FROM image_files WHERE image_id=? AND role='original')", id, user, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("Image is not eligible for retry")
	}
	return tx.Commit()
}
