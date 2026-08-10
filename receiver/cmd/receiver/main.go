// Command receiver captures optical frames, reassembles files, and delivers them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/api"
	"github.com/opticaltransport/otp/receiver/internal/camera"
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

// currentCamera is the selection in force, as the watcher needs to see it.
func currentCamera(w *config.Watcher) camera.Selection {
	cfg := w.Current()
	return camera.Selection{
		Device: cfg.Capture.Device,
		Format: cfg.Capture.Format,
		Width:  cfg.Capture.Width,
		Height: cfg.Capture.Height,
		FPS:    cfg.Capture.FPS,
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

	// The camera an operator chose through the UI, applied before capture opens.
	//
	// Precedence is explicit configuration, then the saved choice, then the default camera in its best
	// mode. Configuration wins because it is what an operator wrote down deliberately, and a service that
	// let a click override a checked-in setting would make deployments unreproducible. The saved choice
	// wins over the default because it is also deliberate, just made through a different interface.
	if cfg.Capture.Device == "" {
		saved, err := camera.LoadSelection(cfg.Storage.Root)
		if err != nil {
			log.Warn("could not read the saved camera selection", zap.Error(err))
		} else if !saved.Zero() {
			// The source comes from the saved choice too, so an operator who switched to a camera in the UI
			// gets one at the next start rather than having to edit configuration as well.
			//
			// Only when configuration did not ask for something else, though. Precedence is the same as for the
			// device: what an operator wrote down deliberately outranks what they clicked, because a deployment
			// has to be reproducible from its configuration. Getting this backwards meant an explicit
			// OTP_RECEIVER_CAPTURE_SOURCE=camera was quietly overridden by a stale saved preference — the
			// receiver announced "capture: camera" at startup and then opened the file source.
			//
			// "Default" is the test rather than "unset", because the two are indistinguishable by the time this
			// runs and it does not matter: an operator who explicitly configures the default gets the default
			// either way.
			explicit := cfg.Capture.Source != config.Default().Capture.Source
			if saved.Source != "" && saved.Source != cfg.Capture.Source && !explicit {
				log.Info("using the saved capture source",
					zap.String("source", saved.Source), zap.String("was", cfg.Capture.Source))
				cfg.Capture.Source = saved.Source
			}
			cfg.Capture.Device = saved.Device
			cfg.Capture.Format = saved.Format
			cfg.Capture.Width = saved.Width
			cfg.Capture.Height = saved.Height
			cfg.Capture.FPS = saved.FPS
			log.Info("using the saved camera selection", zap.String("camera", saved.String()))
		}
	}
	// Auto-configuration at startup: the default camera in its best mode, when one is attached and nothing has
	// been chosen. Done whatever the capture source is, so that an operator who later switches the source to
	// gocv finds a camera already configured rather than a blank form.
	if cfg.Capture.Device == "" {
		if devices, err := camera.List(); err == nil && len(devices) > 0 {
			if preferred, ok := camera.Preferred(devices); ok {
				cfg.Capture.Device = preferred.Device
				cfg.Capture.Format = preferred.Format
				cfg.Capture.Width = preferred.Width
				cfg.Capture.Height = preferred.Height
				cfg.Capture.FPS = preferred.FPS
				log.Info("camera detected and configured automatically",
					zap.String("camera", preferred.String()))
			}
		} else if err != nil {
			log.Debug("could not enumerate capture devices", zap.Error(err))
		}
	}

	source, err := pipeline.OpenSource(cfg.Capture)
	if err != nil {
		// A configured source this build cannot open must not stop the receiver.
		//
		// It stopped it once. A capture source chosen in the settings page was persisted, the binary had no
		// such source compiled in, and the next start exited with a one-line error — so a preference expressed
		// through the interface had taken the service down. A receiver that starts on a channel it can read and
		// says loudly what it could not honour is far better than one that will not start at all, because the
		// second leaves an operator with no interface in which to undo the choice.
		fallback := cfg.Capture
		fallback.Source = "file"
		fallbackSource, fallbackErr := pipeline.OpenSource(fallback)
		if fallbackErr != nil {
			return err
		}
		log.Error("the configured capture source could not be opened; reading frames instead",
			zap.String("configured", cfg.Capture.Source),
			zap.Strings("available", pipeline.AvailableSources()),
			zap.Error(err))
		cfg.Capture.Source = fallback.Source
		source = fallbackSource
	}
	// Wrapped so the source can be replaced while the receiver runs. Selecting a camera in the settings page
	// should mean the camera starts, not that a preference is filed for the next restart.
	channel := pipeline.NewSwappable(source, cfg.Capture)
	defer channel.Close()

	// The watcher is built after the source is opened, so cfg already carries any substitution — which means
	// the settings page reports the source actually in use rather than the one that was asked for and failed.
	watcher := config.NewWatcher(configPath, cfg)
	st := store.New(pool)
	receiver := pipeline.New(st, objects, acks, channel, watcher, log.Logger)

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
			// Only the source knows how deep its backlog got, and the API must not reach into the pipeline
			// to ask — so the number is injected.
			Behind: func() int64 {
				if fs, ok := channel.Current().(interface{ Behind() int64 }); ok {
					return fs.Behind()
				}
				return 0
			},
			// Replaces the running source. The new one is opened before the old is closed, so a camera that
			// will not open leaves the receiver capturing from whatever it had.
			// Frames from a browser go to the running source, when the running source is the one that takes
			// them. Resolved at call time rather than captured once, because the source can be swapped.
			Push: func(img image.Image, raw []byte) (bool, error) {
				browser, ok := channel.Current().(*pipeline.BrowserSource)
				if !ok {
					return false, fmt.Errorf(
						"the capture source is %q, so posted frames have nowhere to go; switch it to \"browser\"",
						channel.Name())
				}
				return browser.Push(img, raw)
			},
			// Imports replay a frame archive into the live pipeline, exactly as though a camera had
			// seen each frame. Ingest is a method value on the running receiver, so it needs no
			// wrapping — its signature already matches what the API package wants.
			Ingest: receiver.Ingest,
			// Probe answers the one question the API package must not have to know how to answer
			// itself: does this image decode as a frame. The importer uses it to tell a composite of
			// two stacked frames from an ordinary single one.
			Probe: func(img image.Image) bool {
				return pipeline.Decodable(img, watcher.Current())
			},
			Switch: func(next config.Capture) error {
				if err := channel.Swap(next); err != nil {
					return err
				}
				// Recorded after the swap succeeded, so what the API reports is what is really being read from.
				watcher.SetSource(next.Source)
				log.Info("capture source switched",
					zap.String("source", next.Source), zap.String("device", next.Device))
				return nil
			},
		}).Routes(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// A camera is not a fixed part of a machine. A laptop's built-in one is there at boot, but the camera an
	// operator actually wants is usually plugged in afterwards — and a receiver that settled on "none attached"
	// at startup would believe it for as long as it ran. Requiring a restart to notice a USB device reads as
	// broken.
	//
	// It configures a camera only when the receiver has none it can use, and never overrides a working choice:
	// an operator who picked the second camera keeps it when a third appears, because their decision is better
	// evidence than the order the kernel enumerated the devices in.
	go camera.Watcher{
		List:    camera.List,
		Current: func() camera.Selection { return currentCamera(watcher) },
		Apply: func(selection camera.Selection, reason string) error {
			applied := watcher.SetCamera(selection.Device, selection.Format,
				selection.Width, selection.Height, selection.FPS)
			log.Info("camera configured automatically",
				zap.String("camera", selection.String()), zap.String("reason", reason))
			// Persisted so the choice survives a restart, exactly as an operator's would. A failure here
			// leaves the camera in use and only costs its persistence, which is the lesser problem.
			if err := camera.SaveSelection(applied.Storage.Root, selection); err != nil {
				log.Warn("could not persist the automatic camera selection", zap.Error(err))
			}
			return nil
		},
	}.Run(ctx)

	errs := make(chan error, 2)
	go func() {
		log.Info("capture starting", zap.String("source", channel.Name()))
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
