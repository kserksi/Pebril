package uploads

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
)

var ErrTooLarge = errors.New("Upload exceeds configured byte limit")
var ErrEmpty = errors.New("Upload is empty")

type Service struct {
	DB         *store.Store
	Manager    *config.Manager
	Deployment config.Deployment
	Executable string
	Capability nativeprobe.Result
	ctx        context.Context
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	semaphore  chan struct{}
	objects    sync.Mutex
	lifecycle  sync.Mutex
	closing    bool
}

func New(db *store.Store, m *config.Manager, d config.Deployment, executable string, capability nativeprobe.Result) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{DB: db, Manager: m, Deployment: d, Executable: executable, Capability: capability, ctx: ctx, cancel: cancel, semaphore: make(chan struct{}, m.Snapshot().Policy.ConversionConcurrency)}
	for _, dir := range []string{s.incoming(), s.objectDir()} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			cancel()
			return nil, err
		}
	}
	// Only incomplete task-owned transfers are discarded on startup.
	rows, err := db.DB.Query("SELECT id FROM images WHERE state='receiving'")
	if err != nil {
		cancel()
		return nil, err
	}
	var partial []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			cancel()
			return nil, err
		}
		partial = append(partial, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		cancel()
		return nil, err
	}
	for _, id := range partial {
		if !validID(id) {
			cancel()
			return nil, errors.New("Invalid persisted upload identifier")
		}
		if err = db.FailUpload(id, "Transfer interrupted before complete reception"); err != nil {
			cancel()
			return nil, err
		}
		os.RemoveAll(s.scratch(id))
	}
	// Reconcile task-owned files left after a crash between filesystem and database publication.
	entries, err := os.ReadDir(s.incoming())
	if err != nil {
		cancel()
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		image, e := db.Image(entry.Name())
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			cancel()
			return nil, e
		}
		if errors.Is(e, sql.ErrNoRows) || image.State != "received" {
			if e = os.RemoveAll(s.scratch(entry.Name())); e != nil {
				cancel()
				return nil, e
			}
		}
	}
	entries, err = os.ReadDir(s.objectDir())
	if err != nil {
		cancel()
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		decoded, e := hex.DecodeString(name)
		owned := e == nil && len(decoded) == 32
		if owned && !db.ObjectReferenced(name) || strings.HasPrefix(name, ".object-") {
			if e = os.Remove(filepath.Join(s.objectDir(), name)); e != nil {
				cancel()
				return nil, e
			}
		}
	}
	ids, err := db.ReceivedUploads()
	if err != nil {
		cancel()
		return nil, err
	}
	for _, id := range ids {
		if !validID(id) {
			cancel()
			return nil, errors.New("Invalid persisted upload identifier")
		}
	}
	for _, id := range ids {
		if m.Snapshot().Policy.ResumeTasks {
			s.start(id)
		} else {
			db.FailUpload(id, "Task restoration is disabled")
			os.RemoveAll(s.scratch(id))
		}
	}
	return s, nil
}
func (s *Service) Close() {
	s.lifecycle.Lock()
	s.closing = true
	s.cancel()
	s.lifecycle.Unlock()
	s.workers.Wait()
}
func (s *Service) incoming() string         { return filepath.Join(s.Deployment.DataDir, ".incoming") }
func (s *Service) objectDir() string        { return filepath.Join(s.Deployment.PublicDir, ".objects") }
func (s *Service) scratch(id string) string { return filepath.Join(s.incoming(), id) }
func validID(id string) bool                { b, e := hex.DecodeString(id); return e == nil && len(b) == 16 }
func NewID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}

