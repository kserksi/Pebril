package uploads

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
)

type blockedReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	source  io.Reader
}

func (r *blockedReader) Read(buffer []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.source.Read(buffer)
}
func TestShutdownTracksReceptionAndKeepsCompleteInput(t *testing.T) {
	deployment, err := config.LoadDeployment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := config.New(deployment, true)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	db, err := store.Open(deployment.DBDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota) VALUES('fixture',x'00',?)", store.DefaultQuota)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := result.LastInsertId()
	service, err := New(db, manager, deployment, "/bin/false", nativeprobe.Result{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec("UPDATE users SET used=quota WHERE id=?", user); err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.Receive(user, func(int64) (io.ReadCloser, error) {
		t.Fatal("Quota rejection read the upload request")
		return nil, nil
	}, nil); err == nil {
		t.Fatal("Upload was admitted with an exhausted quota")
	}
	if _, err = db.DB.Exec("UPDATE users SET used=0 WHERE id=?", user); err != nil {
		t.Fatal(err)
	}
	reader := &blockedReader{entered: make(chan struct{}), release: make(chan struct{}), source: strings.NewReader("received input")}
	received := make(chan error, 1)
	go func() {
		_, _, e := service.Receive(user, func(int64) (io.ReadCloser, error) { return io.NopCloser(reader), nil }, nil)
		received <- e
	}()
	<-reader.entered
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	deadline := time.Now().Add(time.Second)
	for {
		service.lifecycle.Lock()
		stopping := service.closing
		service.lifecycle.Unlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Shutdown did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if _, _, err = service.Receive(user, func(int64) (io.ReadCloser, error) {
		t.Fatal("Stopping service opened a new upload source")
		return nil, nil
	}, nil); err == nil {
		t.Fatal("New reception started during shutdown")
	}
	select {
	case <-closed:
		t.Fatal("Shutdown ignored an active receiver")
	default:
	}
	close(reader.release)
	if err = <-received; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not wait for task completion")
	}
	ids, err := db.ReceivedUploads()
	if err != nil || len(ids) != 1 {
		t.Fatal("Complete input was not retained for restart", ids, err)
	}
}
