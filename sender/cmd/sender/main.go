// Command sender prepares files for optical transmission and displays them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/ackwatch"
	"github.com/opticaltransport/otp/sender/internal/api"
	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/db"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/logging"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/optical"
	"github.com/opticaltransport/otp/sender/internal/pipeline"
	"github.com/opticaltransport/otp/sender/internal/scheduler"
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
	line := pipeline.New(st, js, objects, watcher, log.Logger)
	line.Register(engine)
	acks := ackwatch.New(st, watcher, log.Logger)

	// The retention sweep is self-scheduling once it is running — see pipeline.retention —
	// but something has to enqueue the first one. This is that something. It checks for an
	// already-pending retention job first so a routine restart does not pile up duplicate
	// sweeps, but enqueuing unconditionally would cost nothing worse than one harmless extra
	// pass: reap.Transfer is idempotent, so two sweeps finding the same candidate is not a bug.
	if pending, err := js.List(ctx, jobs.Filter{
		Types:  []string{pipeline.TypeRetention},
		Status: []jobs.Status{jobs.StatusPending, jobs.StatusRunning},
		Limit:  1,
	}); err != nil {
		log.Warn("could not check for a pending retention job", zap.Error(err))
	} else if len(pending) == 0 {
		if _, err := js.Enqueue(ctx, jobs.Spec{
			Type:     pipeline.TypeRetention,
			RunAfter: time.Now().Add(cfg.Retention.Interval),
		}, cfg.Jobs.MaxAttempts); err != nil {
			log.Warn("could not seed the retention job", zap.Error(err))
		}
	}

	// Open returns the channel already wrapped, so "what is on the screen right now" has one answer rather
	// than one per scheduler, and the display sequence is assigned in one place. Wrapping it again here
	// would assign the sequence twice and publish a number that did not match the frame on the channel.
	sink, err := optical.Open(cfg.Display)
	if err != nil {
		return err
	}
	defer sink.Close()

	// One display loop per transmission, tracked so shutdown can wait for them.
	//
	// A scheduler per transmission rather than one shared: its retransmission timers describe a single
	// transfer, and sharing them would let a slow transfer's timeouts govern a fast one's.
	var displays sync.WaitGroup
	transmit := func(_ context.Context, id uuid.UUID) {
		displays.Add(1)
		go func() {
			defer displays.Done()

			// Preparation is a chain of job rows, so its completion is a database fact rather than an event this
			// goroutine owns — which is why this waits on the status rather than on a channel.
			deadline := time.Now().Add(24 * time.Hour)
			for time.Now().Before(deadline) {
				if ctx.Err() != nil {
					return
				}
				tx, err := st.Transmissions.Get(ctx, id)
				if err != nil {
					log.Warn("could not read the transmission", zap.Error(err))
					return
				}
				if tx.Status == store.TxReady || tx.Status == store.TxTransmitting {
					break
				}
				if tx.Status == store.TxFailed || tx.Status == store.TxCancelled {
					log.Warn("transmission will not be displayed",
						zap.String("transmission", id.String()),
						zap.String("status", string(tx.Status)), zap.String("error", tx.Error))
					return
				}
				time.Sleep(250 * time.Millisecond)
			}

			sched := scheduler.New(st, objects, sink, watcher, log.Logger)
			stats, err := sched.Run(ctx, id)
			switch {
			case errors.Is(err, scheduler.ErrCancelled), errors.Is(err, scheduler.ErrPaused):
				// An operator stopped it. The scheduler has already recorded why and logged it, and this
				// is emphatically not an error: reporting it as one would put a deliberate stop in the
				// same bucket as a camera that came unplugged.
				return
			case err != nil && ctx.Err() == nil:
				log.Error("display stopped", zap.String("transmission", id.String()), zap.Error(err))
				return
			}
			log.Info("display finished",
				zap.String("transmission", id.String()),
				zap.Int64("frames_shown", stats.FramesShown),
				zap.Int("retransmissions", stats.Retransmissions),
				zap.Bool("complete", stats.Complete),
				zap.Duration("took", stats.Duration))
		}()
	}

	server := &http.Server{
		Addr: cfg.Addr(),
		Handler: api.New(api.Options{
			Store:    st,
			Jobs:     js,
			Objects:  objects,
			Pipeline: line,
			Acks:     acks,
			Config:   watcher,
			Display:  sink,
			Log:      log.Logger,
			Transmit: transmit,
		}).Routes(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Four loops, none of them optional: configuration reload, the worker pool, the acknowledgement
	// watcher, and the HTTP listener. Whichever fails first ends the process, because a sender missing any
	// one of them looks healthy and transfers nothing.
	errs := make(chan error, 4)
	go func() { errs <- watcher.Watch(ctx) }()
	go func() { errs <- engine.Start(ctx) }()
	go func() { errs <- acks.Run(ctx) }()
	go func() {
		log.Info("http listening", zap.String("addr", cfg.Addr()))
		if cfg.TLSEnabled() {
			errs <- server.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
			return
		}
		errs <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
			log.Error("stopping after a failure", zap.Error(err))
		}
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("the HTTP server did not shut down cleanly", zap.Error(err))
	}
	engine.Stop()
	// Display loops are given a moment to notice the cancelled context. A frame half-written is not a
	// problem: the sink renames into place, so the receiver sees a complete frame or none.
	displays.Wait()
	return nil
}
