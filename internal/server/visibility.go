package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

// visibilityDefaultKeyLifetimeMS is the frozen contract default used when the
// configured default is outside the range the store accepts.
const visibilityDefaultKeyLifetimeMS int64 = 1800000

// visibilityImage resolves the record named by :id and enforces management
// authority. Key lifecycle is owner-only (R03 D2), so key handlers pass
// allowAdmin=false; visibility changes may be assisted by an administrator.
// Non-owners receive 403 rather than the gallery's 404 because this endpoint
// never exposes record content.
func visibilityImage(service *uploads.Service, session auth.Session, c *gin.Context, allowAdmin bool) (store.Image, bool) {
	image, err := service.DB.Image(c.Param("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(404, gin.H{"error": "Image not found"})
		} else {
			c.JSON(500, gin.H{"error": "Image operation failed"})
		}
		return store.Image{}, false
	}
	if image.UserID != session.User.ID && !(allowAdmin && session.User.Admin) {
		c.JSON(403, gin.H{"error": "Image operation is not permitted"})
		return store.Image{}, false
	}
	return image, true
}

// visibilityError maps lifecycle failures without echoing SQL or key material.
func visibilityError(c *gin.Context, err error, message string) {
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, store.ErrImageNotFound):
		c.JSON(404, gin.H{"error": "Image not found"})
	case errors.Is(err, store.ErrVisibility):
		c.JSON(400, gin.H{"error": "Invalid image visibility"})
	case errors.Is(err, store.ErrKeyLifetime):
		c.JSON(400, gin.H{"error": "Invalid key lifetime"})
	default:
		c.JSON(500, gin.H{"error": message})
	}
}

// visibilityLifetimeBody parses the optional {"lifetime_ms":N} key request.
// An empty body or an empty object selects the configured default; unknown,
// duplicate, null and non-integer fields are rejected.
func visibilityLifetimeBody(c *gin.Context) (int64, bool, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	decoder := json.NewDecoder(c.Request.Body)
	token, err := decoder.Token()
	if err == io.EOF {
		return 0, false, true
	}
	if err != nil || token != json.Delim('{') {
		return 0, false, false
	}
	var lifetime int64
	specified := false
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return 0, false, false
		}
		name, ok := field.(string)
		if !ok || name != "lifetime_ms" || specified {
			return 0, false, false
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || string(raw) == "null" || json.Unmarshal(raw, &lifetime) != nil {
			return 0, false, false
		}
		specified = true
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return 0, false, false
	}
	return lifetime, specified, true
}

// visibilityDefaultLifetime returns the configured default when it is inside
// the accepted per-record bounds, otherwise the contract default. Configuration
// validation only requires a positive value, so an out-of-range value must not
// make every key issue request fail.
func visibilityDefaultLifetime(service *uploads.Service) int64 {
	lifetime := service.Manager.Snapshot().Policy.PrivateKeyDefaultMS
	if lifetime < store.MinKeyLifetimeMS || lifetime > store.MaxKeyLifetimeMS {
		return visibilityDefaultKeyLifetimeMS
	}
	return lifetime
}

func visibilityRoutes(router *gin.Engine, authentication *auth.Service, service *uploads.Service) {
	router.POST("/api/v1/images/:id/visibility", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		image, ok := visibilityImage(service, session, c, true)
		if !ok {
			return
		}
		var visibility store.Visibility
		if !accountBody(c, map[string]any{"visibility": &visibility}, true) {
			c.JSON(400, gin.H{"error": "Invalid visibility request"})
			return
		}
		if visibility != store.VisibilityPublic && visibility != store.VisibilityPrivate {
			c.JSON(400, gin.H{"error": "Invalid image visibility"})
			return
		}
		if err := service.DB.SetVisibility(c.Request.Context(), image.ID, visibility); err != nil {
			visibilityError(c, err, "Visibility change failed")
			return
		}
		// A visibility transition invalidates page visits, so a later private
		// period cannot revive grants issued before the record became public.
		if image.Visibility != visibility {
			privateVisibilityChanged(service, image.ID)
		}
		c.JSON(200, gin.H{"visibility": visibility})
	}))
	router.GET("/api/v1/images/:id/visibility", requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		image, ok := visibilityImage(service, session, c, true)
		if !ok {
			return
		}
		value := gin.H{"visibility": image.Visibility, "key": nil}
		record, err := service.DB.Key(c.Request.Context(), image.ID)
		switch {
		case err == nil:
			value["key"] = gin.H{
				"generation":  record.Generation,
				"expires_at":  record.ExpiresAt,
				"lifetime_ms": record.LifetimeMS,
				"issued":      time.Now().UTC().UnixMilli() < record.ExpiresAt,
			}
		case errors.Is(err, sql.ErrNoRows):
		default:
			c.JSON(500, gin.H{"error": "Image key operation failed"})
			return
		}
		c.JSON(200, value)
	}))
	router.POST("/api/v1/images/:id/key", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		image, ok := visibilityImage(service, session, c, false)
		if !ok {
			return
		}
		if image.Visibility != store.VisibilityPrivate {
			c.JSON(409, gin.H{"error": "Image is not private"})
			return
		}
		lifetime, specified, ok := visibilityLifetimeBody(c)
		if !ok {
			c.JSON(400, gin.H{"error": "Invalid key request"})
			return
		}
		if !specified {
			lifetime = visibilityDefaultLifetime(service)
		}
		if lifetime < store.MinKeyLifetimeMS || lifetime > store.MaxKeyLifetimeMS {
			c.JSON(400, gin.H{"error": "Invalid key lifetime"})
			return
		}
		plaintext, record, err := service.DB.IssueKey(c.Request.Context(), image.ID, lifetime, time.Now().UTC())
		if err != nil {
			visibilityError(c, err, "Image key operation failed")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(201, gin.H{
			"generation":  record.Generation,
			"expires_at":  record.ExpiresAt,
			"lifetime_ms": record.LifetimeMS,
			"key":         plaintext,
		})
	}))
	router.DELETE("/api/v1/images/:id/key", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		image, ok := visibilityImage(service, session, c, false)
		if !ok {
			return
		}
		if err := service.DB.RevokeKey(c.Request.Context(), image.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			visibilityError(c, err, "Image key operation failed")
			return
		}
		c.Status(204)
	}))
}
