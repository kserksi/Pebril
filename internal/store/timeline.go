package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultGalleryLimit is the page size used when a caller supplies none.
	DefaultGalleryLimit = 40
	// MaxGalleryLimit caps every gallery page.
	MaxGalleryLimit = 100
)

const dayMilliseconds int64 = 24 * 60 * 60 * 1000

var (
	// ErrTagMode marks an unsupported tag match mode.
	ErrTagMode = errors.New("Invalid tag match mode")
	// ErrGalleryCursor marks a malformed gallery page cursor.
	ErrGalleryCursor = errors.New("Invalid gallery cursor")
)

// TagMatchMode selects how multiple tag filters combine.
type TagMatchMode string

const (
	// TagMatchAll requires every requested tag (AND).
	TagMatchAll TagMatchMode = "all"
	// TagMatchAny accepts any requested tag (OR).
	TagMatchAny TagMatchMode = "any"
)

// GalleryFilter is the store-level gallery query shared by the image list and
// the timeline grouping. UserID is the caller; Admin is the caller's claimed
// admin flag, revalidated against the users row. All widens the image scope to
// every user. Tags resolve in the caller's own library. From and To are UTC
// milliseconds forming a half-open [From,To) range on images.created_at.
type GalleryFilter struct {
	UserID int64
	Admin  bool
	All    bool
	Tags   []string
	Mode   TagMatchMode
	From   *int64
	To     *int64
}

// GalleryCursor is a stable keyset position: rows strictly older than
// (CreatedAt, ID) in the (created_at DESC, id DESC) order.
type GalleryCursor struct {
	CreatedAt int64  `json:"created_at"`
	ID        string `json:"id"`
}

// GalleryPage is one page of gallery images plus the realtime total for the
// filter and the cursor for the next page, if any.
type GalleryPage struct {
	Images []Image        `json:"images"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Next   *GalleryCursor `json:"next"`
}

// TimelineBucket is one UTC day bucket of the timeline. Start is inclusive and
// End is exclusive.
type TimelineBucket struct {
	Day   string `json:"day"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Count int    `json:"count"`
}

