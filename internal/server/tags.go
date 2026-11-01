package server

import (
	"database/sql"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

// Stable error codes from the frozen 0.0.11 tag contract. Messages stay
// human-readable and may change without breaking API clients.
const (
	codeInvalidTagName  = "invalid_tag_name"
	codeTagLimitReached = "tag_limit_reached"
	codeTagNotFound     = "tag_not_found"
	codeImageNotFound   = "image_not_found"
)

// tagError maps tag and image-tag failures without echoing SQL details.
func tagError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrTagName):
		c.JSON(400, gin.H{"error": store.ErrTagName.Error(), "code": codeInvalidTagName})
	case errors.Is(err, store.ErrTagNotFound):
		c.JSON(404, gin.H{"error": store.ErrTagNotFound.Error(), "code": codeTagNotFound})
	case errors.Is(err, store.ErrTagLimit):
		c.JSON(409, gin.H{"error": store.ErrTagLimit.Error(), "code": codeTagLimitReached})
	case errors.Is(err, store.ErrImageTagLimit):
		c.JSON(409, gin.H{"error": store.ErrImageTagLimit.Error(), "code": codeTagLimitReached})
	case errors.Is(err, store.ErrImageNotFound), errors.Is(err, sql.ErrNoRows):
		c.JSON(404, gin.H{"error": "Image not found", "code": codeImageNotFound})
	case errors.Is(err, store.ErrPermission):
		c.JSON(403, gin.H{"error": "Image operation is not permitted"})
	default:
		c.JSON(500, gin.H{"error": "Tag operation failed"})
	}
}

// tagValue is the API projection of a library tag. Internal identifiers and
// ownership never leave the store.
type tagValue struct {
	Name      string `json:"name"`
	Count     int    `json:"count"`
	CreatedAt int64  `json:"created_at"`
}

func tagValues(tags []store.Tag) []tagValue {
	values := make([]tagValue, 0, len(tags))
	for _, tag := range tags {
		values = append(values, tagValue{Name: tag.Name, Count: tag.Count, CreatedAt: tag.CreatedAt})
	}
	return values
}

// imageTagNames is the API projection of one image's tags. Names are returned
// in library order and no tag id or owner id is exposed.
func imageTagNames(tags []store.ImageTag) []string {
	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		names = append(names, tag.Name)
	}
	return names
}

// imageTagBody parses the strict {"tags":[...]} request body.
func imageTagBody(c *gin.Context) ([]string, bool) {
	var names []string
	if !accountBody(c, map[string]any{"tags": &names}, true) {
		c.JSON(400, gin.H{"error": "Invalid tag request"})
		return nil, false
	}
	return names, true
}

// tagsRoutes wires the tag library and per-image tag endpoints. Every mutation
// is owner-scoped; administrators act on the image owner's library (D15), which
// the store resolves. Reads never include another user's library.
func tagsRoutes(router *gin.Engine, authentication *auth.Service, service *uploads.Service) {
	router.GET("/api/v1/tags", requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		tags, err := service.DB.ListTags(c.Request.Context(), session.User.ID)
		if err != nil {
			tagError(c, err)
			return
		}
		c.JSON(200, tagValues(tags))
	}))
	router.POST("/api/v1/tags", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		var name string
		if !accountBody(c, map[string]any{"name": &name}, true) {
			c.JSON(400, gin.H{"error": "Invalid tag request"})
			return
		}
		tag, err := service.DB.EnsureTag(c.Request.Context(), session.User.ID, name)
		if err != nil {
			tagError(c, err)
			return
		}
		c.JSON(200, gin.H{"name": tag.Name, "count": tag.Count})
	}))
	router.DELETE("/api/v1/tags/:name", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		if err := service.DB.DeleteTag(c.Request.Context(), session.User.ID, c.Param("name")); err != nil {
			tagError(c, err)
			return
		}
		c.Status(204)
	}))
	router.GET("/api/v1/images/:id/tags", requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		tags, err := service.DB.ImageTags(c.Request.Context(), c.Param("id"), session.User.ID, session.User.Admin)
		if err != nil {
			tagError(c, err)
			return
		}
		c.JSON(200, gin.H{"tags": imageTagNames(tags)})
	}))
	router.PUT("/api/v1/images/:id/tags", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		names, ok := imageTagBody(c)
		if !ok {
			return
		}
		tags, err := service.DB.ReplaceImageTags(c.Request.Context(), c.Param("id"), session.User.ID, session.User.Admin, names)
		if err != nil {
			tagError(c, err)
			return
		}
		c.JSON(200, gin.H{"tags": imageTagNames(tags)})
	}))
	router.POST("/api/v1/images/:id/tags", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		names, ok := imageTagBody(c)
		if !ok {
			return
		}
		tags, err := service.DB.AddImageTags(c.Request.Context(), c.Param("id"), session.User.ID, session.User.Admin, names)
		if err != nil {
			tagError(c, err)
			return
		}
		c.JSON(200, gin.H{"tags": imageTagNames(tags)})
	}))
	router.DELETE("/api/v1/images/:id/tags/:name", requireSession(authentication, true, func(session auth.Session, c *gin.Context) {
		tags, err := service.DB.RemoveImageTag(c.Request.Context(), c.Param("id"), session.User.ID, session.User.Admin, c.Param("name"))
		if err != nil {
			tagError(c, err)
			return
		}
		c.JSON(200, gin.H{"tags": imageTagNames(tags)})
	}))
}
