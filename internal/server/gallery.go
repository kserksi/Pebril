package server

import (
	"database/sql"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/objectstore"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
	"time"
)

type galleryFile struct {
	Role   string `json:"role"`
	Format string `json:"format"`
	Bytes  int64  `json:"bytes"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Frames int    `json:"frames"`
}

type galleryImage struct {
	store.Image
	OwnerID           int64         `json:"owner_id"`
	Files             []galleryFile `json:"files"`
	OriginalAvailable bool          `json:"original_available"`
}

func galleryValue(image store.Image, service *uploads.Service) galleryImage {
	value := galleryImage{Image: image, OwnerID: image.UserID, Files: make([]galleryFile, 0, len(image.Files))}
	for _, file := range image.Files {
		value.Files = append(value.Files, galleryFile{file.Role, file.Format, file.Bytes, file.Width, file.Height, file.Frames})
		if file.Role == "original" && service.Manager.Snapshot().Policy.AllowOriginalDownload {
			value.OriginalAvailable = true
		}
	}
	return value
}

func galleryError(c *gin.Context, err error) {
	if errors.Is(err, objectstore.ErrUnavailable) {
		c.JSON(503, gin.H{"error": "Selected image storage is unavailable"})
		return
	}
	if errors.Is(err, objectstore.ErrIntegrity) {
		c.JSON(500, gin.H{"error": "Stored image failed integrity verification"})
		return
	}
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, os.ErrNotExist):
		c.JSON(404, gin.H{"error": "Image not found"})
	case errors.Is(err, uploads.ErrOriginalDownloadDisabled), errors.Is(err, store.ErrAccountPermission):
		c.JSON(403, gin.H{"error": "Image operation is not permitted"})
	default:
		c.JSON(500, gin.H{"error": "Image operation failed"})
	}
}

func galleryRoutes(router *gin.Engine, authentication *auth.Service, service *uploads.Service) {
	router.GET("/api/v1/images", galleryList(authentication, service))
	router.GET("/api/v1/images/timeline", galleryTimeline(authentication, service))
	router.GET("/api/v1/images/:id", requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		image, err := service.DB.GalleryImage(c.Param("id"), session.User.ID, session.User.Admin)
		if err != nil {
			galleryError(c, err)
			return
		}
		c.JSON(200, galleryValue(image, service))
	}))
	router.DELETE("/api/v1/images/:id", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		if err := service.Delete(c.Param("id"), session.User.ID, session.User.Admin); err != nil {
			galleryError(c, err)
			return
		}
		c.JSON(200, gin.H{"deleted": true})
	}))
	originalDownload := requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		image, reference, file, err := service.OpenOriginal(c.Request.Context(), c.Param("id"), session.User.ID, session.User.Admin)
		if err != nil {
			galleryError(c, err)
			return
		}
		defer file.Close()
		kind := map[string]string{"svg": "image/svg+xml", "png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp", "gif": "image/gif", "avif": "image/avif"}[reference.Format]
		if kind == "" {
			c.JSON(500, gin.H{"error": "Unsupported stored image format"})
			return
		}
		name := image.ID + "." + reference.Format
		c.Header("Content-Type", kind)
		c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
		c.Header("CDN-Cache-Control", "no-store")
		c.Header("Cloudflare-CDN-Cache-Control", "no-store")
		modified := time.Time{}
		if image.Delivered > 0 {
			modified = time.UnixMilli(image.Delivered)
		}
		http.ServeContent(c.Writer, c.Request, name, modified, file)
	})
	router.GET("/api/v1/images/:id/original", originalDownload)
	router.HEAD("/api/v1/images/:id/original", originalDownload)
}

// Gallery and timeline error codes from the frozen 0.0.11 contract.
const (
	codeInvalidGalleryQuery      = "invalid_gallery_query"
	codeInvalidGalleryPagination = "invalid_gallery_pagination"
	codeInvalidGalleryScope      = "invalid_gallery_scope"
	codeInvalidGalleryGroup      = "invalid_gallery_group"
	codeInvalidTagMode           = "invalid_tag_mode"
	codeInvalidTimeRange         = "invalid_time_range"
	codeInvalidCursor            = "invalid_cursor"
)

var (
	errGalleryTimeValue = errors.New("invalid gallery time value")
)

// galleryListKeys is the query allowlist for the image list. tags_mode is the
// contract name; mode is accepted as an alias.
var galleryListKeys = map[string]bool{
	"limit": true, "offset": true, "scope": true, "tags": true, "tags_mode": true, "mode": true, "from": true, "to": true, "before": true, "group": true,
}

var galleryTimelineKeys = map[string]bool{
	"scope": true, "tags": true, "tags_mode": true, "mode": true, "from": true, "to": true,
}

func galleryQueryError(c *gin.Context, message, code string) {
	c.JSON(400, gin.H{"error": message, "code": code})
}

// galleryListQuery rejects unknown and repeated query parameters.
func galleryListQuery(c *gin.Context, keys map[string]bool) (url.Values, bool) {
	query := c.Request.URL.Query()
	for key, values := range query {
		if !keys[key] || len(values) != 1 {
			galleryQueryError(c, "Invalid gallery query", codeInvalidGalleryQuery)
			return nil, false
		}
	}
	return query, true
}

// galleryIntValue returns one canonical decimal query integer.
func galleryIntValue(query url.Values, key string) (int, bool, bool) {
	values, ok := query[key]
	if !ok {
		return 0, false, true
	}
	n, err := strconv.Atoi(values[0])
	if err != nil || strconv.Itoa(n) != values[0] {
		return 0, true, false
	}
	return n, true, true
}

// galleryScopeValue validates the optional own/all scope and the admin claim.
func galleryScopeValue(c *gin.Context, session auth.Session, query url.Values) (string, bool) {
	scope := query.Get("scope")
	if scope != "" && scope != "own" && scope != "all" {
		galleryQueryError(c, "Invalid gallery scope", codeInvalidGalleryScope)
		return "", false
	}
	if scope == "all" && !session.User.Admin {
		c.JSON(403, gin.H{"error": "Administrator access is required"})
		return "", false
	}
	return scope, true
}

// galleryFilterValues parses the filters shared by the image list and the
// timeline. Unknown filter tag names stay valid here; the store resolves them
// to an empty result, as the contract requires.
func galleryFilterValues(c *gin.Context, session auth.Session, query url.Values, scope string) (store.GalleryFilter, bool) {
	filter := store.GalleryFilter{UserID: session.User.ID, Admin: session.User.Admin, All: scope == "all"}
	if _, alias := query["mode"]; alias {
		if _, ok := query["tags_mode"]; ok {
			galleryQueryError(c, "Invalid gallery query", codeInvalidGalleryQuery)
			return filter, false
		}
	}
	if values, ok := query["tags"]; ok && values[0] != "" {
		parts := strings.Split(values[0], ",")
		names := make([]string, 0, len(parts))
		for _, part := range parts {
			name, err := store.NormalizeTagName(part)
			if err != nil {
				tagError(c, err)
				return filter, false
			}
			names = append(names, name)
		}
		filter.Tags = names
	}
	mode := ""
	if values, ok := query["tags_mode"]; ok {
		mode = values[0]
	} else if values, ok := query["mode"]; ok {
		mode = values[0]
	}
	switch mode {
	case "", "all":
		filter.Mode = store.TagMatchAll
	case "any":
		filter.Mode = store.TagMatchAny
	default:
		galleryQueryError(c, store.ErrTagMode.Error(), codeInvalidTagMode)
		return filter, false
	}
	parse := func(key string) (*int64, error) {
		values, ok := query[key]
		if !ok {
			return nil, nil
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || strconv.FormatInt(value, 10) != values[0] {
			return nil, errGalleryTimeValue
		}
		return &value, nil
	}
	from, err := parse("from")
	if err != nil {
		galleryQueryError(c, "Invalid time range", codeInvalidTimeRange)
		return filter, false
	}
	to, err := parse("to")
	if err != nil {
		galleryQueryError(c, "Invalid time range", codeInvalidTimeRange)
		return filter, false
	}
	if from != nil && to != nil && *from > *to {
		galleryQueryError(c, "Invalid time range", codeInvalidTimeRange)
		return filter, false
	}
	filter.From = from
	filter.To = to
	return filter, true
}

// galleryFilterError maps store gallery and timeline failures.
func galleryFilterError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrTagName):
		c.JSON(400, gin.H{"error": store.ErrTagName.Error(), "code": codeInvalidTagName})
	case errors.Is(err, store.ErrTagMode):
		c.JSON(400, gin.H{"error": store.ErrTagMode.Error(), "code": codeInvalidTagMode})
	case errors.Is(err, store.ErrGalleryCursor):
		c.JSON(400, gin.H{"error": store.ErrGalleryCursor.Error(), "code": codeInvalidCursor})
	case errors.Is(err, store.ErrPermission):
		c.JSON(403, gin.H{"error": "Image operation is not permitted"})
	default:
		c.JSON(500, gin.H{"error": "Gallery operation failed"})
	}
}

func galleryList(authentication *auth.Service, service *uploads.Service) gin.HandlerFunc {
	return requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		query, ok := galleryListQuery(c, galleryListKeys)
		if !ok {
			return
		}
		scope, ok := galleryScopeValue(c, session, query)
		if !ok {
			return
		}
		limit, limitGiven, limitValid := galleryIntValue(query, "limit")
		if !limitValid || (limitGiven && (limit < 1 || limit > 100)) {
			galleryQueryError(c, "Invalid gallery pagination", codeInvalidGalleryPagination)
			return
		}
		offset, offsetGiven, offsetValid := galleryIntValue(query, "offset")
		if !offsetValid || (offsetGiven && offset < 0) {
			galleryQueryError(c, "Invalid gallery pagination", codeInvalidGalleryPagination)
			return
		}
		if group := query.Get("group"); group != "" {
			if group != "day" {
				galleryQueryError(c, "Invalid gallery group", codeInvalidGalleryGroup)
				return
			}
			if limitGiven || offsetGiven || query.Has("before") {
				galleryQueryError(c, "Invalid gallery query", codeInvalidGalleryQuery)
				return
			}
			filter, ok := galleryFilterValues(c, session, query, scope)
			if !ok {
				return
			}
			buckets, err := service.DB.TimelineBuckets(c.Request.Context(), filter)
			if err != nil {
				galleryFilterError(c, err)
				return
			}
			c.JSON(200, gin.H{"groups": buckets})
			return
		}
		cursorMode := query.Has("before")
		for _, key := range []string{"tags", "tags_mode", "mode", "from", "to"} {
			cursorMode = cursorMode || query.Has(key)
		}
		if !cursorMode {
			if !limitGiven {
				limit = 20
			}
			if !offsetGiven {
				offset = 0
			}
			images, total, err := service.DB.ListImages(session.User.ID, scope == "all", limit, offset)
			if err != nil {
				galleryError(c, err)
				return
			}
			values := make([]galleryImage, 0, len(images))
			for _, image := range images {
				values = append(values, galleryValue(image, service))
			}
			c.JSON(200, gin.H{"images": values, "total": total, "limit": limit, "offset": offset})
			return
		}
		if offsetGiven {
			galleryQueryError(c, "Invalid gallery pagination", codeInvalidGalleryPagination)
			return
		}
		if !limitGiven {
			limit = store.DefaultGalleryLimit
		}
		filter, ok := galleryFilterValues(c, session, query, scope)
		if !ok {
			return
		}
		var before *store.GalleryCursor
		if values, present := query["before"]; present {
			cursor, err := store.DecodeGalleryCursor(values[0])
			if err != nil {
				galleryFilterError(c, err)
				return
			}
			before = &cursor
		}
		page, err := service.DB.GalleryImages(c.Request.Context(), filter, before, limit)
		if err != nil {
			galleryFilterError(c, err)
			return
		}
		values := make([]galleryImage, 0, len(page.Images))
		for _, image := range page.Images {
			values = append(values, galleryValue(image, service))
		}
		var next *string
		if page.Next != nil {
			encoded := store.EncodeGalleryCursor(*page.Next)
			next = &encoded
		}
		c.JSON(200, gin.H{"images": values, "total": page.Total, "limit": page.Limit, "next": next})
	})
}

func galleryTimeline(authentication *auth.Service, service *uploads.Service) gin.HandlerFunc {
	return requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		query, ok := galleryListQuery(c, galleryTimelineKeys)
		if !ok {
			return
		}
		scope, ok := galleryScopeValue(c, session, query)
		if !ok {
			return
		}
		filter, ok := galleryFilterValues(c, session, query, scope)
		if !ok {
			return
		}
		buckets, err := service.DB.TimelineBuckets(c.Request.Context(), filter)
		if err != nil {
			galleryFilterError(c, err)
			return
		}
		c.JSON(200, gin.H{"buckets": buckets})
	})
}
