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
	"github.com/kserksi/Pebril/internal/testutil"
)

func remoteFixture(t *testing.T) (*Objects, *testutil.S3Fixture, *httptest.Server) {
	t.Helper()
	fixture, server := testutil.NewS3(t)
	c := config.S3{Endpoint: server.URL, Bucket: "images", Region: "test-region", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Addressing: "path", Prefix: "owned/"}
	remote := newRemote(c, strings.Repeat("a", 32), server.Client())
	if err := remote.claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Objects{remote: remote}, fixture, server
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
	fixture.Corrupt = objects.remote.key(hash)
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
	if err := objects.Reconcile(ctx, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	fixture.Mu.Lock()
	if len(fixture.Objects) != 8 {
		t.Error("Unknown/live references were removed")
	}
	fixture.Mu.Unlock()
	keep := fmt.Sprintf("%064x", 0)
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
	if err := foreign.claim(context.Background()); !errors.Is(err, ErrNamespace) {
		t.Fatal("Another database adopted the namespace", err)
	}
	fixture.Mu.Lock()
	delete(fixture.Objects, objects.remote.marker())
	fixture.Objects["owned/unrelated"] = testutil.S3Object{Data: []byte("foreign")}
	fixture.Mu.Unlock()
	if err := foreign.claim(context.Background()); !errors.Is(err, ErrNamespace) {
		t.Fatal("Nonempty prefix was silently adopted", err)
	}
}
func TestLocalPublicationAndVerifiedStaging(t *testing.T) {
	ctx := context.Background()
	objects, err := New(ctx, filepath.Join(t.TempDir(), "objects"), config.Storage{Backend: "local"}, "")
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
