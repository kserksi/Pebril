package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"html/template"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/objectstore"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

// Private-page delivery constants. A page grant is a server-side credential:
// the browser receives an opaque token after key verification and repeats it in
// X-Pebril-Grant for content requests of the same visit. It is never a cookie,
// never part of a URL and never persisted by the server beyond its lifetime.
const (
	privateGrantHeader       = "X-Pebril-Grant"
	privateGrantLifetime     = 30 * time.Minute
	privateGrantCap          = 4096
	privateVerifyDelay       = 100 * time.Millisecond
	privateVerifyWindow      = time.Minute
	privateVerifyIPLimit     = 10
	privateVerifyRecordLimit = 5
	privatePenaltyBase       = 30 * time.Second
	privatePenaltyMax        = 15 * time.Minute
)

// pageGrant is the in-memory binding of one verified visit. Fingerprint is the
// one-way key binding from the store; Generation detects rotation and epoch
// invalidates grants after a visibility transition.
type pageGrant struct {
	imageID     string
	fingerprint string
	generation  int64
	expiresAt   int64
	epoch       int64
}

// pageGrants is the process-local grant table for one uploads service. Grants
// are bounded by privateGrantCap and disappear with the process, so a refresh,
// a new tab or a restart always requires manual key entry again.
type pageGrants struct {
	mu      sync.Mutex
	entries map[string]pageGrant
	epochs  map[string]int64
}

func newPageGrants() *pageGrants {
	return &pageGrants{entries: make(map[string]pageGrant), epochs: make(map[string]int64)}
}

// issue creates a grant for one verified key binding. Its expiry is the frozen
// contract minimum of the key expiry and the grant lifetime, so a grant can
// never outlive its key.
func (g *pageGrants) issue(binding store.KeyGrant, now time.Time) (string, int64, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", 0, err
	}
	token := hex.EncodeToString(buffer)
	expiresAt := now.Add(privateGrantLifetime).UnixMilli()
	if binding.ExpiresAt < expiresAt {
		expiresAt = binding.ExpiresAt
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.entries) >= privateGrantCap {
		for value, grant := range g.entries {
			if now.UnixMilli() >= grant.expiresAt {
				delete(g.entries, value)
			}
		}
		if len(g.entries) >= privateGrantCap {
			for value := range g.entries {
				delete(g.entries, value)
				break
			}
		}
	}
	g.entries[token] = pageGrant{imageID: binding.ImageID, fingerprint: binding.Fingerprint, generation: binding.Generation, expiresAt: expiresAt, epoch: g.epochs[binding.ImageID]}
	return token, expiresAt, nil
}

