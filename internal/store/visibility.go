package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Visibility controls how a record can be reached. Public records keep the
// existing unauthenticated URL; private records require a verified key.
type Visibility string

const (
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

// Image key lifetimes accepted by IssueKey, in milliseconds.
const (
	MinKeyLifetimeMS int64 = 60000
	MaxKeyLifetimeMS int64 = 259200000
)

// Key lifecycle errors are distinct so callers can map them to the frozen
// private-access error codes without inspecting error text.
var (
	ErrVisibility    = errors.New("Invalid image visibility")
	ErrImageNotFound = errors.New("Image not found")
	ErrKeyInvalid    = errors.New("Image key is invalid")
	ErrKeyRevoked    = errors.New("Image key is revoked")
	ErrKeyExpired    = errors.New("Image key has expired")
	ErrKeyLifetime   = errors.New("Image key lifetime is outside the supported range")
)

const (
	keyBytes  = 16
	keyLength = 26
)

// Crockford base32 omits I, L, O and U so handwritten keys cannot be confused.
var keyEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// KeyRecord is the stored metadata of the single active key for a record.
// The plaintext and its hash are intentionally absent: callers receive the
// plaintext once from IssueKey and must never persist it.
type KeyRecord struct {
	ImageID    string
	Generation int64
	CreatedAt  int64
	ExpiresAt  int64
	LifetimeMS int64
}

// SetVisibility changes whether a record is reachable through the public path.
// Keys are kept across transitions: public delivery ignores them and private
// delivery requires them, so no key lifecycle is bound to this state.
func (s *Store) SetVisibility(ctx context.Context, imageID string, visibility Visibility) error {
	if visibility != VisibilityPublic && visibility != VisibilityPrivate {
		return ErrVisibility
	}
	result, err := s.DB.ExecContext(ctx, "UPDATE images SET visibility=? WHERE id=?", string(visibility), imageID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	// ClientFoundRows makes MySQL report a matched row for an unchanged value,
	// so a zero count always means the record does not exist.
	if affected == 0 {
		return ErrImageNotFound
	}
	return nil
}

// IssueKey creates the first key for a record, or replaces (rotates) the
// existing row and increments its generation. Only the SHA-256 hash is stored;
// the returned plaintext is the only copy and cannot be recovered later.
func (s *Store) IssueKey(ctx context.Context, imageID string, lifetimeMS int64, now time.Time) (string, KeyRecord, error) {
	if lifetimeMS < MinKeyLifetimeMS || lifetimeMS > MaxKeyLifetimeMS {
		return "", KeyRecord{}, ErrKeyLifetime
	}
	plaintext, err := newKey()
	if err != nil {
		return "", KeyRecord{}, err
	}
	stamp := now.UTC().UnixMilli()
	record := KeyRecord{
		ImageID:    imageID,
		CreatedAt:  stamp,
		ExpiresAt:  stamp + lifetimeMS,
		LifetimeMS: lifetimeMS,
	}
	hash := keyHash(plaintext)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", KeyRecord{}, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT 1 FROM images WHERE id=?", imageID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", KeyRecord{}, ErrImageNotFound
		}
		return "", KeyRecord{}, err
	}
	var generation int64
	err = tx.QueryRowContext(ctx, "SELECT generation FROM image_keys WHERE image_id=?", imageID).Scan(&generation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		generation = 1
		_, err = tx.ExecContext(ctx, "INSERT INTO image_keys(image_id,hash,generation,created_at,expires_at,lifetime_ms) VALUES(?,?,?,?,?,?)", imageID, hash, generation, record.CreatedAt, record.ExpiresAt, record.LifetimeMS)
	case err == nil:
		if generation == int64(^uint64(0)>>1) {
			return "", KeyRecord{}, errors.New("Image key generation exhausted")
		}
		generation++
		_, err = tx.ExecContext(ctx, "UPDATE image_keys SET hash=?,generation=?,created_at=?,expires_at=?,lifetime_ms=? WHERE image_id=?", hash, generation, record.CreatedAt, record.ExpiresAt, record.LifetimeMS, imageID)
	}
	if err != nil {
		return "", KeyRecord{}, err
	}
	record.Generation = generation
	if err = tx.Commit(); err != nil {
		return "", KeyRecord{}, err
	}
	return plaintext, record, nil
}

// RotateKey replaces the active key with a new generation. It is the same
// operation as reissuing, so it shares IssueKey's validation and storage.
func (s *Store) RotateKey(ctx context.Context, imageID string, lifetimeMS int64, now time.Time) (string, KeyRecord, error) {
	return s.IssueKey(ctx, imageID, lifetimeMS, now)
}

// RevokeKey deletes the stored key so verification fails with ErrKeyRevoked.
// It reports sql.ErrNoRows when the record already has no key.
func (s *Store) RevokeKey(ctx context.Context, imageID string) error {
	result, err := s.DB.ExecContext(ctx, "DELETE FROM image_keys WHERE image_id=?", imageID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Key returns the stored metadata for a record's active key. The plaintext and
// hash stay inside the store; callers only learn the generation and lifetime.
// It reports sql.ErrNoRows when the record has no key.
func (s *Store) Key(ctx context.Context, imageID string) (KeyRecord, error) {
	record := KeyRecord{ImageID: imageID}
	err := s.DB.QueryRowContext(ctx, "SELECT generation,created_at,expires_at,lifetime_ms FROM image_keys WHERE image_id=?", imageID).Scan(&record.Generation, &record.CreatedAt, &record.ExpiresAt, &record.LifetimeMS)
	return record, err
}

// VerifyKey checks a presented key against the stored hash of one record.
// The grouped display form (dashes and spaces) and case differences are
// accepted because they do not add key space. Comparisons are constant-time
// and neither the plaintext nor the hash is ever returned or logged.
func (s *Store) VerifyKey(ctx context.Context, imageID, value string, now time.Time) error {
	key, ok := canonicalKey(value)
	if !ok {
		return ErrKeyInvalid
	}
	var hash sql.NullString
	var expiresAt sql.NullInt64
	err := s.DB.QueryRowContext(ctx, "SELECT k.hash,k.expires_at FROM images i LEFT JOIN image_keys k ON k.image_id=i.id WHERE i.id=?", imageID).Scan(&hash, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrImageNotFound
	}
	if err != nil {
		return err
	}
	if !hash.Valid || !expiresAt.Valid {
		return ErrKeyRevoked
	}
	if subtle.ConstantTimeCompare([]byte(keyHash(key)), []byte(hash.String)) != 1 {
		return ErrKeyInvalid
	}
	if now.UTC().UnixMilli() >= expiresAt.Int64 {
		return ErrKeyExpired
	}
	return nil
}

func newKey() (string, error) {
	var raw [keyBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return keyEncoding.EncodeToString(raw[:]), nil
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// canonicalKey accepts the displayed grouped form and lower case, then checks
// that the value is a canonical 26-character encoding of a 128-bit key.
func canonicalKey(value string) (string, bool) {
	var normalized strings.Builder
	normalized.Grow(keyLength)
	for _, character := range strings.ToUpper(value) {
		if character == '-' || character == ' ' {
			continue
		}
		normalized.WriteRune(character)
	}
	key := normalized.String()
	if len(key) != keyLength {
		return "", false
	}
	raw, err := keyEncoding.DecodeString(key)
	if err != nil || len(raw) != keyBytes {
		return "", false
	}
	if keyEncoding.EncodeToString(raw) != key {
		return "", false
	}
	return key, true
}
