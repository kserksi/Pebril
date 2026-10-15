package imageproc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testWorker(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestWorkerOutputBoundAndProcessTreeCancellation(t *testing.T) {
	worker := testWorker(t, "sleep 30 &\nwhile :; do printf '%02000d' 0; done\n")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := invoke(ctx, worker, "/fixture/libvips.so.42", filepath.Dir(worker), nil, nil); err == nil {
		t.Fatal("Unbounded worker output was accepted")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("Overflow left the worker process tree or output pipes running")
	}
}
func TestWorkerProtocolAndEnvironmentIsolation(t *testing.T) {
	t.Setenv("PEBRIL_PRIVATE_TEST_SECRET", "fixture")
	worker := testWorker(t, "test -z \"${PEBRIL_PRIVATE_TEST_SECRET+x}\" || exit 1\nprintf '%s' '{\"format\":\"webp\",\"width\":2,\"height\":2,\"frames\":1,\"cover\":false}'\n")
	request := Request{Directory: filepath.Dir(worker), Frames: 1}
	result, err := Convert(context.Background(), worker, "/fixture/libvips.so.42", request)
	if err != nil || result.Format != "webp" {
		t.Fatal("Isolated conversion protocol failed", result, err)
	}
	invalid := testWorker(t, "printf '%s' '{\"format\":\"webp\",\"width\":2,\"height\":2,\"frames\":1}{}'\n")
	if _, err = Convert(context.Background(), invalid, "/fixture/libvips.so.42", Request{Directory: filepath.Dir(invalid), Frames: 1}); err == nil {
		t.Fatal("Trailing worker protocol data was accepted")
	}
}
