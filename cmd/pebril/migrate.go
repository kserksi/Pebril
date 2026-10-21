package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/migration"
)

func migrateOffline(args []string) error {
	if os.Getuid() != os.Geteuid() {
		return errors.New("Run migration as the ordinary service user")
	}
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	source := flags.String("source-root", "", "Stopped and backed-up SQLite/local deployment root")
	target := flags.String("target-root", "", "Separate target deployment root with prepared .env and config.json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source == "" || *target == "" || flags.NArg() != 0 {
		return errors.New("Usage: pebril migrate --source-root PATH --target-root PATH; stop and back up the source first")
	}
	from, err := config.LoadDeploymentFile(*source)
	if err != nil {
		return err
	}
	to, err := config.LoadDeploymentFile(*target)
	if err != nil {
		return err
	}
	sourceConfig, err := config.New(from, false)
	if err != nil {
		return err
	}
	defer sourceConfig.Close()
	targetConfig, err := config.New(to, false)
	if err != nil {
		return err
	}
	defer targetConfig.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return migration.Run(ctx, migration.Options{Source: from, Target: to, SourceStorage: sourceConfig.Storage(), TargetStorage: targetConfig.Storage(), Progress: os.Stdout})
}
