package uploads

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/resources"
	"github.com/kserksi/Pebril/internal/store"
)

type uploadResourceState struct {
	mu     sync.Mutex
	state  resources.State
	polled chan struct{}
}

func (r *uploadResourceState) State() resources.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case r.polled <- struct{}{}:
	default:
	}
	return r.state
}

func (r *uploadResourceState) set(concurrency int, cooldown bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = resources.State{Concurrency: concurrency, Cooldown: cooldown}
}

func resourceUploadFixture(t *testing.T, cooldown bool) (*Service, *uploadResourceState, int64) {
	t.Helper()
	d, err := config.LoadDeployment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := config.New(d, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	resourceConfigureConcurrency(t, m, d, 2)
	db, err := store.Open(d.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota) VALUES('resource-fixture',x'00',?)", store.DefaultQuota)
	if err != nil {
		t.Fatal(err)
	}
	user, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	state := &uploadResourceState{state: resources.State{Concurrency: 2, Cooldown: cooldown}, polled: make(chan struct{}, 128)}
	s, err := NewWithResources(db, m, d, "/bin/false", nativeprobe.Result{}, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, state, user
}

func resourceConfigureConcurrency(t *testing.T, m *config.Manager, d config.Deployment, limit int) {
	t.Helper()
	policy := m.Snapshot().Policy
	policy.ConversionConcurrency = limit
	policy.AutomaticRetry = false
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Root, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
}

func TestResourceCooldownRejectsBeforeBodyAndRecord(t *testing.T) {
	s, _, user := resourceUploadFixture(t, true)
	if _, _, err := s.Receive(user, func(int64) (io.ReadCloser, error) {
		t.Error("cooldown opened the request body")
		return io.NopCloser(strings.NewReader("unused")), nil
	}); !errors.Is(err, ErrResourceCooldown) {
		t.Fatalf("cooldown rejection: %v", err)
	}
	var count int
	if err := s.DB.DB.QueryRow("SELECT count(*) FROM images").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cooldown created %d image records", count)
	}
	entries, err := os.ReadDir(s.incoming())
	if err != nil || len(entries) != 0 {
		t.Fatalf("cooldown created reception files: %v, %v", entries, err)
	}
}

func TestResourceCooldownPreservesAcceptedReception(t *testing.T) {
	s, resource, user := resourceUploadFixture(t, false)
	source := strings.NewReader("complete accepted transfer")
	reader := &blockedReader{entered: make(chan struct{}), release: make(chan struct{}), source: source}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(reader.release) }) })
	type result struct {
		image store.Image
		done  <-chan struct{}
		err   error
	}
	received := make(chan result, 1)
	go func() {
		image, done, err := s.Receive(user, func(int64) (io.ReadCloser, error) { return io.NopCloser(reader), nil })
		received <- result{image, done, err}
	}()
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted reception did not read its body")
	}
	resource.set(1, true)
	release.Do(func() { close(reader.release) })
	select {
	case got := <-received:
		if got.err != nil || got.image.ID == "" || source.Len() != 0 {
			t.Fatalf("cooldown interrupted accepted transfer: %+v, unread bytes %d", got, source.Len())
		}
		select {
		case <-got.done:
		case <-time.After(5 * time.Second):
			t.Fatal("accepted validation did not finish during cooldown")
		}
		// The validator double rejects the content; reception must still have completed.
		image, err := s.DB.Image(got.image.ID)
		if err != nil || image.State != "failed" {
			t.Fatalf("accepted transfer did not reach validation: %+v, %v", image, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cooldown blocked an accepted transfer")
	}
}

func resourceQueuedImage(t *testing.T, s *Service, user int64, order int64) string {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.AcceptUpload(id, user, s.Manager.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Received(id); err != nil {
		t.Fatal(err)
	}
	payload := []byte("controlled conversion input")
	digest := sha256.Sum256(payload)
	hash := hex.EncodeToString(digest[:])
	if err := os.WriteFile(filepath.Join(s.objectDir(), hash), payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.SaveUpload(store.Image{ID: id, UserID: user, State: "queued", Hash: hash, Bytes: int64(len(payload)), Format: "png", Width: 1, Height: 1, Frames: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.DB.Exec("UPDATE images SET created_at=? WHERE id=?", order, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func resourceWait(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func resourceSchedulerPoll(t *testing.T, s *Service, state *uploadResourceState) {
	t.Helper()
	for {
		select {
		case <-state.polled:
			continue
		default:
		}
		break
	}
	s.signalConversion()
	select {
	case <-state.polled:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not poll resource state")
	}
	// Scheduler reads resource state while holding this lock; wait for that pass to finish.
	s.queue.mu.Lock()
	s.queue.mu.Unlock()
}

func TestResourceCooldownRunningWorkManualCapAndQueuedRecovery(t *testing.T) {
	s, resource, user := resourceUploadFixture(t, true)
	if err := os.MkdirAll(s.Deployment.NativeDir, 0700); err != nil {
		t.Fatal(err)
	}
	worker := "#!/bin/sh\n: > \"$PWD/started\"\nwhile [ ! -f \"$PWD/release\" ]; do /bin/sleep 0.01; done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(s.Deployment.NativeDir, "pebril-image-worker"), []byte(worker), 0700); err != nil {
		t.Fatal(err)
	}
	first := resourceQueuedImage(t, s, user, 1)
	second := resourceQueuedImage(t, s, user, 2)
	resourceSchedulerPoll(t, s, resource)
	for _, id := range []string{first, second} {
		image, err := s.DB.Image(id)
		if err != nil || image.State != "queued" || image.Generation != 0 {
			t.Fatalf("cooldown dispatched queued work: %+v, %v", image, err)
		}
	}
	resource.set(2, false)
	s.signalConversion()
	started := func(id string) bool {
		_, err := os.Stat(filepath.Join(s.conversionsDir(), id, "started"))
		return err == nil
	}
	resourceWait(t, "two conversions did not start after recovery", func() bool { return started(first) && started(second) })
	resource.set(1, true)
	third := resourceQueuedImage(t, s, user, 3)
	for pass := 0; pass < 2; pass++ {
		resourceSchedulerPoll(t, s, resource)
	}
	for _, id := range []string{first, second} {
		image, err := s.DB.Image(id)
		if err != nil || image.State != "processing" {
			t.Fatalf("cooldown canceled running work: %+v, %v", image, err)
		}
	}
	if started(third) {
		t.Fatal("cooldown started a new queued conversion")
	}
	// A manual reduction still applies even though cooldown itself preserves running work.
	resourceConfigureConcurrency(t, s.Manager, s.Deployment, 1)
	s.signalConversion()
	resourceWait(t, "manual concurrency reduction did not requeue the excess worker", func() bool {
		image, err := s.DB.Image(second)
		return err == nil && image.State == "queued" && !started(second)
	})
	if err := os.WriteFile(filepath.Join(s.conversionsDir(), first, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	resourceWait(t, "running conversion could not finish during cooldown", func() bool {
		image, err := s.DB.Image(first)
		return err == nil && image.State == "failed"
	})
	resourceSchedulerPoll(t, s, resource)
	for _, id := range []string{second, third} {
		image, err := s.DB.Image(id)
		if err != nil || image.State != "queued" {
			t.Fatalf("cooldown consumed queued work: %+v, %v", image, err)
		}
	}
	resource.set(1, false)
	s.signalConversion()
	resourceWait(t, "requeued worker did not restart after recovery", func() bool {
		image, err := s.DB.Image(second)
		return err == nil && image.State == "processing" && image.Generation == 2 && started(second)
	})
	if err := os.WriteFile(filepath.Join(s.conversionsDir(), second, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	resourceWait(t, "queued conversion did not start after recovery", func() bool { return started(third) })
}

func TestRestrictedRestorationKeepsReceivedAVIF(t *testing.T) {
	old, monitor, user := resourceUploadFixture(t, false)
	old.Close()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	policy := old.Manager.Snapshot()
	policy.Policy.UploadFormats["avif"] = true
	policy.Policy.StaticFormat = "avif"
	if err = old.DB.AcceptUpload(id, user, policy); err != nil {
		t.Fatal(err)
	}
	if err = old.DB.Received(id); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(old.scratch(id), 0700); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 20)
	binary.BigEndian.PutUint32(payload, 20)
	copy(payload[4:], "ftypavif")
	input := filepath.Join(old.scratch(id), "input")
	if err = os.WriteFile(input, payload, 0600); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err = old.DB.DB.QueryRow("SELECT policy FROM images WHERE id=?", id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	deployment := old.Deployment
	deployment.Restricted = true
	service, err := NewWithResources(old.DB, old.Manager, deployment, "/missing-validation-worker", nativeprobe.Result{}, monitor)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	resourceWait(t, "Restored AVIF was not explicitly deferred", func() bool {
		image, e := service.Result(id, user)
		return e == nil && strings.Contains(image.Error, "normal startup")
	})
	image, err := service.DB.Image(id)
	if err != nil || image.State != "received" || image.Attempts != 0 {
		t.Fatal("Restoration rejected accepted AVIF", image, err)
	}
	after, err := os.ReadFile(input)
	if err != nil || !bytes.Equal(after, payload) {
		t.Fatal("Accepted input was lost", err)
	}
	var captured []byte
	if err = service.DB.DB.QueryRow("SELECT policy FROM images WHERE id=?", id).Scan(&captured); err != nil || !bytes.Equal(before, captured) {
		t.Fatal("Restoration rewrote accepted policy", err)
	}
}
