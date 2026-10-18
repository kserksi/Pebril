package server

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

// An upload is accepted only after the complete multipart request has one file and no extra fields.
type singleFile struct {
	part     *multipart.Part
	reader   *multipart.Reader
	finished bool
}

func (s *singleFile) Read(p []byte) (int, error) {
	if s.finished {
		return 0, io.EOF
	}
	n, err := s.part.Read(p)
	if err == io.EOF {
		s.finished = true
		next, e := s.reader.NextPart()
		if e != io.EOF {
			if next != nil {
				next.Close()
			}
			return n, errors.New("Upload must contain exactly one file field")
		}
	}
	return n, err
}
func (s *singleFile) Close() error { return s.part.Close() }

func uploadHandler(service *uploads.Service, user store.User, c *gin.Context) {
	body := c.Request.Body
	var reception sync.Mutex
	active := true
	defer func() {
		reception.Lock()
		active = false
		reception.Unlock()
	}()
	image, done, err := service.Receive(user.ID, func(maximum int64) (io.ReadCloser, error) {
		if maximum > int64(^uint64(0)>>1)-65536 {
			return nil, errors.New("Upload byte limit is too large")
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maximum+65536)
		reader, err := c.Request.MultipartReader()
		if err != nil {
			return nil, errors.New("A multipart upload is required")
		}
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "file" || part.FileName() == "" {
			if part != nil {
				part.Close()
			}
			return nil, errors.New("A single file field named file is required")
		}
		return &singleFile{part: part, reader: reader}, nil
	}, func() error {
		reception.Lock()
		defer reception.Unlock()
		if !active {
			return nil
		}
		// Unblock reception without applying a deadline to a later keepalive request.
		controller := http.NewResponseController(c.Writer)
		_ = controller.SetReadDeadline(time.Now())
		defer controller.SetReadDeadline(time.Time{})
		return body.Close()
	})
	if err != nil {
		status := 400
		if uploads.IsCapacity(err) {
			status = 429
		}
		if errors.Is(err, store.ErrPermission) {
			status = 403
		}
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			status = 413
		}
		message := "Upload reception failed"
		if uploads.IsCapacity(err) || errors.Is(err, store.ErrPermission) || errors.Is(err, uploads.ErrTooLarge) || errors.Is(err, uploads.ErrEmpty) {
			message = err.Error()
		}
		if errors.Is(err, uploads.ErrTooLarge) {
			status = 413
		}
		code := ""
		switch {
		case errors.Is(err, uploads.ErrResourceCooldown):
			code = "resource_cooldown"
		case errors.Is(err, store.ErrCapacity):
			code = "upload_capacity"
		case errors.Is(err, store.ErrQuota):
			code = "storage_quota"
		}
		if code != "" {
			c.JSON(status, gin.H{"error": message, "code": code})
			return
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	select {
	case <-done:
		image, err = service.Result(image.ID, user.ID)
		if err != nil {
			c.JSON(500, gin.H{"error": "Cannot read upload result"})
			return
		}
		if image.State == "failed" && image.Format == "" {
			c.JSON(422, gin.H{"error": image.Error, "id": image.ID})
			return
		}
		if image.State == "completed" {
			c.JSON(201, image)
			return
		}
		c.JSON(202, image)
	case <-c.Request.Context().Done():
		// Receipt and safety verification survive a disconnected browser.
		return
	}
}
func publicImage(service *uploads.Service, cover bool, c *gin.Context) {
	image, reference, file, err := service.OpenPublic(c.Param("id"), cover)
	if err != nil {
		c.Status(404)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		c.Status(500)
		return
	}
	policy := image.Policy.Policy
	if image.Temporary {
		c.Header("Cache-Control", "no-store")
		ttl := fmt.Sprintf("public, max-age=%d", policy.FallbackCDNSeconds)
		c.Header("CDN-Cache-Control", ttl)
		c.Header("Cloudflare-CDN-Cache-Control", ttl)
	} else {
		c.Header("Cache-Control", cacheHeader(policy.PublicBrowserSeconds, policy.PublicCDNSeconds))
	}
	types := map[string]string{"svg": "image/svg+xml", "png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp", "gif": "image/gif", "avif": "image/avif"}
	kind, ok := types[reference.Format]
	if !ok {
		c.Status(500)
		return
	}
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	c.Header("Content-Type", kind)
	c.Header("ETag", `"`+reference.Hash+`"`)
	http.ServeContent(c.Writer, c.Request, "image", info.ModTime(), file)
}

func cacheHeader(browser, cdn int64) string {
	return fmt.Sprintf("public, max-age=%d, s-maxage=%d", browser, cdn)
}
