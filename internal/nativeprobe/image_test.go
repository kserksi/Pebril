package nativeprobe

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/config"
)

func TestDisabledAVIFDoesNotInvokeCodec(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input")
	header := make([]byte, 20)
	binary.BigEndian.PutUint32(header, 20)
	copy(header[4:], "ftypavif")
	copy(header[16:], "mif1")
	if err := os.WriteFile(input, header, 0600); err != nil {
		t.Fatal(err)
	}
	result := VerifyLocal("missing-codec", "missing-library", input, filepath.Join(root, "output"), config.Defaults())
	if result.Error != "Upload format is disabled" {
		t.Fatal("Disabled AVIF reached the codec", result)
	}
}

func TestAVIFBrandBeyondSniffedBoxLength(t *testing.T) {
	header := make([]byte, 512)
	binary.BigEndian.PutUint32(header, 1024)
	copy(header[4:], "ftypavif")
	if !isAVIF(header) {
		t.Fatal("A primary AVIF brand was hidden by a large format box")
	}
	copy(header[8:], "heic")
	if isAVIF(header) {
		t.Fatal("HEIC was recognized as AVIF")
	}
}

func TestNoisyValidatorIsTerminatedBeforeDeadline(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "validator")
	script := []byte("#!/bin/sh\nwhile :; do printf '%01024d\n' 0 >&2; done\n")
	if err := os.WriteFile(executable, script, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Verify(ctx, executable, "unused", "unused", filepath.Join(root, "input"), filepath.Join(root, "output"), filepath.Join(root, "policy"))
	if err == nil {
		t.Fatal("Excessive native output was accepted")
	}
	if ctx.Err() != nil {
		t.Fatal("Validator blocked until the external deadline instead of enforcing the output limit")
	}
}
