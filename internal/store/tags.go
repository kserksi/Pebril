package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Tag limits from the frozen 0.0.11 contract. They are enforced on write; reads
// never depend on them.
const (
	TagMinCodePoints = 1
	TagMaxCodePoints = 32
	ImageTagLimit    = 20
	TagLibraryLimit  = 200
)

var (
	// ErrTagName marks a tag name that is empty, too long, contains a control
	// character, or uses the reserved leading '#'.
	ErrTagName = errors.New("Invalid tag name")
	// ErrTagNotFound marks a tag that does not exist in the acting library.
	ErrTagNotFound = errors.New("Tag not found")
	// ErrTagLimit marks a library that already holds TagLibraryLimit tags.
	ErrTagLimit = errors.New("Tag library limit reached")
	// ErrImageTagLimit marks an image that would exceed ImageTagLimit tags.
	ErrImageTagLimit = errors.New("Image tag limit reached")
)

// tagWhitespace is the ASCII whitespace trimmed from both ends of a tag name.
// Interior whitespace is handled separately: spaces and tabs collapse, other
// whitespace is a control character and is rejected.
const tagWhitespace = " \t\n\r\v\f"

// Tag is one entry of a user's tag library. Count is the number of the user's
// images that currently carry the tag; it is computed on read and never stored.
type Tag struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"-"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	Count     int    `json:"count"`
}

// ImageTag is one tag attached to one image.
type ImageTag struct {
	ImageID string `json:"-"`
	TagID   int64  `json:"-"`
	Name    string `json:"name"`
}

// NormalizeTagName applies the contract normalization: validate UTF-8, trim
// leading and trailing ASCII whitespace, convert to Unicode NFC, reject control
// characters and the reserved leading '#', collapse interior runs of spaces and
// tabs to one space, and enforce the code point bounds. Tag identity stays
// case-sensitive and case-preserving.
func NormalizeTagName(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrTagName
	}
	name := strings.Trim(value, tagWhitespace)
	name = norm.NFC.String(name)
	if name == "" || strings.HasPrefix(name, "#") {
		return "", ErrTagName
	}
	clean := make([]rune, 0, len(name))
	space := false
	for _, r := range name {
		if r == ' ' || r == '\t' {
			space = true
			continue
		}
		if unicode.IsControl(r) {
			return "", ErrTagName
		}
		if space && len(clean) > 0 {
			clean = append(clean, ' ')
		}
		space = false
		clean = append(clean, r)
	}
	if len(clean) < TagMinCodePoints || len(clean) > TagMaxCodePoints {
		return "", ErrTagName
	}
	return string(clean), nil
}

// normalizeTagNames normalizes and de-duplicates a set of names while keeping
// first-seen order.
func normalizeTagNames(names []string) ([]string, error) {
	normalized := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		clean, err := NormalizeTagName(name)
		if err != nil {
			return nil, err
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		normalized = append(normalized, clean)
	}
	return normalized, nil
}

