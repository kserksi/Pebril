package server

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"

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
		if image.State == "failed" {
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
func publicSVG(service *uploads.Service, c *gin.Context) {
	path, err := service.PublicSVG(c.Param("id"))
	if err != nil {
		c.Status(404)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		c.Status(404)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		c.Status(500)
		return
	}
	image, err := service.DB.Image(c.Param("id"))
	if err != nil {
		c.Status(404)
		return
	}
	policy := image.Policy.Policy
	c.Header("Cache-Control", cacheHeader(policy.PublicBrowserSeconds, policy.PublicCDNSeconds))
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	c.Header("Content-Type", "image/svg+xml")
	c.Header("ETag", `"`+image.Hash+`"`)
	http.ServeContent(c.Writer, c.Request, "image.svg", info.ModTime(), f)
}

func cacheHeader(browser, cdn int64) string {
	return fmt.Sprintf("public, max-age=%d, s-maxage=%d", browser, cdn)
}
