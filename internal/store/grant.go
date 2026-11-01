package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// ErrKeyRotated reports that a page grant was issued for a key generation that
// has since been replaced. It is distinct so private delivery can answer
// grant_stale without revealing key material.
var ErrKeyRotated = errors.New("Image key generation was replaced")

// KeyGrant is the server-side binding created by a successful verification.
// Fingerprint is a one-way digest of the stored key hash: a page grant keeps
// only this value, so neither the key nor its hash has to be retained.
type KeyGrant struct {
	ImageID     string
	Generation  int64
	ExpiresAt   int64
	Fingerprint string
}

// ValidKeyFormat reports whether a presented value has the canonical or
// displayed key shape. Shape errors are request errors, not verification
// failures, and are decided before any record lookup.
func ValidKeyFormat(value string) bool {
	_, ok := canonicalKey(value)
	return ok
}

// VerifyGrant verifies a presented key and returns the binding a page grant
// must retain. It repeats VerifyKey's checks and additionally requires the
// record to still be private and its owner account to be active.
func (s *Store) VerifyGrant(ctx context.Context, imageID, value string, now time.Time) (KeyGrant, error) {
	key, ok := canonicalKey(value)
	if !ok {
		return KeyGrant{}, ErrKeyInvalid
	}
	hash, generation, expiresAt, err := s.keyGrantRow(ctx, imageID, now)
	if err != nil {
		return KeyGrant{}, err
	}
	if subtle.ConstantTimeCompare([]byte(keyHash(key)), []byte(hash)) != 1 {
		return KeyGrant{}, ErrKeyInvalid
	}
	return KeyGrant{ImageID: imageID, Generation: generation, ExpiresAt: expiresAt, Fingerprint: grantFingerprint(hash)}, nil
}

// GrantValid re-checks a page grant against the current record. Every private
// content request calls it: rotation, revocation, expiry, visibility changes and
// account changes all invalidate previously issued grants.
func (s *Store) GrantValid(ctx context.Context, grant KeyGrant, now time.Time) error {
	hash, generation, _, err := s.keyGrantRow(ctx, grant.ImageID, now)
	if err != nil {
		return err
	}
	if generation != grant.Generation {
		return ErrKeyRotated
	}
	if subtle.ConstantTimeCompare([]byte(grantFingerprint(hash)), []byte(grant.Fingerprint)) != 1 {
		return ErrKeyInvalid
	}
	return nil
}

// keyGrantRow loads the private-access state of one record. Missing records,
// records that are no longer private and records owned by inactive accounts are
// reported as ErrImageNotFound so callers answer 404. A missing or expired key
// keeps its existing sentinel error.
func (s *Store) keyGrantRow(ctx context.Context, imageID string, now time.Time) (string, int64, int64, error) {
	var visibility, status string
	var hash sql.NullString
	var generation, expiresAt sql.NullInt64
	err := s.DB.QueryRowContext(ctx, "SELECT i.visibility,u.status,k.hash,k.generation,k.expires_at FROM images i JOIN users u ON u.id=i.user_id LEFT JOIN image_keys k ON k.image_id=i.id WHERE i.id=?", imageID).Scan(&visibility, &status, &hash, &generation, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, 0, ErrImageNotFound
	}
	if err != nil {
		return "", 0, 0, err
	}
	if visibility != string(VisibilityPrivate) || status != "active" {
		return "", 0, 0, ErrImageNotFound
	}
	if !hash.Valid || !generation.Valid || !expiresAt.Valid {
		return "", 0, 0, ErrKeyRevoked
	}
	if now.UTC().UnixMilli() >= expiresAt.Int64 {
		return "", 0, 0, ErrKeyExpired
	}
	return hash.String, generation.Int64, expiresAt.Int64, nil
}

// grantFingerprint derives the opaque value retained by a page grant. The
// domain prefix keeps it distinct from the stored key hash.
func grantFingerprint(hash string) string {
	sum := sha256.Sum256([]byte("pebril-page-grant\x00" + hash))
	return hex.EncodeToString(sum[:])
}