// Receive keeps unverified bytes outside image storage and never trusts supplied filenames.
func (s *Service) Receive(user int64, openSource func(int64) (io.ReadCloser, error)) (store.Image, <-chan struct{}, error) {
	s.lifecycle.Lock()
	if s.closing {
		s.lifecycle.Unlock()
		return store.Image{}, nil, errors.New("Upload service is stopping")
	}
	s.workers.Add(1)
	s.lifecycle.Unlock()
	defer s.workers.Done()
	id, err := NewID()
	if err != nil {
		return store.Image{}, nil, err
	}
	policy := s.Manager.Snapshot()
	if err = s.DB.AcceptUpload(id, user, policy); err != nil {
		return store.Image{}, nil, err
	}
	fail := func(e error) (store.Image, <-chan struct{}, error) {
		s.DB.FailUpload(id, "Upload reception failed")
		os.RemoveAll(s.scratch(id))
		return store.Image{}, nil, e
	}
	maximum := max(policy.Policy.StaticMaxBytes, policy.Policy.AnimatedMaxBytes)
	source, err := openSource(maximum)
	if err != nil {
		return fail(err)
	}
	defer source.Close()
	if err = os.Mkdir(s.scratch(id), 0700); err != nil {
		return fail(err)
	}
	file, err := os.OpenFile(filepath.Join(s.scratch(id), "input"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(err)
	}
	copied, err := io.Copy(file, io.LimitReader(source, maximum+1))
	if err == nil && copied > maximum {
		err = ErrTooLarge
	}
	if err == nil && copied == 0 {
		err = ErrEmpty
	}
	if err == nil {
		err = file.Sync()
	}
	closed := file.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return fail(err)
	}
	for _, dir := range []string{s.scratch(id), s.incoming()} {
		parent, e := os.Open(dir)
		if e != nil {
			return fail(e)
		}
		e = parent.Sync()
		parent.Close()
		if e != nil {
			return fail(e)
		}
	}
	if err = s.DB.Received(id); err != nil {
		return fail(err)
	}
	done := s.start(id)
	image, err := s.DB.Image(id)
	return image, done, err
}
func (s *Service) start(id string) <-chan struct{} {
	done := make(chan struct{})
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer close(done)
		select {
		case s.semaphore <- struct{}{}:
			defer func() { <-s.semaphore }()
		case <-s.ctx.Done():
			return
		}
		if err := s.validate(id); err != nil {
			// A shutdown leaves fully received input durable for restart restoration.
			if s.ctx.Err() != nil {
				return
			}
			message := "Image validation or storage failed"
			var pathError *os.PathError
			var linkError *os.LinkError
			if !errors.As(err, &pathError) && !errors.As(err, &linkError) {
				message = err.Error()
			}
			if e := s.DB.FailUpload(id, message); e != nil {
				slog.Error("Cannot persist upload rejection", "image", id, "error", e)
				return
			}
			os.RemoveAll(s.scratch(id))
		}
	}()
	return done
}
func (s *Service) validate(id string) error {
	image, err := s.DB.Image(id)
	if err != nil {
		return err
	}
	scratch := s.scratch(id)
	input := filepath.Join(scratch, "input")
	output := filepath.Join(scratch, "sanitized.svg")
	policyPath := filepath.Join(scratch, "policy.json")
	data, err := json.Marshal(image.Policy.Policy)
	if err != nil {
		return err
	}
	if err = os.WriteFile(policyPath, data, 0600); err != nil {
		return err
	}
	result, err := nativeprobe.Verify(s.ctx, s.Executable, s.Capability.VipsPath, filepath.Join(s.Deployment.NativeDir, "libpebril_native.so"), input, output, policyPath)
	if err != nil {
		return err
	}
	if result.Format != "svg" {
		output = input
	}
	file, err := os.Open(output)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return errors.New("Validated image is empty or invalid")
	}
	// Sanitization may expand XML entities, so output is bounded independently.
	limit := image.Policy.Policy.StaticMaxBytes
	if result.Frames > 1 {
		limit = image.Policy.Policy.AnimatedMaxBytes
	}
	if info.Size() > limit {
		return errors.New("Validated image exceeds configured byte limit")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if _, err = file.Seek(0, 0); err != nil {
		return err
	}
	s.objects.Lock()
	defer s.objects.Unlock()
	target := filepath.Join(s.objectDir(), digest)
	staged, err := os.CreateTemp(s.objectDir(), ".object-")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	_, err = io.Copy(staged, file)
	if err == nil {
		err = staged.Sync()
	}
	closed := staged.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	// Hard-link publishes atomically without replacing another record's existing object.
	if err = os.Link(staged.Name(), target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, e := os.Open(target)
		if e != nil {
			return e
		}
		check := sha256.New()
		_, e = io.Copy(check, existing)
		existing.Close()
		if e != nil {
			return e
		}
		if hex.EncodeToString(check.Sum(nil)) != digest {
			return errors.New("Stored image object failed integrity verification")
		}
	}
	dir, err := os.Open(s.objectDir())
	if err != nil {
		return err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return err
	}
	image.Format = result.Format
	image.Width = result.Width
	image.Height = result.Height
	image.Frames = result.Frames
	image.Bytes = info.Size()
	image.Hash = digest
	image.State = "queued"
	if result.Format == "svg" {
		image.State = "completed"
	}
	if err = s.DB.SaveUpload(image); err != nil {
		if !s.DB.ObjectReferenced(digest) {
			os.Remove(target)
		}
		return err
	}
	if err = os.RemoveAll(scratch); err != nil {
		slog.Warn("Upload temporary cleanup failed", "image", id, "error", err)
	}
	return nil
}
func (s *Service) PublicSVG(id string) (string, error) {
	if !validID(id) {
		return "", os.ErrNotExist
	}
	image, err := s.DB.Image(id)
	if err != nil {
		return "", err
	}
	if image.State != "completed" || image.Format != "svg" || len(image.Hash) != 64 {
		return "", os.ErrNotExist
	}
	var status string
	if err = s.DB.DB.QueryRow("SELECT status FROM users WHERE id=?", image.UserID).Scan(&status); err != nil || status == "deleted" {
		return "", os.ErrNotExist
	}
	return filepath.Join(s.objectDir(), image.Hash), nil
}
func IsCapacity(err error) bool {
	return errors.Is(err, store.ErrCapacity) || errors.Is(err, store.ErrQuota)
}
func (s *Service) Result(id string, user int64) (store.Image, error) {
	image, err := s.DB.Image(id)
	if err == nil && image.UserID != user {
		return store.Image{}, fmt.Errorf("%w", os.ErrNotExist)
	}
	return image, err
}