// EnsureTag resolves a tag by name in the user's library, creating it on demand.
func (s *Store) EnsureTag(ctx context.Context, userID int64, name string) (Tag, error) {
	clean, err := NormalizeTagName(name)
	if err != nil {
		return Tag{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Tag{}, err
	}
	defer tx.Rollback()
	tag, err := ensureTagTx(ctx, tx, userID, clean)
	if err != nil {
		return Tag{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tag{}, err
	}
	return tag, nil
}

// ensureTagTx is EnsureTag inside an existing transaction. A concurrent
// creation that wins the unique (user_id,name) constraint is resolved by
// reading the existing row instead of failing.
func ensureTagTx(ctx context.Context, tx *sql.Tx, userID int64, name string) (Tag, error) {
	tag, err := findTag(ctx, tx, userID, name)
	if err == nil {
		return tag, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Tag{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM tags WHERE user_id=?", userID).Scan(&count); err != nil {
		return Tag{}, err
	}
	if count >= TagLibraryLimit {
		return Tag{}, ErrTagLimit
	}
	created := time.Now().UTC().UnixMilli()
	result, err := tx.ExecContext(ctx, "INSERT INTO tags(user_id,name,created_at) VALUES(?,?,?)", userID, name, created)
	if err != nil {
		if IsUnique(err) {
			return findTag(ctx, tx, userID, name)
		}
		return Tag{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Tag{}, err
	}
	return Tag{ID: id, UserID: userID, Name: name, CreatedAt: created, Count: 0}, nil
}

// findTag reads one library tag and its current image count.
func findTag(ctx context.Context, tx *sql.Tx, userID int64, name string) (Tag, error) {
	var tag Tag
	err := tx.QueryRowContext(ctx, "SELECT id,user_id,name,created_at FROM tags WHERE user_id=? AND name=?", userID, name).
		Scan(&tag.ID, &tag.UserID, &tag.Name, &tag.CreatedAt)
	if err != nil {
		return Tag{}, err
	}
	tag.Count, err = tagCount(ctx, tx, tag.ID, userID)
	if err != nil {
		return Tag{}, err
	}
	return tag, nil
}

// tagCount counts only images owned by the library owner.
func tagCount(ctx context.Context, tx *sql.Tx, tagID, userID int64) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, "SELECT count(i.id) FROM image_tags it JOIN images i ON i.id=it.image_id WHERE it.tag_id=? AND i.user_id=?", tagID, userID).
		Scan(&count)
	return count, err
}

// ListTags lists the user's tag library with per-tag image counts. Zero-image
// tags stay in the library (D16).
func (s *Store) ListTags(ctx context.Context, userID int64) ([]Tag, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.id,t.user_id,t.name,t.created_at,count(i.id)
		FROM tags t
		LEFT JOIN image_tags it ON it.tag_id=t.id
		LEFT JOIN images i ON i.id=it.image_id AND i.user_id=t.user_id
		WHERE t.user_id=?
		GROUP BY t.id,t.user_id,t.name,t.created_at
		ORDER BY t.name,t.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := make([]Tag, 0)
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.UserID, &tag.Name, &tag.CreatedAt, &tag.Count); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tags, nil
}

// DeleteTag removes a library tag and every assignment of it in one
// transaction. The images are not touched.
func (s *Store) DeleteTag(ctx context.Context, userID int64, name string) error {
	clean, err := NormalizeTagName(name)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM tags WHERE user_id=? AND name=?", userID, clean).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTagNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM image_tags WHERE tag_id=?", id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM tags WHERE id=? AND user_id=?", id, userID)
	if err != nil {
		return err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if removed == 0 {
		return ErrTagNotFound
	}
	return tx.Commit()
}

// ImageTags lists the tags attached to one image. The image owner is the tag
// library owner; an admin editing another user's image reads the target
// library, never their own (D15).
func (s *Store) ImageTags(ctx context.Context, imageID string, user int64, admin bool) ([]ImageTag, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	owner, err := imageTagOwner(ctx, tx, imageID, user, admin)
	if err != nil {
		return nil, err
	}
	tags, err := imageTagsTx(ctx, tx, imageID, owner)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tags, nil
}

// AddImageTags attaches names to an image, creating missing library tags. It is
// idempotent: an already attached tag is left alone. The admin scope follows
// ImageTags.
func (s *Store) AddImageTags(ctx context.Context, imageID string, user int64, admin bool, names []string) ([]ImageTag, error) {
	normalized, err := normalizeTagNames(names)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	owner, err := imageTagOwner(ctx, tx, imageID, user, admin)
	if err != nil {
		return nil, err
	}
	current, err := imageTagIDsTx(ctx, tx, imageID, owner)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]bool, len(current)+len(normalized))
	for _, id := range current {
		seen[id] = true
	}
	ids := make([]int64, 0, len(normalized))
	for _, name := range normalized {
		tag, err := ensureTagTx(ctx, tx, owner, name)
		if err != nil {
			return nil, err
		}
		if !seen[tag.ID] {
			seen[tag.ID] = true
			ids = append(ids, tag.ID)
		}
	}
	if len(seen) > ImageTagLimit {
		return nil, ErrImageTagLimit
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT INTO image_tags(image_id,tag_id) VALUES(?,?)", imageID, id); err != nil {
			if IsUnique(err) {
				continue
			}
			return nil, err
		}
	}
	tags, err := imageTagsTx(ctx, tx, imageID, owner)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tags, nil
}