// GalleryImages returns one keyset page of the caller's images with the
// total count for the filter. An empty result caused by a filter name that does
// not exist in the caller's library is not an error.
func (s *Store) GalleryImages(ctx context.Context, filter GalleryFilter, before *GalleryCursor, limit int) (GalleryPage, error) {
	page := GalleryPage{Images: make([]Image, 0), Limit: galleryLimit(limit)}
	if before != nil && (before.ID == "" || len(before.ID) > 128) {
		return page, ErrGalleryCursor
	}
	if filter.From != nil && filter.To != nil && *filter.From >= *filter.To {
		return page, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	where, args, empty, err := galleryScope(ctx, tx, filter)
	if err != nil {
		return page, err
	}
	if empty {
		return page, tx.Commit()
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM images"+where, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	query := "SELECT id FROM images" + where
	queryArgs := append([]any{}, args...)
	if before != nil {
		query += " AND (images.created_at<? OR (images.created_at=? AND images.id<?))"
		queryArgs = append(queryArgs, before.CreatedAt, before.CreatedAt, before.ID)
	}
	query += " ORDER BY images.created_at DESC,images.id DESC LIMIT ?"
	queryArgs = append(queryArgs, page.Limit+1)
	rows, err := tx.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return page, err
	}
	ids := make([]string, 0, page.Limit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return page, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return page, err
	}
	rows.Close()
	more := len(ids) > page.Limit
	if more {
		ids = ids[:page.Limit]
	}
	for _, id := range ids {
		image, err := readImage(tx, id)
		if err != nil {
			return page, err
		}
		page.Images = append(page.Images, image)
	}
	if more && len(page.Images) > 0 {
		last := page.Images[len(page.Images)-1]
		next := GalleryCursor{CreatedAt: last.Created, ID: last.ID}
		page.Next = &next
	}
	if err := tx.Commit(); err != nil {
		return page, err
	}
	return page, nil
}

// TimelineBuckets groups the caller's images into UTC day buckets. Counts are
// realtime and only days that contain matching images are returned, newest
// first. From and To follow the same half-open range as GalleryImages.
func (s *Store) TimelineBuckets(ctx context.Context, filter GalleryFilter) ([]TimelineBucket, error) {
	buckets := make([]TimelineBucket, 0)
	if filter.From != nil && filter.To != nil && *filter.From >= *filter.To {
		return buckets, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return buckets, err
	}
	defer tx.Rollback()
	where, args, empty, err := galleryScope(ctx, tx, filter)
	if err != nil {
		return buckets, err
	}
	if empty {
		return buckets, tx.Commit()
	}
	query := "SELECT " + s.dayIndexSQL() + " AS day_index,count(*) FROM images" + where + " GROUP BY day_index ORDER BY day_index DESC"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return buckets, err
	}
	for rows.Next() {
		var index int64
		var count int
		if err := rows.Scan(&index, &count); err != nil {
			rows.Close()
			return buckets, err
		}
		start := index * dayMilliseconds
		buckets = append(buckets, TimelineBucket{
			Day:   time.UnixMilli(start).UTC().Format("2006-01-02"),
			Start: start,
			End:   start + dayMilliseconds,
			Count: count,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return buckets, err
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return buckets, err
	}
	return buckets, nil
}

// EncodeGalleryCursor encodes a keyset cursor for the API. It is URL-safe and
// unpadded so it can be passed back as one query value.
func EncodeGalleryCursor(cursor GalleryCursor) string {
	value := strconv.FormatInt(cursor.CreatedAt, 10) + ":" + cursor.ID
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

// DecodeGalleryCursor parses a cursor produced by EncodeGalleryCursor.
func DecodeGalleryCursor(value string) (GalleryCursor, error) {
	if value == "" {
		return GalleryCursor{}, ErrGalleryCursor
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return GalleryCursor{}, ErrGalleryCursor
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 || parts[1] == "" || len(parts[1]) > 128 {
		return GalleryCursor{}, ErrGalleryCursor
	}
	created, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return GalleryCursor{}, ErrGalleryCursor
	}
	return GalleryCursor{CreatedAt: created, ID: parts[1]}, nil
}

// galleryLimit clamps a requested page size into the contract bounds.
func galleryLimit(limit int) int {
	if limit < 1 {
		return DefaultGalleryLimit
	}
	if limit > MaxGalleryLimit {
		return MaxGalleryLimit
	}
	return limit
}

// normalizeTagMode applies the "all" default.
func normalizeTagMode(mode TagMatchMode) (TagMatchMode, error) {
	switch mode {
	case "", TagMatchAll:
		return TagMatchAll, nil
	case TagMatchAny:
		return TagMatchAny, nil
	default:
		return "", ErrTagMode
	}
}

// galleryScope builds the shared WHERE clause for GalleryImages and
// TimelineBuckets. It revalidates admin access, composes the owner/all scope
// with the half-open time range and tag matching, and reports empty when a tag
// filter name is absent from the library.
func galleryScope(ctx context.Context, tx *sql.Tx, filter GalleryFilter) (string, []any, bool, error) {
	mode, err := normalizeTagMode(filter.Mode)
	if err != nil {
		return "", nil, false, err
	}
	allowedAdmin, err := galleryAccess(tx, filter.UserID, filter.Admin)
	if err != nil {
		return "", nil, false, err
	}
	if filter.All && !allowedAdmin {
		return "", nil, false, ErrPermission
	}
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 8)
	if filter.All {
		conditions = append(conditions, "(images.user_id=? OR ?)")
		args = append(args, filter.UserID, allowedAdmin)
	} else {
		conditions = append(conditions, "images.user_id=?")
		args = append(args, filter.UserID)
	}
	if filter.From != nil {
		conditions = append(conditions, "images.created_at>=?")
		args = append(args, *filter.From)
	}
	if filter.To != nil {
		conditions = append(conditions, "images.created_at<?")
		args = append(args, *filter.To)
	}
	normalized, err := normalizeTagNames(filter.Tags)
	if err != nil {
		return "", nil, false, err
	}
	if len(normalized) > 0 {
		ids, complete, err := resolveTagIDs(ctx, tx, filter.UserID, normalized)
		if err != nil {
			return "", nil, false, err
		}
		if !complete {
			return "", nil, true, nil
		}
		placeholders := tagPlaceholders(len(ids))
		if mode == TagMatchAny {
			conditions = append(conditions, "EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id=images.id AND it.tag_id IN ("+placeholders+"))")
			for _, id := range ids {
				args = append(args, id)
			}
		} else {
			conditions = append(conditions, "(SELECT count(DISTINCT it.tag_id) FROM image_tags it WHERE it.image_id=images.id AND it.tag_id IN ("+placeholders+"))=?")
			for _, id := range ids {
				args = append(args, id)
			}
			args = append(args, len(ids))
		}
	}
	return " WHERE " + strings.Join(conditions, " AND "), args, false, nil
}

// resolveTagIDs maps normalized names to ids in one library. complete is false
// when at least one name is absent, which the contract turns into an empty
// result rather than an error.
func resolveTagIDs(ctx context.Context, tx *sql.Tx, userID int64, names []string) ([]int64, bool, error) {
	args := make([]any, 0, len(names)+1)
	args = append(args, userID)
	for _, name := range names {
		args = append(args, name)
	}
	query := "SELECT id,name FROM tags WHERE user_id=? AND name IN (" + tagPlaceholders(len(names)) + ")"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	found := make(map[string]int64, len(names))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, false, err
		}
		found[name] = id
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(found) != len(names) {
		return nil, false, nil
	}
	ids := make([]int64, len(names))
	for i, name := range names {
		ids[i] = found[name]
	}
	return ids, true, nil
}

// tagPlaceholders returns n comma-separated '?' placeholders.
func tagPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// dayIndexSQL returns the backend expression for the UTC day index of
// images.created_at, which the timeline groups by.
func (s *Store) dayIndexSQL() string {
	if s.Backend() == "mysql" {
		return fmt.Sprintf("CAST(FLOOR(images.created_at/%d) AS SIGNED)", dayMilliseconds)
	}
	return fmt.Sprintf("images.created_at/%d", dayMilliseconds)
}