// use returns the grant only when it belongs to this record, has not expired and
// was not invalidated by a visibility transition.
func (g *pageGrants) use(token, imageID string, now time.Time) (pageGrant, bool) {
	if len(token) != 64 {
		return pageGrant{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	grant, ok := g.entries[token]
	if !ok {
		return pageGrant{}, false
	}
	if now.UnixMilli() >= grant.expiresAt {
		delete(g.entries, token)
		return pageGrant{}, false
	}
	if grant.imageID != imageID || grant.epoch != g.epochs[imageID] {
		return pageGrant{}, false
	}
	return grant, true
}

// invalidate drops every grant of one record. It is called after a visibility
// transition so a later private period cannot revive an earlier visit.
func (g *pageGrants) invalidate(imageID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.epochs[imageID]++
	for token, grant := range g.entries {
		if grant.imageID == imageID {
			delete(g.entries, token)
		}
	}
}

// privateGrantsByService keeps visits process-local and scoped to the uploads
// service that authorized them. A grant therefore cannot be replayed against
// another deployment or another service instance.
var privateGrantsByService sync.Map

func pageGrantsFor(service *uploads.Service) *pageGrants {
	value, _ := privateGrantsByService.LoadOrStore(service, newPageGrants())
	return value.(*pageGrants)
}

func privateVisibilityChanged(service *uploads.Service, imageID string) {
	if value, ok := privateGrantsByService.Load(service); ok {
		value.(*pageGrants).invalidate(imageID)
	}
}

// privateRateWindow counts verify attempts per client address and per record.
// Exceeding the window starts an exponential backoff reported via Retry-After.
type privateRateWindow struct {
	start        time.Time
	count        int
	blockedUntil time.Time
	level        int
}

type privateRateLimiter struct {
	mu      sync.Mutex
	entries map[string]*privateRateWindow
}

func newPrivateRateLimiter() *privateRateLimiter {
	return &privateRateLimiter{entries: make(map[string]*privateRateWindow)}
}

func (l *privateRateLimiter) allow(key string, limit int, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) > 2048 {
		for value, window := range l.entries {
			if now.Sub(window.start) > time.Hour && now.After(window.blockedUntil) {
				delete(l.entries, value)
			}
		}
	}
	window, ok := l.entries[key]
	if !ok {
		window = &privateRateWindow{start: now}
		l.entries[key] = window
	}
	if now.Before(window.blockedUntil) {
		return false, int((window.blockedUntil.Sub(now) + time.Second - 1) / time.Second)
	}
	if now.Sub(window.start) >= privateVerifyWindow {
		window.start = now
		window.count = 0
	}
	window.count++
	if window.count <= limit {
		return true, 0
	}
	shift := window.level
	if shift > 10 {
		shift = 10
	}
	duration := privatePenaltyBase << shift
	if duration > privatePenaltyMax {
		duration = privatePenaltyMax
	}
	window.level++
	window.blockedUntil = now.Add(duration)
	return false, int((duration + time.Second - 1) / time.Second)
}

func privateClient(c *gin.Context) string {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil || host == "" {
		if c.Request.RemoteAddr == "" {
			return "unknown"
		}
		return c.Request.RemoteAddr
	}
	return host
}

func privateRetryAfter(c *gin.Context, seconds int) {
	if seconds < 1 {
		seconds = 1
	}
	c.Header("Retry-After", strconv.Itoa(seconds))
}

func privateRoutes(router *gin.Engine, service *uploads.Service) {
	grants := pageGrantsFor(service)
	limiter := newPrivateRateLimiter()
	router.GET("/p/:id", privateLanding(service))
	router.GET("/private.js", privateScript)
	router.POST("/api/v1/private/:id/verify", privateVerify(service, grants, limiter))
	for _, role := range []string{"result", "cover", "original"} {
		path := "/p/:id/content"
		if role != "result" {
			path += "/" + role
		}
		handler := privateContent(service, grants, role)
		router.GET(path, handler)
		router.HEAD(path, handler)
	}
}

// privatePagePolicy narrows the site policy to what the private shell needs: its
// own stylesheet and script, images rendered from same-origin blob URLs, and
// fetch calls only to this origin. Inline script, style and eval stay forbidden.
const privatePagePolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// privateUnavailableHTML is a constant so that unknown, public, deleted and
// otherwise unavailable records return byte-identical generic pages.
const privateUnavailableHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="referrer" content="no-referrer"><meta name="robots" content="noindex"><title>Not available</title><link rel="stylesheet" href="/style.css"></head><body><main><p class="eyebrow">PEBRIL</p><h1>This page is not available.</h1><p>The private image you requested does not exist or is no longer available.</p></main></body></html>`

// privatePageData carries the only request-derived landing values. html/template
// escapes the image id for its attribute context and the owner name for text.
type privatePageData struct {
	ImageID string
	Owner   string
}

var privatePageTemplate = template.Must(template.New("private").ParseFS(assets, "web/private.html"))

// privateLanding answers the public shell of a private page. It never includes
// image metadata or content before verification.
func privateLanding(service *uploads.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("CDN-Cache-Control", "no-store")
		c.Header("Cloudflare-CDN-Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Robots-Tag", "noindex")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Security-Policy", privatePagePolicy)
		image, err := service.PrivateImage(c.Param("id"))
		if err != nil {
			c.Data(http.StatusNotFound, "text/html; charset=utf-8", []byte(privateUnavailableHTML))
			return
		}
		owner, err := service.DB.User(image.UserID)
		if err != nil {
			c.Data(http.StatusNotFound, "text/html; charset=utf-8", []byte(privateUnavailableHTML))
			return
		}
		data := privatePageData{ImageID: image.ID, Owner: owner.Username}
		var page bytes.Buffer
		if err := privatePageTemplate.ExecuteTemplate(&page, "private.html", data); err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", page.Bytes())
	}
}

// privateScript serves the private landing script from the embedded assets.
func privateScript(c *gin.Context) {
	script, err := assets.ReadFile("web/private.js")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("X-Robots-Tag", "noindex")
	c.Data(http.StatusOK, "text/javascript; charset=utf-8", script)
}

// privateVerify exchanges a record key for one visit grant. It is
// unauthenticated and CSRF-exempt, so it must not consult cookies; failures are
// indistinguishable between a wrong key and an unknown record.
func privateVerify(service *uploads.Service, grants *pageGrants, limiter *privateRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if c.GetHeader("Authorization") != "" {
			c.JSON(403, gin.H{"error": "forbidden"})
			return
		}
		// Browsers send Origin on this cross-document POST; when present it must
		// match. Requests without Origin (non-browser clients) have no ambient
		// credential to protect and remain acceptable.
		if c.GetHeader("Origin") != "" && !sameOrigin(c) {
			c.JSON(403, gin.H{"error": "forbidden"})
			return
		}
		now := time.Now()
		var key string
		if !accountBody(c, map[string]any{"key": &key}, true) || len(key) > 128 || !store.ValidKeyFormat(key) {
			c.JSON(400, gin.H{"error": "invalid_request"})
			return
		}
		id := c.Param("id")
		record := id
		if len(record) > 128 {
			record = record[:128]
		}
		if allowed, retry := limiter.allow("ip:"+privateClient(c), privateVerifyIPLimit, now); !allowed {
			privateRetryAfter(c, retry)
			c.JSON(429, gin.H{"error": "too_many_attempts"})
			return
		}
		if allowed, retry := limiter.allow("record:"+record, privateVerifyRecordLimit, now); !allowed {
			privateRetryAfter(c, retry)
			c.JSON(429, gin.H{"error": "too_many_attempts"})
			return
		}
		binding, err := service.DB.VerifyGrant(c.Request.Context(), id, key, now)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrKeyRevoked):
				c.JSON(403, gin.H{"error": "key_revoked"})
			case errors.Is(err, store.ErrKeyExpired):
				c.JSON(403, gin.H{"error": "key_expired"})
			default:
				// Includes ErrImageNotFound and ErrKeyInvalid: both must look the
				// same and cost the same before answering.
				time.Sleep(privateVerifyDelay)
				c.JSON(401, gin.H{"error": "key_invalid"})
			}
			return
		}
		token, expiresAt, err := grants.issue(binding, now)
		if err != nil {
			c.JSON(503, gin.H{"error": "internal_error"})
			return
		}
		c.JSON(200, gin.H{"grant": token, "expires_at": expiresAt})
	}
}

// privateContent serves one content role only after a valid visit grant. The
// grant is checked before any range or conditional processing, so 206/304 can
// never leak existence or content without authorization.
func privateContent(service *uploads.Service, grants *pageGrants, role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("CDN-Cache-Control", "no-store")
		c.Header("Cloudflare-CDN-Cache-Control", "no-store")
		c.Header("Vary", privateGrantHeader)
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
		id := c.Param("id")
		// A record that is missing, no longer private or owned by an inactive
		// account is indistinguishable from an unknown private path.
		if _, err := service.PrivateImage(id); err != nil {
			c.JSON(404, gin.H{"error": "not_found"})
			return
		}
		now := time.Now()
		grant, ok := grants.use(c.GetHeader(privateGrantHeader), id, now)
		if !ok {
			c.JSON(401, gin.H{"error": "grant_required"})
			return
		}
		err := service.DB.GrantValid(c.Request.Context(), store.KeyGrant{ImageID: id, Generation: grant.generation, Fingerprint: grant.fingerprint, ExpiresAt: grant.expiresAt}, now)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrKeyRotated):
			c.JSON(401, gin.H{"error": "grant_stale"})
			return
		case errors.Is(err, store.ErrImageNotFound):
			c.JSON(404, gin.H{"error": "not_found"})
			return
		case errors.Is(err, store.ErrKeyInvalid), errors.Is(err, store.ErrKeyRevoked), errors.Is(err, store.ErrKeyExpired):
			c.JSON(401, gin.H{"error": "grant_required"})
			return
		default:
			c.JSON(500, gin.H{"error": "internal_error"})
			return
		}
		_, reference, file, err := service.OpenPrivate(c.Request.Context(), id, role)
		if err != nil {
			if errors.Is(err, objectstore.ErrUnavailable) {
				c.JSON(503, gin.H{"error": "storage_unavailable"})
			} else if errors.Is(err, objectstore.ErrIntegrity) {
				c.JSON(500, gin.H{"error": "internal_error"})
			} else {
				c.JSON(404, gin.H{"error": "not_found"})
			}
			return
		}
		defer file.Close()
		types := map[string]string{"svg": "image/svg+xml", "png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp", "gif": "image/gif", "avif": "image/avif"}
		kind, supported := types[reference.Format]
		if !supported {
			c.JSON(500, gin.H{"error": "internal_error"})
			return
		}
		name := "image"
		if extension := strings.ToLower(reference.Format); extension != "" {
			name += "." + extension
		}
		disposition := "inline"
		if role == "original" {
			disposition = "attachment"
		}
		c.Header("Content-Type", kind)
		c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
		http.ServeContent(c.Writer, c.Request, name, time.Time{}, file)
	}
}
