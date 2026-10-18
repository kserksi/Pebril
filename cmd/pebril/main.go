package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/imageproc"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/resources"
	"github.com/kserksi/Pebril/internal/server"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

var version = "0.0.9-dev"

func main() {
	if err := run(); err != nil {
		slog.Error("Startup or runtime failure", "error", err)
		os.Exit(1)
	}
}
func run() error {
	args := os.Args[1:]
	if os.Geteuid() == 0 && !(len(args) == 1 && args[0] == "version") {
		return errors.New("Run Pebril as an unprivileged user; use pebril.sh for dependency setup")
	}
	if len(args) > 0 && args[0] == "_native-probe" {
		if len(args) != 5 {
			return errors.New("Invalid self-test arguments")
		}
		avif, err := strconv.ParseBool(args[4])
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(nativeprobe.ProbeLocal(args[1], args[2], args[3], avif))
	}

	if len(args) > 0 && args[0] == "_image-verify" {
		if len(args) != 6 {
			return errors.New("Invalid image validation arguments")
		}
		data, err := os.ReadFile(args[5])
		if err != nil {
			return err
		}
		var policy config.Runtime
		if err = json.Unmarshal(data, &policy); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(nativeprobe.VerifyLocal(args[1], args[2], args[3], args[4], policy))
	}
	command, allow, err := commandArgs(args)
	if err != nil {
		return err
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	deploy, err := config.LoadDeployment(filepath.Dir(executable))
	if err != nil {
		return err
	}
	if command == "reset-admin" {
		return resetAdministrator(deploy)
	}
	deploy.Restricted, err = resources.CheckStartup(allow, command == "check")
	if err != nil {
		return err
	}
	manager, err := config.New(deploy, true)
	if err != nil {
		return err
	}
	defer manager.Close()
	if err = os.MkdirAll(deploy.NativeDir, 0700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	capability, err := nativeprobe.Run(ctx, executable, deploy.NativeDir, deploy.AVIF && !deploy.Restricted)
	if err == nil {
		capability.VipsPath, capability.VipsVersion, err = imageproc.Prepare(ctx, deploy.NativeDir, capability.VipsPath)
		if err != nil {
			capability.Ready = false
			capability.Errors = append(capability.Errors, "Conversion runtime preparation failed")
		}
	}
	if command == "check" {
		if e := json.NewEncoder(os.Stdout).Encode(capability); e != nil {
			return e
		}
		return err
	}
	if err != nil {
		return err
	}
	for _, dir := range []string{deploy.DataDir, deploy.PublicDir, deploy.PrivateDir, deploy.DBDir, filepath.Join(deploy.DataDir, ".incoming"), filepath.Join(deploy.PublicDir, ".objects")} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	monitor, err := resources.NewMonitor(ctx, func() int { return manager.Snapshot().Policy.ConversionConcurrency }, deploy.Restricted,
		[]string{deploy.DBDir, deploy.PublicDir, deploy.PrivateDir, filepath.Join(deploy.DataDir, ".incoming"), filepath.Join(deploy.DataDir, ".converting"), filepath.Join(deploy.PublicDir, ".objects"), os.TempDir()})
	if err != nil {
		return err
	}
	defer monitor.Close()
	database, err := store.Open(deploy.DBDir)
	if err != nil {
		return err
	}
	defer database.Close()
	authentication, err := auth.New(database)
	if err != nil {
		return err
	}
	upload, err := uploads.NewWithResources(database, manager, deploy, executable, capability, monitor)
	if err != nil {
		return err
	}
	defer upload.Close()
	if err = manager.Watch(func(err error) {
		if err != nil {
			slog.Error("New configuration rejected; retaining the last valid configuration", "error", err)
		} else {
			slog.Info("Configuration applied", "revision", manager.Snapshot().Revision)
		}
	}); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", deploy.Listen)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: server.HandlerWithUploads(version, manager, database, capability, authentication, upload), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 * 1024}
	stopped := make(chan error, 1)
	go func() { stopped <- httpServer.Serve(listener) }()
	slog.Info("Image service started", "version", version, "address", listener.Addr().String(), "libvips", capability.VipsVersion)
	select {
	case err = <-stopped:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = httpServer.Shutdown(shutdown); err != nil {
			httpServer.Close()
			return err
		}
		err = <-stopped
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func commandArgs(args []string) (string, bool, error) {
	command, selected, allow := "serve", false, false
	for _, arg := range args {
		switch arg {
		case "--allow-low-memory":
			allow = true
		case "serve", "check", "version", "reset-admin":
			if selected {
				return "", false, errors.New("Choose one service command")
			}
			command, selected = arg, true
		default:
			return "", false, errors.New("Usage: pebril [serve|check|version] [--allow-low-memory], or pebril reset-admin")
		}
	}
	if command == "reset-admin" && len(args) != 1 {
		return "", false, errors.New("Usage: pebril reset-admin (no additional arguments)")
	}
	return command, allow, nil
}
