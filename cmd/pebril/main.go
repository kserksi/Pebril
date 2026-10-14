package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/server"
	"github.com/kserksi/Pebril/internal/store"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

var version = "0.0.2-dev"

func main() {
	if err := run(); err != nil {
		slog.Error("Startup or runtime failure", "error", err)
		os.Exit(1)
	}
}
func run() error {
	args := os.Args[1:]
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

	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}
	if len(args) > 1 {
		return errors.New("Usage: pebril [serve|check|version]")
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command != "serve" && command != "check" {
		return errors.New("Usage: pebril [serve|check|version]")
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
	capability, err := nativeprobe.Run(ctx, executable, deploy.NativeDir, deploy.AVIF)
	if command == "check" {
		if e := json.NewEncoder(os.Stdout).Encode(capability); e != nil {
			return e
		}
		return err
	}
	if err != nil {
		return err
	}
	for _, dir := range []string{deploy.DataDir, deploy.PublicDir, deploy.PrivateDir} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	database, err := store.Open(deploy.DBDir)
	if err != nil {
		return err
	}
	defer database.Close()
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
	httpServer := &http.Server{Handler: server.Handler(version, manager, database, capability), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 * 1024}
	stopped := make(chan error, 1)
	go func() { stopped <- httpServer.Serve(listener) }()
	slog.Info("Foundation service started", "version", version, "address", listener.Addr().String(), "libvips", capability.VipsVersion)
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
