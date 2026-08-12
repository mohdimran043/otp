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
	"sort"
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

	st := store.New(pool)

	// Settings the operator changed through the UI, laid over the file and the environment before anything
	// reads the configuration.
	//
	// This is what makes those changes mean anything across a restart. The settings API used to apply them to
	// the running configuration and store nothing, which the reloadable settings survived and the display sink
	// did not: the sink is opened once, here, so choosing "camera" as the transfer channel took effect on the
	// next restart — and the restart re-read the file and the environment and discarded the choice. The
	// control could not work. It has to happen after the migrations, because it reads a table they create.
	cfg = withStoredSettings(ctx, st, cfg, log)

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

	// A transfer that was displaying when this process last stopped is given its display loop back.
	//
	// The loop is a goroutine, so it dies with the process; the status is a row, so it does not. Without
	// this, a restart mid-transfer leaves a transmission claiming to be transmitting with nothing behind
	// it — the display stays blank, the transfer never completes, and every status the API reports says it
	// is in flight. It cannot be rescued through the API either, because start takes only a ready transfer
	// and resume only a paused one, so the single status that needs it is the one neither accepts.
	//
	// Re-displaying from the beginning of what is outstanding is the correct recovery rather than a
	// compromise: acknowledgements are durable, so the scheduler shows only unacknowledged chunks, and it
	// deliberately keeps its retransmission timers in memory because a restart cannot know what the
	// receiver saw while it was gone.
	resumeInterruptedDisplays(ctx, st, transmit, log.Logger)

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

// resumeInterruptedDisplays gives a display loop back to every transfer that was mid-display when this
// process last stopped, and reports how many it started.
//
// Only the transmitting status, because only it is a broken promise. A transmitting row asserts that
// something is putting frames on the screen, and after a restart that assertion is false — which is exactly
// the condition to repair. Every other unfinished status is waiting for something that is still true: a ready
// transfer prepared with autostart=false is waiting for an operator to press start, and displaying it because
// the process happened to restart would put a file on a screen they had chosen not to put there yet. A paused
// one is waiting for the same reason, more explicitly.
//
// A failure here is logged and not fatal. The transfers are recoverable by hand, and a sender that refuses to
// boot because it could not read one row is worse than one that boots and says what it could not resume.
func resumeInterruptedDisplays(ctx context.Context, st *store.Store, transmit func(context.Context, uuid.UUID), log *zap.Logger) int {
	// The limit is stated rather than left to default, which pages at a hundred. A hundred transfers
	// interleaving on one screen is not a real deployment, so the difference will never be reached — but a
	// default that quietly drops the hundred-and-first would leave exactly the orphan this exists to
	// prevent, and be invisible when it did.
	const allInterrupted = 1000

	interrupted, err := st.Transmissions.List(ctx,
		[]store.TransmissionStatus{store.TxTransmitting}, allInterrupted, 0)
	if err != nil {
		log.Warn("could not look for transfers interrupted by a restart", zap.Error(err))
		return 0
	}

	for _, tx := range interrupted {
		log.Info("resuming a transfer interrupted by a restart",
			zap.String("transmission", tx.ID.String()),
			zap.Int("acked_chunks", tx.AckedChunks),
			zap.Int("chunk_count", tx.ChunkCount))
		transmit(ctx, tx.ID)
	}
	return len(interrupted)
}

// withStoredSettings lays the operator's stored display settings over a freshly loaded configuration.
//
// Every failure here is a warning and a fall back to the un-overlaid configuration, never a refusal to start.
// That is deliberate. The stored settings are a convenience — the UI remembering what it was told — and a
// sender that will not boot because one row holds a value it cannot parse is far worse than one that boots on
// the configured defaults and says so. The three ways it can go wrong are all handled the same way: the table
// is missing because migrations have not been run, a value will not parse, or the overlaid result does not
// validate as a whole.
//
// Note the deliberate gap: this runs at startup only. If sender.yaml is edited while the process is running,
// the watcher reloads the file and the stored overlay is not reapplied, so a reloadable setting like the frame
// rate would revert until the next restart. The sink is unaffected either way, being read only here.
func withStoredSettings(ctx context.Context, st *store.Store, cfg config.Config, log *logging.Logger) config.Config {
	stored, err := st.DisplaySettings.All(ctx)
	if err != nil {
		log.Warn("could not read stored display settings; continuing on the configured values",
			zap.Error(err))
		return cfg
	}
	if len(stored) == 0 {
		return cfg
	}

	overlaid, err := cfg.WithOverrides(stored)
	if err != nil {
		log.Warn("a stored display setting could not be read; continuing on the configured values",
			zap.Error(err))
		return cfg
	}
	if err := overlaid.Validate(); err != nil {
		log.Warn("the stored display settings do not make a valid configuration; continuing on the configured values",
			zap.Error(err))
		return cfg
	}

	keys := make([]string, 0, len(stored))
	for key := range stored {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	log.Info("applied stored display settings", zap.Strings("settings", keys),
		zap.String("sink", overlaid.Display.Sink))

	return overlaid
}
