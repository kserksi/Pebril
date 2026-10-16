package uploads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kserksi/Pebril/internal/imageproc"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
)

type conversionJob struct {
	cancel   context.CancelFunc
	started  time.Time
	stopping bool
}
type conversionQueue struct {
	mu   sync.Mutex
	jobs map[string]*conversionJob
	wake chan struct{}
}

func (s *Service) conversionsDir() string { return filepath.Join(s.Deployment.DataDir, ".converting") }
func (s *Service) signalConversion() {
	select {
	case s.queue.wake <- struct{}{}:
	default:
	}
}
func (s *Service) startScheduler() error {
	if err := os.MkdirAll(s.conversionsDir(), 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.conversionsDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && validID(entry.Name()) {
			if err = os.RemoveAll(filepath.Join(s.conversionsDir(), entry.Name())); err != nil {
				return err
			}
		}
	}
	if err = s.DB.RestoreConversions(s.Manager.Snapshot().Policy.ResumeTasks); err != nil {
		return err
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var lastUser int64
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			case <-s.queue.wake:
			}
			s.queue.mu.Lock()
			limit := s.Manager.Snapshot().Policy.ConversionConcurrency
			cooldown := false
			if s.Resources != nil {
				state := s.Resources.State()
				cooldown = state.Cooldown
				if !cooldown {
					limit = min(limit, state.Concurrency)
				}
			}
			jobs := make([]*conversionJob, 0, len(s.queue.jobs))
			for _, job := range s.queue.jobs {
				if !job.stopping {
					jobs = append(jobs, job)
				}
			}
			sort.Slice(jobs, func(i, j int) bool { return jobs[i].started.After(jobs[j].started) })
			for len(jobs) > limit {
				job := jobs[0]
				jobs = jobs[1:]
				job.stopping = true
				s.lifecycle.Lock()
				job.cancel()
				s.lifecycle.Unlock()
			}
			for !cooldown && len(s.queue.jobs) < limit && s.ctx.Err() == nil {
				id, e := s.DB.NextConversion(lastUser, s.Deployment.Restricted)
				if e != nil {
					break
				}
				if _, busy := s.queue.jobs[id]; busy {
					break
				}
				image, e := s.DB.ClaimConversion(id)
				if e != nil {
					break
				}
				lastUser = image.UserID
				ctx, cancel := context.WithCancel(s.ctx)
				s.queue.jobs[id] = &conversionJob{cancel: cancel, started: time.Now()}
				s.workers.Add(1)
				go s.runConversion(ctx, cancel, image)
			}
			s.queue.mu.Unlock()
		}
	}()
	s.signalConversion()
	return nil
}
func (s *Service) runConversion(ctx context.Context, cancel context.CancelFunc, image store.Image) {
	defer s.workers.Done()
	defer cancel()
	defer func() {
		s.queue.mu.Lock()
		if ctx.Err() != nil {
			if err := s.DB.RequeueConversion(image.ID, image.Generation); err != nil {
				slog.Error("Cannot requeue interrupted conversion", "image", image.ID)
			}
		}
		delete(s.queue.jobs, image.ID)
		s.queue.mu.Unlock()
		s.signalConversion()
	}()
	err := s.convertImage(ctx, image)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.objects.Lock()
		defer s.objects.Unlock()
		s.lifecycle.Lock()
		if ctx.Err() != nil {
			s.lifecycle.Unlock()
			return
		}
		failure := s.DB.ConversionFailed(image, "Image conversion failed")
		s.lifecycle.Unlock()
		if failure != nil {
			slog.Error("Cannot persist conversion failure", "image", image.ID)
			return
		}
		for _, f := range image.Files {
			s.collectObject(f.Hash)
		}
	}
}

