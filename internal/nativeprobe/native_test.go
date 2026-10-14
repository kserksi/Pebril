package nativeprobe

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMissingLibraries(t *testing.T) {
	dir := t.TempDir()
	result := ProbeLocal(filepath.Join(dir, "missing-vips.so"), filepath.Join(dir, "missing-rust.so"), dir, false)
	if result.Ready || result.SVG || result.GIF || len(result.Errors) == 0 {
		t.Fatal("Missing libraries must fail", result)
	}
}
func TestChildFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "crash")
	if e := os.WriteFile(script, []byte("#!/bin/sh\nkill -SEGV $$\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e := runChild(context.Background(), script, dir, "missing", false); e == nil {
		t.Fatal("Crashes must be rejected")
	}
	script = filepath.Join(dir, "sleep")
	if e := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60 &\nwait\n"), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, e := runChild(ctx, script, dir, "missing", false); e == nil {
		t.Fatal("Timeouts must be rejected")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("Process tree was not collected promptly")
	}
}

func TestVersionComparison(t *testing.T) {
	for _, test := range []struct {
		actual, minimum string
		want            bool
	}{{"2.34", "2.34", true}, {"2.33", "2.34", false}, {"22.04", "22.04", true}, {"9.8", "9.8", true}, {"9.7", "9.8", false}, {"13", "12", true}, {"invalid", "12", false}} {
		if got := atLeast(test.actual, test.minimum); got != test.want {
			t.Fatal(test, got)
		}
	}
}

func TestBufferedCopyCannotBypassOutputLimit(t *testing.T) {
	var buffer boundedBuffer
	if _, err := io.Copy(&buffer, strings.NewReader(strings.Repeat("x", 65*1024))); err == nil {
		t.Fatal("io.Copy bypassed the output bound")
	}
	if buffer.Len() > 64*1024 {
		t.Fatal("Output buffer exceeded the bound")
	}
}
