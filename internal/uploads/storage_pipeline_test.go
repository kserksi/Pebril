package uploads

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/testutil"
)

func TestNativeStoragePipeline(t *testing.T) {
	output, runtime := os.Getenv("PEBRIL_TEST_OUTPUT"), os.Getenv("PEBRIL_TEST_RUNTIME")
	if output == "" || runtime == "" {
		t.Skip("Built native pipeline inputs not configured")
	}
	output, e := filepath.Abs(output)
	if e != nil {
		t.Fatal(e)
	}
	var pngBody, gifBody bytes.Buffer
	if e = png.Encode(&pngBody, image.NewRGBA(image.Rect(0, 0, 8, 8))); e != nil {
		t.Fatal(e)
	}
	palette := color.Palette{color.Black, color.White}
	first, second := image.NewPaletted(image.Rect(0, 0, 8, 8), palette), image.NewPaletted(image.Rect(0, 0, 8, 8), palette)
	second.SetColorIndex(0, 0, 1)
	if e = gif.EncodeAll(&gifBody, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 20}, LoopCount: 2}); e != nil {
		t.Fatal(e)
	}
	for _, backend := range []string{"local", "s3"} {
		for _, database := range []string{"sqlite", "mysql"} {
			t.Run(database+"/"+backend, func(t *testing.T) {
				d, e := config.LoadDeployment(t.TempDir())
				if e != nil {
					t.Fatal(e)
				}
				if database == "mysql" {
					d = testutil.MySQLDeployment(t, d, "pipe"+backend)
				}
				d.NativeDir = filepath.Join(output, "bin")
				storage := config.Storage{Backend: "local"}
				if backend == "s3" {
					_, server := testutil.NewS3(t)
					previous := http.DefaultTransport
					http.DefaultTransport = server.Client().Transport
					t.Cleanup(func() { http.DefaultTransport = previous })
					storage = config.Storage{Backend: "s3", S3: &config.S3{Endpoint: server.URL, Bucket: "images", Region: "test-region", AccessKeyID: "test-access", SecretAccessKey: "test-secret", Addressing: "path", Prefix: "pipeline/"}}
				}
				data, e := json.Marshal(struct {
					config.Runtime
					Storage config.Storage `json:"storage"`
				}{config.Defaults(), storage})
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(d.Root, "config.json"), data, 0600); e != nil {
					t.Fatal(e)
				}
				manager, e := config.New(d, false)
				if e != nil {
					t.Fatal(e)
				}
				defer manager.Close()
				db, e := store.OpenDeployment(d)
				if e != nil {
					t.Fatal(e)
				}
				defer db.Close()
				result, e := db.DB.Exec("INSERT INTO users(username,password,quota) VALUES('pipeline',?,?)", []byte{0}, store.DefaultQuota)
				if e != nil {
					t.Fatal(e)
				}
				user, e := result.LastInsertId()
				if e != nil {
					t.Fatal(e)
				}
				service, e := New(db, manager, d, filepath.Join(output, "pebril"), nativeprobe.Result{Ready: true, VipsPath: runtime})
				if e != nil {
					t.Fatal(e)
				}
				defer func() {
					if service != nil {
						service.Close()
					}
				}()
				var ids []string
				for _, fixture := range []struct {
					name  string
					data  []byte
					cover bool
				}{
					{"static", pngBody.Bytes(), false}, {"animation", gifBody.Bytes(), true}, {"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"><rect width="8" height="8" fill="red"/></svg>`), false},
				} {
					record, done, e := service.Receive(user, func(int64) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(fixture.data)), nil }, nil)
					if e != nil {
						t.Fatal(e)
					}
					select {
					case <-done:
					case <-time.After(30 * time.Second):
						t.Fatal("Validation did not finish")
					}
					deadline := time.Now().Add(30 * time.Second)
					for {
						record, e = db.Image(record.ID)
						if e != nil {
							t.Fatal(e)
						}
						if record.State == "completed" {
							break
						}
						if record.State == "failed" || time.Now().After(deadline) {
							t.Fatal(fixture.name, record.State, record.Error)
						}
						time.Sleep(20 * time.Millisecond)
					}
					ids = append(ids, record.ID)
					_, role, file, e := service.OpenPublic(context.Background(), record.ID, false)
					if e != nil {
						t.Fatal(e)
					}
					body, e := io.ReadAll(file)
					file.Close()
					if e != nil || int64(len(body)) != role.Bytes {
						t.Fatal("Processed output unreadable", e)
					}
					if fixture.cover {
						_, cover, f, e := service.OpenPublic(context.Background(), record.ID, true)
						if e != nil {
							t.Fatal(e)
						}
						body, e = io.ReadAll(f)
						f.Close()
						if e != nil || len(body) == 0 || cover.Role != "cover" {
							t.Fatal("Animation cover unavailable", e)
						}
					}
					if fixture.name == "svg" {
						continue
					}
					_, _, original, e := service.OpenOriginal(context.Background(), record.ID, user, false)
					if e != nil {
						t.Fatal(e)
					}
					body, e = io.ReadAll(original)
					original.Close()
					if e != nil || !bytes.Equal(body, fixture.data) {
						t.Fatal("Original changed", e)
					}
				}
				service.Close()
				service, e = New(db, manager, d, filepath.Join(output, "pebril"), nativeprobe.Result{Ready: true, VipsPath: runtime})
				if e != nil {
					t.Fatal("Stored objects failed restart reconciliation", e)
				}
				for _, id := range ids {
					if e = service.Delete(id, user, false); e != nil {
						t.Fatal(e)
					}
				}
				owner, e := db.User(user)
				if e != nil || owner.Used != 0 {
					t.Fatal("Deletion left quota charged", e)
				}
			})
		}
	}
}
