// Command sender prepares files for optical transmission and displays them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/db"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/logging"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/pipeline"
	"github.com/opticaltransport/otp/sender/internal/store"
)

func main() {
	var (
		configPath = flag.String("config", os.Getenv("OTP_SENDER_CONFIG"), "path to the configuration file")
		migrate    = flag.Bool("migrate", false, "apply pending migrations and exit")
		showConfig = flag.Bool("check-config", false, "validate the configuration and exit")
	)
	flag.Parse()

	if err := run(*configPath, *migrate, *showConfig); err != nil {
		// Written to stderr rather than logged, because the most likely failure is that
		// configuration was invalid — in which case there is no logger yet.
		fmt.Fprintf(os.Stderr, "sender: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, migrateOnly, checkOnly bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if checkOnly {
		fmt.Printf("configuration is valid: %s, %s encoder on a %dx%d grid, %s compression, %s error correction\n",
			cfg.Addr(), cfg.Optical.Encoder, cfg.Optical.GridWidth, cfg.Optical.GridHeight,
			cfg.Optical.Compression, cfg.Optical.FEC.Codec)
		return nil
	}

	log, err := logging.New(cfg.Log)
	if err != nil {
		return err
	}
	defer log.Sync()

	log.Info("starting",
		zap.Uint16("protocol_version", protocol.Current),
		zap.String("storage", cfg.Storage.Backend),
		zap.String("broker", cfg.Broker.Backend),
		zap.String("encoder", cfg.Optical.Encoder),
		zap.String("compression", cfg.Optical.Compression),
		zap.String("fec", cfg.Optical.FEC.Codec),
		zap.Bool("encrypted", cfg.Optical.EncryptionKeyHex != ""))

	// Signals are handled before anything is opened, so that a container being stopped during
	// startup exits promptly rather than finishing a migration nobody is waiting for.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()

	if migrateOnly || cfg.Database.MigrateOnStart {
		ran, err := pool.Migrate(ctx)
		if err != nil {
			return err
		}
		for _, m := range ran {
			log.Info("applied migration", zap.Int("version", m.Version), zap.String("name", m.Name))
		}
		if migrateOnly {
			version, err := pool.SchemaVersion(ctx)
			if err != nil {
				return err
			}
			log.Info("schema is up to date", zap.Int("version", version))
			return nil
		}
	}

	objects, err := objectstore.Open(ctx, cfg.Storage)
	if err != nil {
		return err
	}
	defer objects.Close()

	watcher := config.NewWatcher(configPath, cfg)
	watcher.OnError(func(err error) { log.Warn("configuration reload failed", zap.Error(err)) })
	watcher.OnIgnored(func(fields []string) {
		log.Warn("some configuration changes need a restart", zap.Strings("sections", fields))
	})
	watcher.OnChange(func(next config.Config) {
		if err := log.SetLevel(next.Log.Level); err != nil {
			log.Warn("could not change the log level", zap.Error(err))
		}
		log.Info("configuration reloaded",
			zap.String("log_level", log.Level()),
			zap.Int("job_concurrency", next.Jobs.Concurrency),
			zap.Float64("fps", next.Display.FPS),
			zap.Int("window", next.Display.WindowSize))
	})

	st := store.New(pool)
	js := jobs.NewStore(pool)
	engine := jobs.NewEngine(js, watcher, log.Logger)
	pipeline.New(st, js, objects, watcher, log.Logger).Register(engine)

	// The configuration watcher and the job engine run alongside each other; whichever stops
	// first stops the process, since neither is optional.
	errs := make(chan error, 2)
	go func() { errs <- watcher.Watch(ctx) }()
	go func() { errs <- engine.Start(ctx) }()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		engine.Stop()
		return nil
	case err := <-errs:
		engine.Stop()
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}
}