func (s *Service) collectObject(hash string) {
	if len(hash) == 64 && !s.DB.ObjectReferenced(hash) {
		if err := os.Remove(filepath.Join(s.objectDir(), hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("Cannot remove unreferenced image object")
		}
	}
}
func (s *Service) storeFile(path, role string, result nativeprobe.ImageResult) (store.ImageFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return store.ImageFile{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return store.ImageFile{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return store.ImageFile{}, errors.New("Invalid conversion output")
	}
	digest := sha256.New()
	if _, err = io.Copy(digest, f); err != nil {
		return store.ImageFile{}, err
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	target := filepath.Join(s.objectDir(), hash)
	stage, err := os.CreateTemp(s.objectDir(), ".object-")
	if err != nil {
		return store.ImageFile{}, err
	}
	defer os.Remove(stage.Name())
	if _, err = f.Seek(0, 0); err == nil {
		_, err = io.Copy(stage, f)
	}
	if err == nil {
		err = stage.Sync()
	}
	closed := stage.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return store.ImageFile{}, err
	}
	if err = os.Link(stage.Name(), target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return store.ImageFile{}, err
		}
		existing, e := os.Open(target)
		if e != nil {
			return store.ImageFile{}, e
		}
		check := sha256.New()
		_, e = io.Copy(check, existing)
		existing.Close()
		if e != nil {
			return store.ImageFile{}, e
		}
		if hex.EncodeToString(check.Sum(nil)) != hash {
			return store.ImageFile{}, errors.New("Stored image integrity verification failed")
		}
	}
	dir, err := os.Open(s.objectDir())
	if err != nil {
		return store.ImageFile{}, err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return store.ImageFile{}, err
	}
	return store.ImageFile{Role: role, Hash: hash, Bytes: info.Size(), Format: result.Format, Width: result.Width, Height: result.Height, Frames: result.Frames}, nil
}
func (s *Service) convertImage(ctx context.Context, image store.Image) error {
	original := store.FileRole(image.Files, "original")
	target := image.Policy.Policy.StaticFormat
	if original.Frames > 1 {
		target = image.Policy.Policy.AnimatedFormat
	}
	if (!s.Deployment.AVIF || s.Deployment.Restricted) && (original.Format == "avif" || target == "avif") {
		return errors.New("New AVIF processing is disabled")
	}
	if original.Hash == "" {
		return errors.New("Conversion input is missing")
	}
	dir := filepath.Join(s.conversionsDir(), image.ID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	request := imageproc.Request{Input: filepath.Join(s.objectDir(), original.Hash), Directory: dir, Rust: filepath.Join(s.Deployment.NativeDir, "libpebril_native.so"), SourceFormat: original.Format, Frames: original.Frames, Policy: image.Policy.Policy}
	result, err := imageproc.Convert(ctx, filepath.Join(s.Deployment.NativeDir, "pebril-image-worker"), s.Capability.VipsPath, request)
	if err != nil {
		return err
	}
	expected := image.Policy.Policy.StaticFormat
	if original.Frames > 1 {
		expected = image.Policy.Policy.AnimatedFormat
	}
	if result.Format != expected {
		return errors.New("Unexpected converted format")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	expectedCount := 1
	if result.Cover {
		expectedCount++
	}
	if len(entries) != expectedCount {
		return errors.New("Unexpected conversion outputs")
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || (entry.Name() != "result" && (!result.Cover || entry.Name() != "cover")) {
			return errors.New("Invalid conversion output entry")
		}
	}
	policy := image.Policy.Policy
	// Upload byte switches do not impose an unconfirmed limit on generated content.
	policy.StaticMaxBytes = math.MaxInt64
	policy.AnimatedMaxBytes = math.MaxInt64
	policy.UploadFormats = map[string]bool{"png": true, "jpeg": true, "webp": true, "gif": true, "avif": true}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	policyPath := filepath.Join(dir, "verify.json")
	if err = os.WriteFile(policyPath, data, 0600); err != nil {
		return err
	}
	verified, err := nativeprobe.Verify(ctx, s.Executable, s.Capability.VipsPath, request.Rust, filepath.Join(dir, "result"), filepath.Join(dir, "unused.svg"), policyPath, !s.Deployment.Restricted)
	if err != nil {
		return err
	}
	if verified.Format != expected || verified.Width != result.Width || verified.Height != result.Height || verified.Frames != original.Frames {
		return errors.New("Converted output does not match its verified dimensions")
	}
	var cover nativeprobe.ImageResult
	if result.Cover {
		// A cover inherits its accepted animation's pixel budget, not static-upload admission.
		policy.StaticMaxPixels = policy.AnimatedMaxPixels
		data, err = json.Marshal(policy)
		if err != nil {
			return err
		}
		if err = os.WriteFile(policyPath, data, 0600); err != nil {
			return err
		}
		cover, err = nativeprobe.Verify(ctx, s.Executable, s.Capability.VipsPath, request.Rust, filepath.Join(dir, "cover"), filepath.Join(dir, "unused.svg"), policyPath, !s.Deployment.Restricted)
		if err != nil {
			return err
		}
		if cover.Format != "webp" || cover.Frames != 1 {
			return errors.New("Invalid animation cover")
		}
	}
	s.objects.Lock()
	defer s.objects.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var files []store.ImageFile
	if image.Policy.Policy.KeepOriginal {
		files = append(files, original)
	}
	out, err := s.storeFile(filepath.Join(dir, "result"), "result", verified)
	if err != nil {
		return err
	}
	files = append(files, out)
	persisted := false
	defer func() {
		if !persisted {
			for _, f := range files {
				s.collectObject(f.Hash)
			}
		}
	}()
	if result.Cover {
		f, e := s.storeFile(filepath.Join(dir, "cover"), "cover", cover)
		if e != nil {
			return e
		}
		files = append(files, f)
	}
	image.Hash = out.Hash
	image.Format = out.Format
	image.Bytes = out.Bytes
	image.Width = out.Width
	image.Height = out.Height
	image.Frames = out.Frames
	s.lifecycle.Lock()
	if ctx.Err() != nil {
		s.lifecycle.Unlock()
		return ctx.Err()
	}
	err = s.DB.FinishConversion(image, files)
	s.lifecycle.Unlock()
	if err != nil {
		return err
	}
	persisted = true
	for _, f := range image.Files {
		s.collectObject(f.Hash)
	}
	return nil
}
func (s *Service) PublicFile(id string, cover bool) (store.Image, store.ImageFile, error) {
	if !validID(id) {
		return store.Image{}, store.ImageFile{}, os.ErrNotExist
	}
	image, err := s.DB.Image(id)
	if err != nil {
		return image, store.ImageFile{}, err
	}
	var status string
	if err = s.DB.DB.QueryRow("SELECT status FROM users WHERE id=?", image.UserID).Scan(&status); err != nil || status == "deleted" {
		return image, store.ImageFile{}, os.ErrNotExist
	}
	role := "result"
	if cover {
		role = "cover"
	} else if image.Temporary {
		role = "original"
	}
	f := store.FileRole(image.Files, role)
	if f.Hash == "" || (image.State != "completed" && !image.Temporary) || (cover && image.State != "completed") {
		return image, f, os.ErrNotExist
	}
	if len(f.Hash) != 64 || strings.ContainsAny(f.Hash, "/\\") {
		return image, f, os.ErrNotExist
	}
	return image, f, nil
}
func (s *Service) Retry(id string, user int64) error {
	if !validID(id) {
		return os.ErrNotExist
	}
	if err := s.DB.RetryConversion(id, user); err != nil {
		return err
	}
	s.signalConversion()
	return nil
}

func (s *Service) OpenPublic(id string, cover bool) (store.Image, store.ImageFile, *os.File, error) {
	s.objects.Lock()
	defer s.objects.Unlock()
	image, reference, err := s.PublicFile(id, cover)
	if err != nil {
		return image, reference, nil, err
	}
	file, err := os.Open(filepath.Join(s.objectDir(), reference.Hash))
	return image, reference, file, err
}
