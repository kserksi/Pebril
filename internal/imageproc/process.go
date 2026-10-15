package imageproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kserksi/Pebril/internal/nativeassets"
	"github.com/kserksi/Pebril/internal/nativeinstall"
)

type outputBuffer struct {
	data   bytes.Buffer
	cancel context.CancelFunc
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	if len(p) > 64*1024-b.data.Len() {
		b.cancel()
		return 0, errors.New("Conversion response exceeds limit")
	}
	return b.data.Write(p)
}
func invoke(ctx context.Context, worker, library, directory string, args []string, input []byte) (Result, error) {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(child, worker, args...)
	command.Dir = directory
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "VIPS_CONCURRENCY=1", "TMPDIR=" + directory, "LD_LIBRARY_PATH=" + filepath.Dir(library)}
	command.Stdin = bytes.NewReader(input)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	out, stderr := outputBuffer{cancel: cancel}, outputBuffer{cancel: cancel}
	command.Stdout = &out
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return Result{}, errors.New("Isolated image conversion failed")
	}
	decoder := json.NewDecoder(&out.data)
	decoder.DisallowUnknownFields()
	var result Result
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("Invalid conversion response")
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}
func Prepare(ctx context.Context, native, library string) (string, string, error) {
	if err := nativeassets.EnsureWorker(native); err != nil {
		return "", "", err
	}
	var version string
	check := func(candidate string) error {
		probeContext, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		dir, err := os.MkdirTemp(native, ".conversion-probe-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		result, err := invoke(probeContext, filepath.Join(native, "pebril-image-worker"), candidate, dir, []string{"check"}, nil)
		if err != nil {
			return err
		}
		expected, e := filepath.EvalSymlinks(candidate)
		if e != nil {
			return e
		}
		actual, e := filepath.EvalSymlinks(result.Library)
		if e != nil || expected != actual {
			return errors.New("Conversion worker loaded an unexpected library")
		}
		version = result.Version
		return nil
	}
	if err := check(library); err == nil {
		return library, version, nil
	}
	// A system candidate may pass the initial probes but lack the worker's ABI.
	installed, err := nativeinstall.Ensure(ctx, native, check)
	if err != nil {
		return "", "", errors.New("Required conversion worker or runtime is unavailable")
	}
	return installed, version, nil
}
func Convert(ctx context.Context, worker, library string, request Request) (Result, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return Result{}, err
	}
	result, err := invoke(ctx, worker, library, request.Directory, nil, data)
	if err != nil {
		return result, err
	}
	if result.Width <= 0 || result.Height <= 0 || result.Frames != request.Frames || result.Cover != (request.Frames > 1) {
		return result, errors.New("Invalid converted dimensions or frame count")
	}
	return result, nil
}
