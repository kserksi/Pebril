package uploads

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/objectstore"
	"github.com/kserksi/Pebril/internal/resources"
	"github.com/kserksi/Pebril/internal/store"
)

var ErrTooLarge = errors.New("Upload exceeds configured byte limit")
var ErrEmpty = errors.New("Upload is empty")
var ErrOriginalDownloadDisabled = errors.New("Original download is disabled")
var ErrResourceCooldown = errors.New("Processing is cooling down; try uploading again after resources recover")

type ResourceState interface {
	State() resources.State
}

type uploadJob struct {
	ctx         context.Context
	cancel      context.CancelFunc
	reception   sync.Mutex
	closeSource func() error
	done        chan struct{}
}

type Service struct {
	files      *objectstore.Objects
	Resources  ResourceState
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
	uploads    map[string]*uploadJob
	queue      conversionQueue
}

func New(db *store.Store, m *config.Manager, d config.Deployment, executable string, capability nativeprobe.Result) (*Service, error) {
	return NewWithResources(db, m, d, executable, capability, nil)
}

func NewWithResources(db *store.Store, m *config.Manager, d config.Deployment, executable string, capability nativeprobe.Result, monitor ResourceState) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{Resources: monitor, DB: db, Manager: m, Deployment: d, Executable: executable, Capability: capability, ctx: ctx, cancel: cancel, semaphore: make(chan struct{}, m.Snapshot().Policy.ConversionConcurrency), uploads: map[string]*uploadJob{}, queue: conversionQueue{jobs: map[string]*conversionJob{}, wake: make(chan struct{}, 1)}}
	objects, err := objectstore.New(ctx, s.objectDir(), m.Storage(), db.Namespace())
	if err != nil {
		cancel()
		return nil, err
	}
	s.files = objects
	for _, dir := range []string{s.incoming()} {
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
	if err = s.files.Reconcile(ctx, db.ObjectReferenced); err != nil {
		cancel()
		return nil, err
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
			s.start(id, true)
		} else {
			db.FailUpload(id, "Task restoration is disabled")
			os.RemoveAll(s.scratch(id))
		}
	}
	if err = s.startScheduler(); err != nil {
		s.Close()
		return nil, err
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
func validHash(hash string) bool            { b, e := hex.DecodeString(hash); return e == nil && len(b) == 32 }
func NewID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}

// Receive keeps unverified bytes outside image storage and never trusts supplied filenames.
func (s *Service) Receive(user int64, openSource func(int64) (io.ReadCloser, error), cancelSource func() error) (store.Image, <-chan struct{}, error) {
	if err := s.StorageReady(s.ctx); err != nil {
		return store.Image{}, nil, objectstore.ErrUnavailable
	}
	s.lifecycle.Lock()
	if s.closing {
		s.lifecycle.Unlock()
		return store.Image{}, nil, errors.New("Upload service is stopping")
	}
	if s.Resources != nil && s.Resources.State().Cooldown {
		s.lifecycle.Unlock()
		return store.Image{}, nil, ErrResourceCooldown
	}
	s.workers.Add(1)
	s.lifecycle.Unlock()
	defer s.workers.Done()
	id, err := NewID()
	if err != nil {
		return store.Image{}, nil, err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	job := &uploadJob{ctx: ctx, cancel: cancel, closeSource: cancelSource, done: make(chan struct{})}
	s.lifecycle.Lock()
	s.uploads[id] = job
	s.lifecycle.Unlock()
	started := false
	defer func() {
		job.reception.Lock()
		job.closeSource = nil
		job.reception.Unlock()
		if !started {
			s.finishUpload(id, job)
		}
	}()
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
	job.reception.Lock()
	if cancelSource == nil {
		job.closeSource = source.Close
	}
	job.reception.Unlock()
	var sourceClosed sync.Once
	closeReception := func() {
		sourceClosed.Do(func() {
			_ = source.Close()
			job.reception.Lock()
			job.closeSource = nil
			job.reception.Unlock()
		})
	}
	defer closeReception()
	cancelled := job.ctx.Err()
	if cancelled != nil {
		return fail(cancelled)
	}
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
	closeReception()
	done := s.start(id, false)
	started = true
	image, err := s.DB.Image(id)
	return image, done, err
}
func (s *Service) start(id string, restored bool) <-chan struct{} {
	s.lifecycle.Lock()
	job := s.uploads[id]
	if job == nil {
		ctx, cancel := context.WithCancel(s.ctx)
		job = &uploadJob{ctx: ctx, cancel: cancel, done: make(chan struct{})}
		s.uploads[id] = job
	}
	s.workers.Add(1)
	s.lifecycle.Unlock()
	go func() {
		defer s.workers.Done()
		defer s.finishUpload(id, job)
		select {
		case s.semaphore <- struct{}{}:
			defer func() { <-s.semaphore }()
		case <-job.ctx.Done():
			return
		}
		if err := s.validate(job.ctx, id); err != nil {
			// A shutdown leaves fully received input durable for restart restoration.
			if job.ctx.Err() != nil {
				return
			}
			if restored && s.Deployment.Restricted && errors.Is(err, nativeprobe.ErrAVIFDisabled) {
				if _, e := s.DB.DB.Exec("UPDATE images SET error=? WHERE id=? AND state='received'", "AVIF processing is waiting for a normal startup", id); e != nil {
					slog.Error("Cannot persist deferred AVIF status", "image", id, "error", e)
				}
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
	return job.done
}
func (s *Service) finishUpload(id string, job *uploadJob) {
	s.lifecycle.Lock()
	delete(s.uploads, id)
	job.cancel()
	close(job.done)
	s.lifecycle.Unlock()
}
func (s *Service) validate(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	result, err := nativeprobe.Verify(ctx, s.Executable, s.Capability.VipsPath, filepath.Join(s.Deployment.NativeDir, "libpebril_native.so"), input, output, policyPath, !s.Deployment.Restricted)
	if err != nil {
		return err
	}
	if s.Deployment.Restricted && result.Format != "svg" && result.Frames <= 1 && image.Policy.Policy.StaticFormat == "avif" {
		return nativeprobe.ErrAVIFDisabled
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
	s.objects.Lock()
	defer s.objects.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	digest, count, err := s.files.Publish(ctx, output)
	if err != nil {
		return err
	}
	if count != info.Size() {
		return objectstore.ErrIntegrity
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
		s.collectObject(digest)
		return err
	}
	if err = os.RemoveAll(scratch); err != nil {
		slog.Warn("Upload temporary cleanup failed", "image", id, "error", err)
	}
	if image.State == "queued" {
		s.signalConversion()
	}
	return nil
}
func IsCapacity(err error) bool {
	return errors.Is(err, store.ErrCapacity) || errors.Is(err, store.ErrQuota) || errors.Is(err, ErrResourceCooldown)
}
func (s *Service) Result(id string, user int64) (store.Image, error) {
	image, err := s.DB.Image(id)
	if err == nil && image.UserID != user {
		return store.Image{}, fmt.Errorf("%w", os.ErrNotExist)
	}
	if err == nil && s.Deployment.Restricted && (image.State == "queued" || image.State == "retry_wait") && (image.Format == "avif" || image.Frames <= 1 && image.Policy.Policy.StaticFormat == "avif") {
		image.Error = "AVIF conversion is waiting for a normal startup"
	}
	return image, err
}

func (s *Service) StorageReady(ctx context.Context) error {
	if s.files == nil {
		return objectstore.ErrUnavailable
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.files.Check(probe)
}
