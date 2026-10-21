package uploads

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
)

func TestOriginalDownloadAndSharedDeletion(t *testing.T) {
	s, _, owner := resourceUploadFixture(t, true)
	first := resourceQueuedImage(t, s, owner, 1)
	second := resourceQueuedImage(t, s, owner, 2)
	image, err := s.DB.ClaimConversion(first)
	if err != nil {
		t.Fatal(err)
	}
	original := store.FileRole(image.Files, "original")
	result := original
	result.Role = "result"
	coverHash := strings.Repeat("a", 64)
	if err = os.WriteFile(filepath.Join(s.objectDir(), coverHash), []byte("unique cover"), 0600); err != nil {
		t.Fatal(err)
	}
	cover := store.ImageFile{Role: "cover", Hash: coverHash, Bytes: 12, Format: "webp", Width: 1, Height: 1, Frames: 1}
	if err = s.DB.FinishConversion(image, []store.ImageFile{original, result, cover}); err != nil {
		t.Fatal(err)
	}
	strangerResult, err := s.DB.DB.Exec("INSERT INTO users(username,password,quota) VALUES('stranger',x'00',?)", store.DefaultQuota)
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := strangerResult.LastInsertId()
	adminResult, err := s.DB.DB.Exec("INSERT INTO users(username,password,quota,admin) VALUES('admin',x'00',?,1)", store.DefaultQuota)
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := adminResult.LastInsertId()
	if _, _, file, err := s.OpenOriginal(context.Background(), first, stranger, false); !errors.Is(err, sql.ErrNoRows) || file != nil {
		t.Fatalf("non-owner original: %v", err)
	}
	if err = s.Delete(first, stranger, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("forged administrator deletion: %v", err)
	}
	_, _, opened, err := s.OpenOriginal(context.Background(), first, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	_, _, adminFile, err := s.OpenOriginal(context.Background(), first, admin, true)
	if err != nil {
		t.Fatal(err)
	}
	adminFile.Close()
	policy := s.Manager.Snapshot().Policy
	policy.AllowOriginalDownload = false
	data, _ := json.Marshal(policy)
	if err = os.WriteFile(filepath.Join(s.Deployment.Root, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.Manager.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, _, file, err := s.OpenOriginal(context.Background(), first, owner, false); !errors.Is(err, ErrOriginalDownloadDisabled) || file != nil {
		t.Fatalf("live original-download switch: %v", err)
	}
	if err = s.Delete(first, admin, true); err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(opened)
	if err != nil || !bytes.Equal(payload, []byte("controlled conversion input")) {
		t.Fatalf("deletion invalidated an open download: %q, %v", payload, err)
	}
	if _, _, file, err := s.OpenPublic(context.Background(), first, false); err == nil || file != nil {
		t.Fatal("deleted public record still opens")
	}
	if _, err = os.Stat(filepath.Join(s.objectDir(), coverHash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced cover remains: %v", err)
	}
	if _, err = os.Stat(filepath.Join(s.objectDir(), original.Hash)); err != nil {
		t.Fatal("shared original was removed", err)
	}
	if err = s.Delete(second, owner, false); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.objectDir(), original.Hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("last reference did not remove object: %v", err)
	}
	account, err := s.DB.User(owner)
	if err != nil || account.Used != 0 {
		t.Fatalf("record charges not released: %+v, %v", account, err)
	}
}

func TestOriginalDownloadRequiresRetainedFile(t *testing.T) {
	s, _, owner := resourceUploadFixture(t, true)
	id := resourceQueuedImage(t, s, owner, 1)
	image, err := s.DB.ClaimConversion(id)
	if err != nil {
		t.Fatal(err)
	}
	original := store.FileRole(image.Files, "original")
	if err = os.Remove(filepath.Join(s.objectDir(), original.Hash)); err != nil {
		t.Fatal(err)
	}
	if _, _, file, err := s.OpenOriginal(context.Background(), id, owner, false); !errors.Is(err, os.ErrNotExist) || file != nil {
		t.Fatalf("missing physical original: %v", err)
	}
	result := original
	result.Role = "result"
	if err = s.DB.FinishConversion(image, []store.ImageFile{result}); err != nil {
		t.Fatal(err)
	}
	if _, _, file, err := s.OpenOriginal(context.Background(), id, owner, false); !errors.Is(err, os.ErrNotExist) || file != nil {
		t.Fatalf("result-only image exposed an original: %v", err)
	}
}

func TestDeleteCancelsReceptionAndMultipartOpening(t *testing.T) {
	for _, opening := range []bool{false, true} {
		t.Run(strconv.FormatBool(opening), func(t *testing.T) {
			s, _, owner := resourceUploadFixture(t, false)
			reader, writer := io.Pipe()
			defer writer.Close()
			entered := make(chan struct{})
			gate := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			defer release()
			received := make(chan error, 1)
			cancelSource := func() error { release(); return reader.Close() }
			go func() {
				_, _, err := s.Receive(owner, func(int64) (io.ReadCloser, error) {
					close(entered)
					if opening {
						<-gate
						return nil, io.ErrClosedPipe
					}
					return reader, nil
				}, cancelSource)
				received <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("reception did not open its source")
			}
			var id string
			if err := s.DB.DB.QueryRow("SELECT id FROM images WHERE user_id=?", owner).Scan(&id); err != nil {
				t.Fatal(err)
			}
			deleted := make(chan error, 1)
			go func() { deleted <- s.Delete(id, owner, false) }()
			select {
			case err := <-deleted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("deletion did not cancel/reap reception")
			}
			if err := <-received; err == nil {
				t.Fatal("cancelled reception succeeded")
			}
			if _, err := s.DB.Image(id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("cancelled reception resurrected its image", err)
			}
			if _, err := os.Stat(s.scratch(id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("cancelled reception left scratch", err)
			}
		})
	}
}

func TestDeleteReapsValidationAndConversion(t *testing.T) {
	for _, converting := range []bool{false, true} {
		t.Run(strconv.FormatBool(converting), func(t *testing.T) {
			s, monitor, owner := resourceUploadFixture(t, true)
			if err := os.MkdirAll(s.Deployment.NativeDir, 0700); err != nil {
				t.Fatal(err)
			}
			worker := filepath.Join(s.Deployment.NativeDir, "pebril-image-worker")
			if err := os.WriteFile(worker, []byte("#!/bin/sh\necho $$ > \"$PWD/pid\"\nexec /bin/sleep 60\n"), 0700); err != nil {
				t.Fatal(err)
			}
			var id, directory string
			if converting {
				id = resourceQueuedImage(t, s, owner, 1)
				directory = filepath.Join(s.conversionsDir(), id)
				monitor.set(1, false)
				s.signalConversion()
			} else {
				monitor.set(1, false)
				s.Executable = worker
				image, _, err := s.Receive(owner, func(int64) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("controlled validation input")), nil
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				id = image.ID
				directory = s.scratch(id)
			}
			var pid int
			resourceWait(t, "worker did not start", func() bool {
				data, err := os.ReadFile(filepath.Join(directory, "pid"))
				if err == nil {
					pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
				}
				return err == nil && pid > 0
			})
			if err := s.Delete(id, owner, false); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("worker was not reaped: %v", err)
			}
			if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("worker scratch remains", err)
			}
			if _, err := s.DB.Image(id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("worker resurrected deleted image", err)
			}
		})
	}
}

func TestStartupFinishesInterruptedDeletionCleanup(t *testing.T) {
	s, monitor, owner := resourceUploadFixture(t, true)
	first := resourceQueuedImage(t, s, owner, 1)
	shared := resourceQueuedImage(t, s, owner, 2)
	s.Close()
	image, err := s.DB.Image(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.DeleteImage(first, owner, false); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(s.scratch(first), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(s.conversionsDir(), first), 0700); err != nil {
		t.Fatal(err)
	}
	orphan := strings.Repeat("b", 64)
	for _, name := range []string{orphan, ".object-interrupted"} {
		if err = os.WriteFile(filepath.Join(s.objectDir(), name), []byte("interrupted publication"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := NewWithResources(s.DB, s.Manager, s.Deployment, "/bin/false", nativeprobe.Result{}, monitor)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, path := range []string{s.scratch(first), filepath.Join(s.conversionsDir(), first), filepath.Join(s.objectDir(), orphan), filepath.Join(s.objectDir(), ".object-interrupted")} {
		if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("interrupted cleanup remains at %s: %v", path, err)
		}
	}
	if _, err = s.DB.Image(shared); err != nil {
		t.Fatal("restart lost shared record", err)
	}
	if _, err = os.Stat(filepath.Join(s.objectDir(), image.Hash)); err != nil {
		t.Fatal("restart removed referenced shared object", err)
	}
}
