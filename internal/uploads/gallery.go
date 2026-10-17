package uploads

import (
	"os"
	"path/filepath"

	"github.com/kserksi/Pebril/internal/store"
)

// Delete invalidates publication before stopping work, then releases only unreferenced objects.
func (s *Service) Delete(id string, user int64, admin bool) error {
	if !validID(id) {
		return os.ErrNotExist
	}
	// Match scheduling's queue/lifecycle order and publication's objects/lifecycle order.
	s.queue.mu.Lock()
	s.objects.Lock()
	s.lifecycle.Lock()
	hashes, err := s.DB.DeleteImage(id, user, admin)
	var receiving, converting <-chan struct{}
	var upload *uploadJob
	if err == nil {
		if job := s.uploads[id]; job != nil {
			job.cancel()
			receiving, upload = job.done, job
		}
		if job := s.queue.jobs[id]; job != nil {
			job.stopping = true
			job.cancel()
			converting = job.done
		}
	}
	s.lifecycle.Unlock()
	s.objects.Unlock()
	s.queue.mu.Unlock()
	if err != nil {
		return err
	}
	if upload != nil {
		upload.reception.Lock()
		if upload.closeSource != nil {
			_ = upload.closeSource()
		}
		upload.reception.Unlock()
	}
	if receiving != nil {
		<-receiving
	}
	if converting != nil {
		<-converting
	}
	s.objects.Lock()
	defer s.objects.Unlock()
	for _, hash := range hashes {
		s.collectObject(hash)
	}
	if err = os.RemoveAll(s.scratch(id)); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.conversionsDir(), id))
}

func (s *Service) OpenOriginal(id string, user int64, admin bool) (store.Image, store.ImageFile, *os.File, error) {
	if !validID(id) {
		return store.Image{}, store.ImageFile{}, nil, os.ErrNotExist
	}
	s.objects.Lock()
	defer s.objects.Unlock()
	image, err := s.DB.GalleryImage(id, user, admin)
	if err != nil {
		return image, store.ImageFile{}, nil, err
	}
	if !s.Manager.Snapshot().Policy.AllowOriginalDownload {
		return image, store.ImageFile{}, nil, ErrOriginalDownloadDisabled
	}
	original := store.FileRole(image.Files, "original")
	if !validHash(original.Hash) {
		return image, original, nil, os.ErrNotExist
	}
	file, err := os.Open(filepath.Join(s.objectDir(), original.Hash))
	return image, original, file, err
}
