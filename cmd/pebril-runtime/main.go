// pebril-runtime is the independent deployment helper; it does not start the service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeassets"
	"github.com/kserksi/Pebril/internal/nativeinstall"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/resources"
)

var checkStartup = resources.CheckStartup

func main() {
	ctx := context.Background()
	stop := func() {}
	// Preflight has no transaction to unwind; preserve signal termination during its prompt.
	if len(os.Args) < 2 || os.Args[1] != "settings" {
		ctx, stop = signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	}
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func safePath(name string) error {
	if strings.ContainsAny(name, "\r\n\x00") {
		return errors.New("Deployment paths must not contain line breaks or NUL bytes")
	}
	return nil
}

func run(ctx context.Context, args []string) error {
	if os.Geteuid() == 0 {
		return errors.New("Run the deployment helper as the ordinary service user; only Bash system installation steps may elevate")
	}
	if len(args) == 5 && args[0] == "_native-probe" {
		avif, err := strconv.ParseBool(args[4])
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(nativeprobe.ProbeLocal(args[1], args[2], args[3], avif))
	}
	if len(args) == 1 && args[0] == "host" {
		return nativeprobe.HostCompatible()
	}
	if len(args) >= 2 && args[0] == "settings" {
		allow, checkOnly := false, false
		for _, option := range args[2:] {
			switch option {
			case "--allow-low-memory":
				allow = true
			case "--check":
				checkOnly = true
			default:
				return errors.New("Invalid deployment settings option")
			}
		}
		root, err := filepath.EvalSymlinks(args[1])
		if err != nil {
			return err
		}
		deploy, err := config.LoadDeployment(root)
		if err != nil {
			return err
		}
		deploy.Restricted, err = checkStartup(allow, checkOnly)
		if err != nil {
			return err
		}
		avif := deploy.AVIF && !deploy.Restricted
		for _, field := range []string{deploy.NativeDir, strconv.FormatBool(avif), deploy.Root} {
			if err = safePath(field); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(os.Stdout, "%s\x00%t\x00%s\x00%t\x00", deploy.NativeDir, avif, deploy.Root, deploy.Restricted)
		return err
	}
	valid := len(args) == 3 && args[0] == "find" || len(args) == 4 && (args[0] == "probe" || args[0] == "install")
	if !valid {
		return errors.New("Usage: pebril-runtime settings ROOT [--check] [--allow-low-memory] | host | find NATIVE AVIF | probe LIBRARY NATIVE AVIF | install upstream|pebril NATIVE AVIF")
	}
	native := args[len(args)-2]
	if err := safePath(native); err != nil {
		return err
	}
	native, err := filepath.Abs(native)
	if err != nil {
		return err
	}
	avif, err := strconv.ParseBool(args[len(args)-1])
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(native, 0700); err != nil {
		return err
	}
	if err = nativeassets.Ensure(native); err != nil {
		return err
	}
	if err = nativeassets.EnsureWorker(native); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var library string
	switch args[0] {
	case "find":
		result, e := nativeprobe.Run(ctx, executable, native, avif)
		if e != nil {
			return e
		}
		library = result.VipsPath
	case "probe":
		if err = safePath(args[1]); err != nil {
			return err
		}
		result, e := nativeprobe.Check(ctx, executable, native, args[1], avif)
		if e != nil {
			return e
		}
		library = result.VipsPath
	case "install":
		artifact, ok := nativeinstall.Source(args[1])
		if !ok {
			return errors.New("Unknown runtime source; use upstream or pebril")
		}
		if err = nativeprobe.HostCompatible(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Installing reviewed runtime:", artifact.ID, artifact.URL)
		library, err = nativeinstall.Install(ctx, native, artifact, func(candidate string) error {
			_, e := nativeprobe.Check(ctx, executable, native, candidate, avif)
			return e
		})
		if err != nil {
			return err
		}
	}
	if err = safePath(library); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, library)
	return err
}
