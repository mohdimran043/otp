// Command receiver captures optical frames, reassembles files, and delivers them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/api"
	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/db"
	"github.com/opticaltransport/otp/receiver/internal/logging"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/receiver/internal/pipeline"
	"github.com/opticaltransport/otp/receiver/internal/store"
)

func main() {
	var (
		configPath = flag.String("config", os.Getenv("OTP_RECEIVER_CONFIG"), "path to the configuration file")
		migrate    = flag.Bool("migrate", false, "apply pending migrations and exit")
		checkOnly  = flag.Bool("check-config", false, "validate the configuration and exit")
	)
	flag.Parse()

	if err := run(*configPath, *migrate, *checkOnly); err != nil {
		fmt.Fprintf(os.Stderr, "receiver: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, migrateOnly, checkOnly bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if checkOnly {
		fmt.Printf("configuration is valid: %s, capturing from %s at %s\n",
			cfg.Addr(), cfg.Capture.Source, cfg.Capture.Dir)
		return nil
	}

	log, err := logging.New(cfg.Log)
	if err != nil {
		return err
	}
	defer log.Sync()

	log.Info("starting",
		zap.Uint16("protocol_version", protocol.Current),
		zap.String("capture", cfg.Capture.Source),
		zap.String("frames", cfg.Capture.Dir),
		zap.String("acks", cfg.Ack.Dir),
		zap.Bool("encrypted", cfg.Decoder.EncryptionKeyHex != ""),
		zap.Strings("callback_hosts", cfg.Callback.AllowedHosts))

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
			return nil
		}
	}

	objects, err := objectstore.Open(ctx, cfg.Storage)
	if err != nil {
		return err
	}
	defer objects.Close()

	// The acknowledgement channel is its own store rooted at the shared volume: a different mount, with a
	// different lifetime, reachable by the sender as well.
	acks, err := objectstore.NewFilesystem(cfg.Ack.Dir)
	if err != nil {
		return err
	}
	defer acks.Close()

	source, err := pipeline.OpenSource(cfg.Capture)
	if err != nil {
		return err
	}
	defer source.Close()

	watcher := config.NewWatcher(configPath, cfg)
	st := store.New(pool)
	receiver := pipeline.New(st, objects, acks, source, watcher, log.Logger)

	server := &http.Server{
		Addr: cfg.Addr(),
		Handler: api.New(api.Options{
			Store:   st,
			Objects: objects,
			Config:  watcher,
			Log:     log.Logger,
			// The API reports on whichever session is running, so a dashboard needs no session id to ask
			// about the live capture.
			Session: func() uuid.UUID { return receiver.Session() },
		}).Routes(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	errs := make(chan error, 2)
	go func() {
		log.Info("capture starting", zap.String("source", source.Name()))
		errs <- receiver.Run(ctx)
	}()
	go func() {
		log.Info("http listening", zap.String("addr", cfg.Addr()))
		if cfg.Server.TLSCertFile != "" && cfg.Server.TLSKeyFile != "" {
			errs <- server.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
			return
		}
		errs <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
			log.Error("stopping after a failure", zap.Error(err))
		}
	}

	log.Info("shutting down")
	// In-flight requests are given the configured grace. The capture loop stops with the context, which is
	// correct: a frame half-processed is re-read from the channel next time, because the display keeps
	// showing it until it is acknowledged.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("the HTTP server did not shut down cleanly", zap.Error(err))
	}
	// A moment for the capture loop to notice the cancelled context and close its session row.
	time.Sleep(200 * time.Millisecond)
	return nil
}
