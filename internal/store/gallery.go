package store

import (
	"database/sql"
	"errors"
)

// galleryAccess rechecks actor status and administrator authority in the read/mutation snapshot.
func galleryAccess(tx *sql.Tx, user int64, admin bool) (bool, error) {
	var actualAdmin bool
	var status string
	if err := tx.QueryRow("SELECT admin,status FROM users WHERE id=?", user).Scan(&actualAdmin, &status); err != nil {
		return false, err
	}
	if status != "active" && status != "banned" {
		return false, sql.ErrNoRows
	}
	return admin && actualAdmin && status == "active", nil
}

func (s *Store) ListImages(user int64, all bool, limit, offset int) ([]Image, int, error) {
	images := []Image{}
	if limit < 1 || offset < 0 {
		return images, 0, errors.New("Invalid gallery pagination")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	admin, err := galleryAccess(tx, user, all)
	if err != nil {
		return nil, 0, err
	}
	if all && !admin {
		return nil, 0, ErrPermission
	}
	var total int
	if err = tx.QueryRow("SELECT count(*) FROM images WHERE user_id=? OR ?", user, admin).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query("SELECT id FROM images WHERE user_id=? OR ? ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?", user, admin, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, 0, err
	}
	for _, id := range ids {
		image, err := readImage(tx, id)
		if err != nil {
			return nil, 0, err
		}
		images = append(images, image)
	}
	return images, total, tx.Commit()
}

func galleryImage(tx *sql.Tx, id string, user int64, admin bool) (Image, error) {
	allowedAdmin, err := galleryAccess(tx, user, admin)
	if err != nil {
		return Image{}, err
	}
	image, err := readImage(tx, id)
	if err != nil {
		return Image{}, err
	}
	if image.UserID != user && !allowedAdmin {
		return Image{}, sql.ErrNoRows
	}
	return image, nil
}

func (s *Store) GalleryImage(id string, user int64, admin bool) (Image, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return Image{}, err
	}
	defer tx.Rollback()
	image, err := galleryImage(tx, id, user, admin)
	if err != nil {
		return Image{}, err
	}
	return image, tx.Commit()
}

// DeleteImage releases one record's logical charge; physical cleanup must recheck references.
func (s *Store) DeleteImage(id string, user int64, admin bool) ([]string, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	image, err := galleryImage(tx, id, user, admin)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	hashes := []string{}
	for _, file := range image.Files {
		if file.Hash != "" && !seen[file.Hash] {
			seen[file.Hash] = true
			hashes = append(hashes, file.Hash)
		}
	}
	if image.Hash != "" && !seen[image.Hash] {
		hashes = append(hashes, image.Hash)
	}
	// Removing the row also rejects every late upload or conversion generation.
	if _, err = tx.Exec("DELETE FROM images WHERE id=?", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(s.subtractUsageSQL(), image.TotalBytes, image.UserID); err != nil {
		return nil, err
	}
	return hashes, tx.Commit()
}
