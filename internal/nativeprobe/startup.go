package nativeprobe

import (
	"context"
	"errors"
	"fmt"
	"github.com/kserksi/Pebril/internal/nativeassets"
	"github.com/kserksi/Pebril/internal/nativeinstall"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func atLeast(actual, minimum string) bool {
	a, b := strings.Split(actual, "."), strings.Split(minimum, ".")
	for i := 0; i < len(b); i++ {
		left := 0
		var e error
		if i < len(a) {
			left, e = strconv.Atoi(a[i])
			if e != nil {
				return false
			}
		}
		right, e := strconv.Atoi(b[i])
		if e != nil {
			return false
		}
		if left != right {
			return left > right
		}
	}
	return true
}
func hostCompatible() error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return errors.New("Automatic runtime packages support Linux x64 only")
	}
	if !atLeast(GLIBCVersion(), "2.34") {
		return errors.New("Pebril requires glibc >=2.34")
	}
	data, e := os.ReadFile("/etc/os-release")
	if e != nil {
		return e
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = strings.Trim(value, "\"'")
		}
	}
	minimum, known := map[string]string{"debian": "12", "ubuntu": "22.04", "rocky": "9.8"}[values["ID"]]
	if !known || !atLeast(values["VERSION_ID"], minimum) {
		return fmt.Errorf("Automatic library installation is not supported on %s %s", values["ID"], values["VERSION_ID"])
	}
	data, e = os.ReadFile("/proc/cpuinfo")
	if e != nil {
		return e
	}
	checked := false
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "flags" {
			continue
		}
		checked = true
		flags := " " + strings.Join(strings.Fields(value), " ") + " "
		for _, needed := range []string{"ssse3", "sse4_1", "sse4_2", "popcnt"} {
			if !strings.Contains(flags, " "+needed+" ") {
				return fmt.Errorf("The pinned runtime package requires CPU feature %s", needed)
			}
		}
	}
	if !checked {
		return errors.New("Cannot verify CPU capabilities")
	}
	return nil
}

// Run isolates local candidates, then validates managed installations; install only when no compatible library exists.
func Run(ctx context.Context, executable, dir string, avif bool) (Result, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return Result{}, e
	}
	if e := nativeassets.Ensure(dir); e != nil {
		return Result{}, fmt.Errorf("Restore internal native library: %w", e)
	}
	candidates := []string{"/usr/lib/x86_64-linux-gnu/libvips.so.42", "/lib/x86_64-linux-gnu/libvips.so.42", "/usr/lib64/libvips.so.42", "/lib64/libvips.so.42", "/usr/local/lib/libvips.so.42", "libvips.so.42", filepath.Join(dir, "libvips.so.42")}
	seen := map[string]bool{}
	var failures []string
	var result Result
	for _, library := range candidates {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if filepath.IsAbs(library) {
			resolved, e := filepath.EvalSymlinks(library)
			if e != nil {
				continue
			}
			if seen[resolved] {
				continue
			}
			seen[resolved] = true
		}
		candidate, e := runChild(ctx, executable, dir, library, avif)
		if e == nil {
			return candidate, nil
		}
		result = candidate
		failures = append(failures, fmt.Sprintf("%s: %s", library, e))
	}
	if e := hostCompatible(); e != nil {
		return result, e
	}
	if _, e := os.Stat(filepath.Join(dir, "libpebril_native.so")); e != nil {
		return result, fmt.Errorf("Internal project library is missing; use scripts/build.sh to build with library recovery: %w", e)
	}
	slog.Info("Checking managed runtime; downloading the pinned package if needed", "artifact", nativeinstall.Vips.ID)
	_, e := nativeinstall.Ensure(ctx, dir, func(library string) error {
		candidate, e := runChild(ctx, executable, dir, library, avif)
		if e == nil {
			result = candidate
		}
		return e
	})
	if e != nil {
		result.Errors = append(failures, e.Error())
		return result, fmt.Errorf("Core runtime installation failed; cancelling startup: %w", e)
	}
	// The installation moved from staging to its final directory; verify the actual load path again.
	active, e := nativeinstall.Active(dir, nativeinstall.Vips)
	if e != nil {
		return result, e
	}
	return runChild(ctx, executable, dir, active, avif)
}
