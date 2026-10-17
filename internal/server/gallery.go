package server

import (
	"database/sql"
	"errors"
	"mime"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
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
	router.GET("/api/v1/images", requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		query := c.Request.URL.Query()
		for key, values := range query {
			if len(values) != 1 || (key != "limit" && key != "offset" && key != "scope") {
				c.JSON(400, gin.H{"error": "Invalid gallery query"})
				return
			}
		}
		limit, offset := 20, 0
		for key, target := range map[string]*int{"limit": &limit, "offset": &offset} {
			if values, ok := query[key]; ok {
				n, err := strconv.Atoi(values[0])
				if err != nil || n < 0 || strconv.Itoa(n) != values[0] || (key == "limit" && (n < 1 || n > 100)) {
					c.JSON(400, gin.H{"error": "Invalid gallery pagination"})
					return
				}
				*target = n
			}
		}
		scope := query.Get("scope")
		if scope != "" && scope != "own" && scope != "all" {
			c.JSON(400, gin.H{"error": "Invalid gallery scope"})
			return
		}
		if scope == "all" && !session.User.Admin {
			c.JSON(403, gin.H{"error": "Administrator access is required"})
			return
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
	}))
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
	router.GET("/api/v1/images/:id/original", requireSession(authentication, false, func(session auth.Session, c *gin.Context) {
		image, reference, file, err := service.OpenOriginal(c.Param("id"), session.User.ID, session.User.Admin)
		if err != nil {
			galleryError(c, err)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			galleryError(c, err)
			return
		}
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
		http.ServeContent(c.Writer, c.Request, name, info.ModTime(), file)
	}))
}
