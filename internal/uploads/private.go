package uploads

import (
	"context"
	"os"

	"github.com/kserksi/Pebril/internal/objectstore"
	"github.com/kserksi/Pebril/internal/store"
)

// PrivateImage returns a record reachable through the private landing page.
// The record must exist, be private and belong to an active account; otherwise
// it is indistinguishable from a missing record.
func (s *Service) PrivateImage(id string) (store.Image, error) {
	if !validID(id) {
		return store.Image{}, os.ErrNotExist
	}
	image, err := s.DB.Image(id)
	if err != nil {
		return image, err
	}
	if image.Visibility != store.VisibilityPrivate {
		return image, os.ErrNotExist
	}
	var status string
	if err = s.DB.DB.QueryRow("SELECT status FROM users WHERE id=?", image.UserID).Scan(&status); err != nil || status != "active" {
		return image, os.ErrNotExist
	}
	return image, nil
}

// PrivateFile resolves one content role for a private record. The converted
// result falls back to the original when it is missing, matching the frozen
// contract for private pages.
func (s *Service) PrivateFile(id, role string) (store.Image, store.ImageFile, error) {
	image, err := s.PrivateImage(id)
	if err != nil {
		return image, store.ImageFile{}, err
	}
	var file store.ImageFile
	switch role {
	case "cover":
		if image.State != "completed" {
			return image, file, os.ErrNotExist
		}
		file = store.FileRole(image.Files, "cover")
	case "original":
		if !image.Policy.Policy.AllowOriginalDownload {
			return image, file, os.ErrNotExist
		}
		file = store.FileRole(image.Files, "original")
	case "result":
		file = store.FileRole(image.Files, "result")
		if file.Hash == "" || image.State != "completed" {
			file = store.FileRole(image.Files, "original")
		}
	default:
		return image, file, os.ErrNotExist
	}
	if !validHash(file.Hash) {
		return image, file, os.ErrNotExist
	}
	return image, file, nil
}

// OpenPrivate opens the stored bytes of one authorized private-page content
// role. Authorization is a caller responsibility; this method only resolves the
// record, role and bytes under the same object lock as public delivery.
func (s *Service) OpenPrivate(ctx context.Context, id, role string) (store.Image, store.ImageFile, objectstore.File, error) {
	s.objects.Lock()
	defer s.objects.Unlock()
	image, reference, err := s.PrivateFile(id, role)
	if err != nil {
		return image, reference, nil, err
	}
	file, err := s.files.Open(ctx, reference.Hash, reference.Bytes)
	return image, reference, file, err
}
