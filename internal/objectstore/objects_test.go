package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/testutil"
	"sync"
	"time"
)

func remoteFixture(t *testing.T) (*Objects, *testutil.S3Fixture, *httptest.Server) {
	t.Helper()
	fixture, server := testutil.NewS3(t)
	c := config.S3{Endpoint: server.URL, Bucket: "images", Region: "test-region", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Addressing: "path", Prefix: "owned/"}
	database, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err = database.DB.Exec("INSERT INTO users(id,username,password,quota) VALUES(1,'fixture',x'00',1024)"); err != nil {
		t.Fatal(err)
	}
	remote := newRemote(c, database.Namespace(), server.Client())
	if err := remote.claim(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	return &Objects{remote: remote, keys: database, readers: map[string]int{}, pending: map[string]retiredObject{}}, fixture, server
}
func TestRemotePublicationStreamingAndOwnership(t *testing.T) {
	objects, fixture, _ := remoteFixture(t)
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "input")
	data := []byte("verified image payload")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash, size, err := objects.Publish(ctx, path)
	if err != nil || size != int64(len(data)) {
		t.Fatal(hash, size, err)
	}
	addReference(t, objects, hash, 1)
	if _, _, err = objects.Publish(ctx, path); err != nil {
		t.Fatal("Immutable dedupe did not verify existing bytes", err)
	}
	file, err := objects.Open(ctx, hash, size)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(got, data[3:]) {
		t.Fatal("Ranged stream changed bytes", err)
	}
	file.Close()
	fixture.Mu.Lock()
	if len(fixture.Ranges) != 1 || fixture.Ranges[0] != "bytes=3-" {
		t.Error("Remote stream did not use a byte range")
	}
	fixture.Mu.Unlock()
	staged := filepath.Join(directory, "staged")
	if err = objects.Stage(ctx, hash, size, staged); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(staged); !bytes.Equal(got, data) {
		t.Fatal("Worker input staging changed bytes")
	}
	fixture.Mu.Lock()
	fixture.Corrupt = physicalKey(t, objects, hash)
	fixture.Mu.Unlock()
	if _, _, err = objects.Publish(ctx, path); !errors.Is(err, ErrIntegrity) {
		t.Fatal("HEAD size/metadata was accepted without content integrity", err)
	}
	if err = objects.Stage(ctx, hash, size, filepath.Join(directory, "bad-stage")); !errors.Is(err, ErrIntegrity) {
		t.Fatal("Corrupt remote input reached worker staging", err)
	}
	if _, err = os.Stat(filepath.Join(directory, "bad-stage")); !os.IsNotExist(err) {
		t.Fatal("Failed staging left a partial input")
	}
	fixture.Mu.Lock()
	fixture.Corrupt = ""
	fixture.Denied = true
	fixture.Mu.Unlock()
	if _, err = objects.Open(ctx, hash, size); !errors.Is(err, ErrUnavailable) {
		t.Fatal("Permission failure was treated as object absence", err)
	}
	fixture.Mu.Lock()
	fixture.Denied = false
	fixture.Mu.Unlock()
	clearReferences(t, objects)
	if err = objects.Remove(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, err = objects.Open(ctx, hash, size); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Deleted object remained readable", err)
	}
}
func TestRemoteReconciliationPreservesForeignAndReferencedObjects(t *testing.T) {
	objects, fixture, _ := remoteFixture(t)
	ctx := context.Background()
	prefix := objects.remote.configuration.Prefix + "objects/"
	fixture.Mu.Lock()
	for n := 0; n < 5; n++ {
		fixture.Objects[prefix+fmt.Sprintf("%064x", n)] = testutil.S3Object{Data: []byte("value"), Owner: objects.remote.namespace}
	}
	foreign := prefix + strings.Repeat("f", 64)
	fixture.Objects[foreign] = testutil.S3Object{Data: []byte("foreign")}
	fixture.Objects["outside/"+strings.Repeat("e", 64)] = testutil.S3Object{Data: []byte("outside"), Owner: objects.remote.namespace}
	fixture.Mu.Unlock()
	for n := 0; n < 5; n++ {
		addReference(t, objects, fmt.Sprintf("%064x", n), n+1)
	}
	if err := objects.Reconcile(ctx, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	fixture.Mu.Lock()
	if len(fixture.Objects) != 8 {
		t.Error("Unknown/live references were removed")
	}
	fixture.Mu.Unlock()
	keep := fmt.Sprintf("%064x", 0)
	database := objects.keys.(*store.Store)
	if _, err := database.DB.Exec("DELETE FROM images WHERE hash!=?", keep); err != nil {
		t.Fatal(err)
	}
	if err := objects.Reconcile(ctx, func(hash string) bool { return hash == keep }); err != nil {
		t.Fatal(err)
	}
	fixture.Mu.Lock()
	defer fixture.Mu.Unlock()
	if _, ok := fixture.Objects[foreign]; !ok {
		t.Fatal("Foreign hash-shaped key was deleted")
	}
	if _, ok := fixture.Objects[prefix+keep]; !ok {
		t.Fatal("Referenced object was deleted")
	}
	if _, ok := fixture.Objects["outside/"+strings.Repeat("e", 64)]; !ok {
		t.Fatal("Reconciliation escaped namespace")
	}
	if len(fixture.Objects) != 4 || fixture.Lists < 4 {
		t.Fatal("Paginated reconciliation failed", len(fixture.Objects), fixture.Lists)
	}
}
func TestNamespaceClaimRefusesExistingForeignKeys(t *testing.T) {
	objects, fixture, server := remoteFixture(t)
	foreign := newRemote(objects.remote.configuration, strings.Repeat("b", 32), server.Client())
	if err := foreign.claim(context.Background(), true); !errors.Is(err, ErrNamespace) {
		t.Fatal("Another database adopted the namespace", err)
	}
	fixture.Mu.Lock()
	delete(fixture.Objects, objects.remote.marker())
	fixture.Objects["owned/unrelated"] = testutil.S3Object{Data: []byte("foreign")}
	fixture.Mu.Unlock()
	if err := foreign.claim(context.Background(), true); !errors.Is(err, ErrNamespace) {
		t.Fatal("Nonempty prefix was silently adopted", err)
	}
}
func TestLocalPublicationAndVerifiedStaging(t *testing.T) {
	ctx := context.Background()
	objects, err := New(ctx, filepath.Join(t.TempDir(), "objects"), config.Storage{Backend: "local"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "input")
	data := []byte("local verified image")
	os.WriteFile(input, data, 0600)
	hash, size, err := objects.Publish(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if hash != hex.EncodeToString(digest[:]) {
		t.Fatal("Content hash changed")
	}
	os.WriteFile(filepath.Join(objects.directory, hash), []byte("corrupt"), 0600)
	if _, _, err = objects.Publish(ctx, input); !errors.Is(err, ErrIntegrity) {
		t.Fatal("Existing corrupt object was overwritten", err)
	}
	if err = objects.Stage(ctx, hash, size, filepath.Join(t.TempDir(), "staged")); !errors.Is(err, ErrIntegrity) {
		t.Fatal("Local worker input did not verify bytes", err)
	}
}

func TestAmbiguousRemotePutIsVerifiedOnRetry(t *testing.T) {
	objects, fixture, _ := remoteFixture(t)
	input := filepath.Join(t.TempDir(), "input")
	data := []byte("uncertain published bytes")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.Mu.Lock()
	fixture.Uncertain = true
	fixture.Mu.Unlock()
	hash, size, err := objects.Publish(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	file, err := objects.Open(context.Background(), hash, size)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("Ambiguous PUT retry did not verify stored bytes", err)
	}
}

func addReference(t *testing.T, objects *Objects, hash string, n int) {
	t.Helper()
	database := objects.keys.(*store.Store)
	if _, err := database.DB.Exec("INSERT INTO images(id,user_id,state,policy,created_at,hash) VALUES(?,1,'completed','{}',0,?)", fmt.Sprintf("%032x", n), hash); err != nil {
		t.Fatal(err)
	}
}
func clearReferences(t *testing.T, objects *Objects) {
	t.Helper()
	if _, err := objects.keys.(*store.Store).DB.Exec("DELETE FROM images"); err != nil {
		t.Fatal(err)
	}
}
func physicalKey(t *testing.T, objects *Objects, hash string) string {
	t.Helper()
	generation, err := objects.keys.ResolveObjectKey(hash)
	if err != nil {
		t.Fatal(err)
	}
	return objects.remote.key(objectKey(hash, generation))
}

func TestOpenedRemoteReaderSurvivesDeletionAndRepublication(t *testing.T) {
	objects, fixture, _ := remoteFixture(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "input")
	body := []byte("selected remote representation")
	os.WriteFile(path, body, 0600)
	hash, size, err := objects.Publish(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := physicalKey(t, objects, hash)
	reader, err := objects.Open(ctx, hash, size)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err = objects.Remove(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, _, err = objects.Publish(ctx, path); err != nil {
		t.Fatal(err)
	}
	newKey := physicalKey(t, objects, hash)
	if oldKey == newKey {
		t.Fatal("Retired physical key was reused")
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("Already opened reader lost its selected bytes", err)
	}
	if _, err = reader.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, body[3:]) {
		t.Fatal("Opened multi-range reader changed generations", err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.Mu.Lock()
	_, oldExists := fixture.Objects[oldKey]
	_, newExists := fixture.Objects[newKey]
	fixture.Mu.Unlock()
	if oldExists || !newExists {
		t.Fatal("Deferred collection deleted a republished generation")
	}
}
func TestLateRemoteDeleteCannotRemoveRepublishedObject(t *testing.T) {
	objects, fixture, _ := remoteFixture(t)
	path := filepath.Join(t.TempDir(), "input")
	body := []byte("late deletion race")
	os.WriteFile(path, body, 0600)
	hash, size, err := objects.Publish(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := physicalKey(t, objects, hash)
	wait, started := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(wait) }) })
	fixture.Mu.Lock()
	fixture.DelayDelete = wait
	fixture.DeleteStarted = started
	fixture.Mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	removed := make(chan error, 1)
	go func() { removed <- objects.Remove(ctx, hash) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Delete did not reach the remote service")
	}
	cancel()
	select {
	case err = <-removed:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal("Uncertain delete was reported successful", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancelled delete did not return")
	}
	if _, _, err = objects.Publish(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	newKey := physicalKey(t, objects, hash)
	if newKey == oldKey {
		t.Fatal("Uncertain delete reused its physical target")
	}
	release.Do(func() { close(wait) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		fixture.Mu.Lock()
		_, exists := fixture.Objects[oldKey]
		fixture.Mu.Unlock()
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Late remote deletion did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	file, err := objects.Open(context.Background(), hash, size)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("Late old delete removed newly referenced bytes", err)
	}
}
func TestDatabaseFailureDoesNotAuthorizeRemoteCollection(t *testing.T) {
	objects, fixture, _ := remoteFixture(t)
	path := filepath.Join(t.TempDir(), "input")
	os.WriteFile(path, []byte("retained on unknown DB state"), 0600)
	hash, _, err := objects.Publish(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	key := physicalKey(t, objects, hash)
	objects.keys.(*store.Store).DB.Close()
	if err = objects.Reconcile(context.Background(), func(string) bool { return true }); !errors.Is(err, ErrUnavailable) {
		t.Fatal("Unknown DB state was accepted as unreferenced", err)
	}
	fixture.Mu.Lock()
	_, exists := fixture.Objects[key]
	fixture.Mu.Unlock()
	if !exists {
		t.Fatal("Unknown-reference object was deleted")
	}
}
