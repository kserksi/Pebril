package nativeprobe

/*
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kserksi/Pebril/internal/resources"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

type Result struct {
	Ready       bool     `json:"ready"`
	VipsVersion string   `json:"vips_version,omitempty"`
	VipsPath    string   `json:"vips_path,omitempty"`
	Raster      bool     `json:"raster"`
	SVG         bool     `json:"svg"`
	GIF         bool     `json:"gif"`
	AVIF        bool     `json:"avif_enabled"`
	Errors      []string `json:"errors,omitempty"`
}

// ProbeLocal is used only by the hidden self-test child entry. Each process loads one candidate library.
func ProbeLocal(library, rustPath, scratch string, avif bool) Result {
	result := Result{AVIF: avif}
	ctemp := C.CString(scratch)
	defer C.free(unsafe.Pointer(ctemp))
	lib := C.CString(library)
	defer C.free(unsafe.Pointer(lib))
	var version [128]C.char
	var loaded [4096]C.char
	var message [2048]C.char
	enabled := C.int(0)
	if avif {
		enabled = 1
	}
	if C.probe_vips(lib, ctemp, enabled, &version[0], C.int(len(version)), &loaded[0], C.int(len(loaded)), &message[0], C.int(len(message))) != 0 {
		result.Raster = true
		result.VipsVersion = C.GoString(&version[0])
		result.VipsPath = C.GoString(&loaded[0])
	} else {
		result.Errors = append(result.Errors, C.GoString(&message[0]))
	}
	rust := C.CString(rustPath)
	defer C.free(unsafe.Pointer(rust))
	if C.probe_rust(rust, &message[0], C.int(len(message))) != 0 {
		result.SVG = true
		result.GIF = true
	} else {
		result.Errors = append(result.Errors, C.GoString(&message[0]))
	}
	result.Ready = result.Raster && result.SVG && result.GIF
	return result
}
func GLIBCVersion() string { return C.GoString(C.probe_glibc_version()) }

// Keep Buffer named so io.Copy cannot bypass Write through promoted ReadFrom/WriteTo.
type boundedBuffer struct{ buffer bytes.Buffer }

func (b *boundedBuffer) Len() int                      { return b.buffer.Len() }
func (b *boundedBuffer) String() string                { return b.buffer.String() }
func (b *boundedBuffer) Read(data []byte) (int, error) { return b.buffer.Read(data) }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64*1024 {
		return 0, errors.New("Native self-test output exceeds 64KiB")
	}
	return b.buffer.Write(p)
}

// Native probes do not load libraries in the main process; crashes, timeouts, and invalid output reject startup.
func runChild(ctx context.Context, executable, dir, library string, avif bool) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	scratch, err := os.MkdirTemp(dir, ".probe-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(scratch)
	args := []string{"_native-probe", library, filepath.Join(dir, "libpebril_native.so"), scratch, strconv.FormatBool(avif)}
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = dir
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "VIPS_CONCURRENCY=1", "VIPS_MAX_MEM=16777216"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	out := cancelingImageBuffer{cancel: cancel}
	stderr := cancelingImageBuffer{cancel: cancel}
	command.Stdout = &out
	command.Stderr = &stderr
	if err := resources.Run(command); err != nil {
		return Result{}, fmt.Errorf("Native self-test child failed: %w (%s)", err, stderr.String())
	}
	var result Result
	decoder := json.NewDecoder(&out)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("Invalid native self-test response: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Result{}, errors.New("Unexpected content after native self-test response")
	}
	if !result.Ready || !result.Raster || !result.SVG || !result.GIF || result.AVIF != avif {
		return result, fmt.Errorf("Core components are incomplete: %v", result.Errors)
	}
	return result, nil
}