// RemoveImageTag detaches one tag from one image. The tag itself is retained,
// possibly with a zero count (D16).
func (s *Store) RemoveImageTag(ctx context.Context, imageID string, user int64, admin bool, name string) ([]ImageTag, error) {
	clean, err := NormalizeTagName(name)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	owner, err := imageTagOwner(ctx, tx, imageID, user, admin)
	if err != nil {
		return nil, err
	}
	var tagID int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM tags WHERE user_id=? AND name=?", owner, clean).Scan(&tagID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTagNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM image_tags WHERE image_id=? AND tag_id=?", imageID, tagID); err != nil {
		return nil, err
	}
	tags, err := imageTagsTx(ctx, tx, imageID, owner)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tags, nil
}

// ReplaceImageTags swaps the whole tag set of one image. Names are normalized
// and de-duplicated; missing library tags are created.
func (s *Store) ReplaceImageTags(ctx context.Context, imageID string, user int64, admin bool, names []string) ([]ImageTag, error) {
	normalized, err := normalizeTagNames(names)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	owner, err := imageTagOwner(ctx, tx, imageID, user, admin)
	if err != nil {
		return nil, err
	}
	if len(normalized) > ImageTagLimit {
		return nil, ErrImageTagLimit
	}
	ids := make([]int64, 0, len(normalized))
	for _, name := range normalized {
		tag, err := ensureTagTx(ctx, tx, owner, name)
		if err != nil {
			return nil, err
		}
		ids = append(ids, tag.ID)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM image_tags WHERE image_id=?", imageID); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT INTO image_tags(image_id,tag_id) VALUES(?,?)", imageID, id); err != nil {
			return nil, err
		}
	}
	tags, err := imageTagsTx(ctx, tx, imageID, owner)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tags, nil
}

// imageTagOwner resolves the image owner after checking row access. A caller
// may act on their own image, or an active admin may act on any image; anything
// else is reported as ErrImageNotFound so existence is not leaked.
func imageTagOwner(ctx context.Context, tx *sql.Tx, imageID string, user int64, admin bool) (int64, error) {
	var owner int64
	err := tx.QueryRowContext(ctx, "SELECT user_id FROM images WHERE id=?", imageID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrImageNotFound
	}
	if err != nil {
		return 0, err
	}
	allowed, err := galleryAccess(tx, user, admin)
	if err != nil {
		return 0, err
	}
	if owner != user && !allowed {
		return 0, ErrImageNotFound
	}
	return owner, nil
}

// imageTagIDsTx reads the tag ids currently attached to an image, restricted to
// the owner's library.
func imageTagIDsTx(ctx context.Context, tx *sql.Tx, imageID string, owner int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT it.tag_id FROM image_tags it JOIN tags t ON t.id=it.tag_id WHERE it.image_id=? AND t.user_id=?", imageID, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// imageTagsTx reads the tags attached to an image and names them.
func imageTagsTx(ctx context.Context, tx *sql.Tx, imageID string, owner int64) ([]ImageTag, error) {
	rows, err := tx.QueryContext(ctx, "SELECT t.id,t.name FROM image_tags it JOIN tags t ON t.id=it.tag_id WHERE it.image_id=? AND t.user_id=? ORDER BY t.name,t.id", imageID, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := make([]ImageTag, 0)
	for rows.Next() {
		var tag ImageTag
		tag.ImageID = imageID
		if err := rows.Scan(&tag.TagID, &tag.Name); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tags, nil
}
