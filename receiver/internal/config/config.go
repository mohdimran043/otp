// Package config loads the receiver's configuration.
//
// It mirrors the sender's in shape — YAML, environment overrides, a reloadable subset — because an
// operator running both should not have to learn two conventions. What differs is what it
// configures: a camera rather than a display, a decoder rather than an encoder, and the callback
// delivery the receiver performs on the sender's behalf.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/opticaltransport/otp/receiver/ai/engine"
	"github.com/opticaltransport/otp/receiver/ai/soft"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Config is the receiver's complete configuration.
type Config struct {
	Server   Server   `yaml:"server"`
	Database Database `yaml:"database"`
	Storage  Storage  `yaml:"storage"`
	Capture  Capture  `yaml:"capture"`
	Peer     Peer     `yaml:"peer"`
	Decoder  Decoder  `yaml:"decoder"`
	Ack      Ack      `yaml:"ack"`
	Callback Callback `yaml:"callback"`
	Auth     Auth     `yaml:"auth"`
	Log      Log      `yaml:"log"`
	Metrics  Metrics  `yaml:"metrics"`
}

// Server is the HTTP listener.
type Server struct {
	Host          string        `yaml:"host"`
	Port          int           `yaml:"port"`
	ReadTimeout   time.Duration `yaml:"read_timeout"`
	WriteTimeout  time.Duration `yaml:"write_timeout"`
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`
	TLSCertFile   string        `yaml:"tls_cert_file"`
	TLSKeyFile    string        `yaml:"tls_key_file"`
	CORSOrigins   []string      `yaml:"cors_origins"`
}

// Database is the Postgres connection.
type Database struct {
	URL             string        `yaml:"url"`
	MaxConns        int32         `yaml:"max_conns"`
	MinConns        int32         `yaml:"min_conns"`
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime"`
	MaxConnIdleTime time.Duration `yaml:"max_conn_idle_time"`
	ConnectTimeout  time.Duration `yaml:"connect_timeout"`
	MigrateOnStart  bool          `yaml:"migrate_on_start"`
}

// Storage is where captured frames, chunks, and merged files live.
type Storage struct {
	Backend string `yaml:"backend"`
	Root    string `yaml:"root"`
	MinIO   MinIO  `yaml:"minio"`
}

// MinIO is the S3-compatible backend's settings.
type MinIO struct {
	Endpoint  string `yaml:"endpoint"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
	UseSSL    bool   `yaml:"use_ssl"`
	Region    string `yaml:"region"`
}

// Capture configures the optical input.
type Capture struct {
	// Source is "browser", "camera", "file", or "gocv" in a build that includes it.
	//
	// "browser" is the default, and it is the counterpart to the sender defaulting to the camera-only
	// sink: a page holding the camera posts what it sees, so the two halves meet over an optical path
	// out of the box rather than over a shared mount. It is the right default of the two camera sources
	// because it needs no device — the page owns the camera and the operating system asks its own
	// permission — whereas "camera" reads Video4Linux directly and a containerised receiver has no
	// /dev/video0 unless someone passes one in.
	//
	// "file" reads the frames the sender's file sink writes. That is a development channel, and useful
	// as one; it is not what this system is for, and defaulting to it meant a deployment could carry a
	// file end to end without a camera ever being involved.
	Source string `yaml:"source"`

	// Dir is the file source's directory: the shared volume the sender writes frames into.
	Dir string `yaml:"dir"`

	// Consume deletes each frame once it has been read, which is what makes the shared directory
	// behave like a channel rather than an archive. Turning it off is useful for replaying a
	// session, and useless in production: the directory would grow without bound.
	Consume bool `yaml:"consume"`

	// IdleInterval is how long to wait before looking again when the channel is quiet. Reloadable.
	IdleInterval time.Duration `yaml:"idle_interval"`

	// RetainFrames keeps captured frames after a transmission completes. They are what a replay
	// works from, so an installation still being tuned wants them.
	RetainFrames bool `yaml:"retain_frames"`

	// MinToneFraction is how much of a captured image must be dark, and how much must be light, before it is
	// worth trying to decode: the cheap "is anything on that screen" test that keeps a waiting camera from
	// filling the failure log. Reloadable, because it is the one knob that can silently hide everything.
	//
	// A rejected image reaches neither the decoder nor the failure log, so when this is set too high frames are
	// posted, counted as accepted, and then vanish — indistinguishable from a decode failure, and with no
	// evidence either way. Aiming a camera is exactly when that matters.
	//
	// The default twelfth suits a frame that fills the view. It does not suit a small or slightly soft one,
	// least of all a binary frame: two levels average toward flat grey as the cells blur together, collapsing
	// both tails at once. Lower it while aiming; zero turns the test off and leaves the decoder's own checksums
	// as the only filter, which costs wasted decode attempts and nothing else.
	MinToneFraction float64 `yaml:"min_tone_fraction"`

	// Device, Format, Width, Height, and FPS configure the camera the "gocv" source opens.
	//
	// An empty Device means the default camera — the lowest-numbered device that actually reports a video
	// capture capability, which is not the same as the lowest-numbered /dev/video node: most webcams also
	// register a metadata node that produces no images at all. An empty mode means the camera's best, which
	// is what this platform wants: resolution is bytes per frame and frame rate is frames per second, and
	// their product is the transfer rate.
	//
	// Reloadable, so that choosing a camera from the settings page does not need a restart.
	Device string  `yaml:"device"`
	Format string  `yaml:"format"`
	Width  int     `yaml:"width"`
	Height int     `yaml:"height"`
	FPS    float64 `yaml:"fps"`

	// DecodeWorkers is how many captured frames are decoded at once. Zero means one per core, less one.
	//
	// It is the single most important setting for keeping up with a fast display. Decoding is what the
	// receiver spends its time on — hundreds of milliseconds a frame at a dense geometry — and it is
	// per-frame work that shares nothing, so it scales with cores almost exactly. A display running faster
	// than the receiver can decode does not transfer more bytes; the surplus frames are photographed and
	// thrown away, at the cost of a write each.
	DecodeWorkers int `yaml:"decode_workers"`

	// Lanes is how many frames the sender tiles onto one display, and therefore how many this
	// receiver looks for in each photograph.
	//
	// One means the ordinary single-frame path. Above one, every capture is searched for that many
	// frames and each becomes its own result — without which a four-lane display would have three
	// quarters of every photograph discarded at the last step, having been rendered, displayed,
	// photographed and located.
	//
	// It costs a fiducial search per extra lane on captures that hold them, and one wasted search on
	// captures that do not, which is why it is configured rather than always on.
	Lanes int `yaml:"lanes"`

	// RGB describes the production camera, when there is one. See RGBCamera.
	RGB RGBCamera `yaml:"rgb"`

	// Simulate degrades every frame before it is decoded, as a lens and a sensor would: "clean", "typical",
	// "harsh", or "rolling-shutter". Empty means frames are read exactly as they were written.
	//
	// It exists for the file-backed source, where there is no optics at all — and without it a file-to-file
	// deployment proves only that the decoder can read the encoder's own output, which is the easiest
	// possible case. Naming a profile makes the virtual channel behave like a real one, so a demonstration
	// or a soak test exercises the tolerances a camera actually will.
	Simulate string `yaml:"simulate"`
}

// RGBCamera describes an industrial colour camera photographing the display.
//
// It exists because the production channel is not the development one. A phone or a webcam is aimed by
// hand and reports nothing about itself; a Basler, FLIR or Allied Vision camera on 10GigE or CoaXPress is
// installed once, and everything about whether it will work is decided before it is bought — sensor
// resolution against panel size against how many cells the sender is asked to put on screen.
//
// So these are not settings that drive a driver. This receiver does not open a GigE Vision camera: those
// cameras are reached through their vendors' SDKs, and the frames arrive here the same way the browser's
// do, by POST to /api/v1/capture/frames from a small grabber process that owns the SDK. What this block is
// for is to write down what that camera *is*, so the receiver can answer the question the hardware choice
// actually turns on — see Feasibility, which reports pixels per cell and the largest grid this camera can
// resolve, before anybody orders anything.
//
// Every field is descriptive. Setting them changes no capture behaviour; getting them wrong changes only
// the advice.
type RGBCamera struct {
	// Enabled marks this deployment as using a production camera. Off by default, because the answer for a
	// laptop and a phone is "no" and a planner that assumed otherwise would report on hardware nobody has.
	Enabled bool `yaml:"enabled"`

	// Model is free text for the operator's own records — "Basler ace 2 a2A2448-120cc".
	Model string `yaml:"model"`

	// SensorWidth and SensorHeight are the camera's full resolution in pixels. These are the numbers the
	// feasibility answer turns on, and the reason a 5 MP sensor is not five megapixels of display: a 16:9
	// panel inside a 4:3-ish sensor is limited by width, and the rest of the sensor sees the room.
	SensorWidth  int `yaml:"sensor_width"`
	SensorHeight int `yaml:"sensor_height"`

	// FPS is the camera's frame rate, which should exceed the display's refresh so every displayed frame
	// gets more than one chance to be photographed.
	FPS float64 `yaml:"fps"`

	// ExposureMicros is the shutter time. Short is what avoids motion and refresh smear; under a
	// millisecond is the usual target, and it is the setting that trades against lighting.
	ExposureMicros float64 `yaml:"exposure_micros"`

	// PixelFormat is the camera's output format, "RGB8", "RGB10", "RGB12", "BayerRG8" and so on.
	//
	// Worth recording because a colour payload lives or dies on it. A Bayer format debayered in the camera
	// or the grabber has had its colour interpolated from neighbouring photosites, which is the same loss
	// JPEG chroma subsampling inflicts and for the same reason — see the receiver's capture format setting.
	PixelFormat string `yaml:"pixel_format"`

	// Interface is how the camera is attached: "10GigE", "5GigE", "USB3", "CoaXPress".
	Interface string `yaml:"interface"`

	// TriggerMode is "free-run" or "hardware". Hardware trigger synchronised to the display refresh is what
	// stops a capture straddling two displayed frames, which is a frame lost for a reason no amount of
	// aiming fixes.
	TriggerMode string `yaml:"trigger_mode"`

	// PanelWidth and PanelHeight are the display being photographed, in pixels. Together with the sensor
	// they decide how many camera pixels land on one of the sender's cells.
	PanelWidth  int `yaml:"panel_width"`
	PanelHeight int `yaml:"panel_height"`

	// PanelFill is how much of the camera's shorter dimension the panel occupies, 0..1. One means the panel
	// exactly fills the frame, which no real installation achieves — leave margin for the fiducials to stay
	// in shot when something is nudged.
	PanelFill float64 `yaml:"panel_fill"`
}

// Peer is where the other side of the gap can be reached by a human.
//
// Not by this process: the two applications share a protocol and a directory and nothing else, and that
// separation is the point. This is a URL for an operator's browser, so that a received file can be put next
// to the one that was sent. Both sides address a transfer by the same transmission id, which is what makes a
// one-click comparison possible at all.
//
// Empty by default, because on a real air-gapped installation the sender's interface may not be reachable
// from the receiver's network — and a link that cannot work is worse than no link.
type Peer struct {
	// SenderUIURL is the origin the sender's browser app is served from, such as http://sender.local:8080.
	SenderUIURL string `yaml:"sender_ui_url"`
}

// Decoder configures how frames are read.
type Decoder struct {
	// CellPixelsHint is the sender's configured cell size. It is only used to size the sampling
	// window when the grid descriptor cannot be read, which is the one case where the decoder has
	// nothing measured to work from. Zero means no hint.
	CellPixelsHint int `yaml:"cell_pixels_hint"`

	// MinFinderScore and MinTimingScore are the confidence floors below which a located geometry is
	// rejected before its payload is read. They are the receiver's own policy rather than the
	// protocol's: the decoder reports how well it matched, and how much confidence is enough is a
	// judgement about this installation. Reloadable, because it is what an operator adjusts while
	// watching a marginal camera.
	MinFinderScore float64 `yaml:"min_finder_score"`
	MinTimingScore float64 `yaml:"min_timing_score"`

	// EncryptionKeyHex decrypts payloads, and must match the sender's. Sixty-four hex characters.
	EncryptionKeyHex string `yaml:"encryption_key_hex"`

	// Recovery configures the soft-decision retry applied to a frame whose geometry resolved and
	// whose payload did not verify.
	//
	// On by default. It costs nothing on a healthy channel — a frame that decodes never reaches it
	// — and on a marginal one it is the difference between a frame that missed its checksum by
	// three cells being recovered and being discarded outright.
	Recovery Recovery `yaml:"recovery"`
}

// Recovery bounds the soft-decision search. See receiver/ai/soft for what it does.
type Recovery struct {
	Enabled bool `yaml:"enabled"`

	// MaxCells is how many of the least confident cells may be corrected; the search space is
	// 2^MaxCells. MaxCandidates caps the corrections actually tried.
	MaxCells      int `yaml:"max_cells"`
	MaxCandidates int `yaml:"max_candidates"`

	// Budget is the wall-clock ceiling on one frame's search, and it is the setting that keeps this
	// safe across the sender's whole grid range.
	//
	// A candidate hashes the entire payload, so its cost grows with the grid: exhausting the
	// candidate cap costs about 50 ms at grid 80 and would cost seconds at grid 1024. An operator
	// who raises the grid should not have to discover that they have also lengthened every failed
	// decode by two orders of magnitude, so the bound is expressed in time rather than in attempts.
	//
	// It is not an absolute ceiling. Sampling the payload region has to happen before any candidate
	// can be tried, and at a large grid that sampling alone exceeds any sane budget — so a floor of
	// candidates always runs once the read is done rather than paying for it and returning nothing.
	// See receiver/ai/soft for the measurements.
	Budget time.Duration `yaml:"budget"`

	// Engine names which implementation reads rejected frames: "go" for the deterministic candidate
	// search, "sidecar" for a model server with that search in front of it, "none" for nothing.
	//
	// Named rather than inferred from whether a URL is set, because "I configured a model server and
	// got the baseline" must not be a thing that can happen quietly. A sidecar named and unreachable
	// stops the receiver at startup instead.
	Engine string `yaml:"engine"`

	// SidecarURL and SidecarTimeout configure the model server. The timeout bounds one enhance call,
	// so a stalled service cannot stall the decode path with it.
	SidecarURL     string        `yaml:"sidecar_url"`
	SidecarTimeout time.Duration `yaml:"sidecar_timeout"`
}

// Settings converts the configuration into what the engine package needs.
//
// The translation lives here rather than in the engine package so that the engine tree depends on
// nothing of the receiver's — which is what lets a benchmark or a corpus analysis build an engine
// without a configuration file to hand.
func (r Recovery) Settings() engine.Settings {
	return engine.Settings{
		Enabled:        r.Enabled,
		Engine:         r.Engine,
		SidecarURL:     r.SidecarURL,
		SidecarTimeout: r.SidecarTimeout,
		Search: soft.Options{
			MaxCells:      r.MaxCells,
			MaxCandidates: r.MaxCandidates,
			Budget:        r.Budget,
		},
	}
}

// Ack configures the acknowledgement channel.
type Ack struct {
	// Dir is the shared directory acknowledgements are written into.
	Dir string `yaml:"dir"`

	// Secret signs them. Both applications must hold the same value.
	Secret string `yaml:"secret"`
}

// Callback configures delivery of merged files.
type Callback struct {
	// AllowedHosts is the set of hosts a merged file may be delivered to.
	//
	// This is the receiver's defence against being used as a request forgery proxy. The callback URL
	// arrives over the optical channel, from outside this machine's trust boundary, and is then
	// turned into an outbound request — so without an allowlist, whoever controls the sender
	// chooses what the receiver connects to, including addresses inside the receiver's own network
	// that nothing outside it should be able to reach.
	AllowedHosts []string `yaml:"allowed_hosts"`

	// AllowAnyHost disables that check.
	//
	// It exists because a closed deployment where the sender is as trusted as the receiver has no
	// need of the allowlist, and forcing one there would be ceremony. It is off by default, and a
	// receiver that turns it on is accepting that its sender can direct it anywhere.
	AllowAnyHost bool `yaml:"allow_any_host"`

	// Timeout bounds one delivery attempt, and RetryDelay is how long before the next.
	Timeout    time.Duration `yaml:"timeout"`
	RetryDelay time.Duration `yaml:"retry_delay"`

	// MaxAttempts is how many times a delivery is tried before it is abandoned.
	MaxAttempts int `yaml:"max_attempts"`

	// MaxBodyBytes bounds what will be posted, so a receiver cannot be made to stream an
	// unbounded body at a third party.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

// Auth configures authentication.
type Auth struct {
	JWTSecret string        `yaml:"jwt_secret"`
	TokenTTL  time.Duration `yaml:"token_ttl"`
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// Default returns the configuration a deployment gets before any file or environment variable.
func Default() Config {
	return Config{
		Server: Server{
			Host:          "0.0.0.0",
			Port:          8081,
			ReadTimeout:   time.Minute,
			WriteTimeout:  5 * time.Minute,
			ShutdownGrace: 30 * time.Second,
			CORSOrigins:   []string{"http://localhost:5174"},
		},
		Database: Database{
			URL:             "postgres://otp:otp@localhost:5432/otp_receiver?sslmode=disable",
			MaxConns:        16,
			MinConns:        2,
			MaxConnLifetime: time.Hour,
			MaxConnIdleTime: 30 * time.Minute,
			ConnectTimeout:  10 * time.Second,
			MigrateOnStart:  true,
		},
		Storage: Storage{
			Backend: "filesystem",
			Root:    "/var/lib/otp/receiver",
			MinIO:   MinIO{Bucket: "otp-receiver", Region: "us-east-1"},
		},
		Capture: Capture{
			Source:       "browser",
			Dir:          "/var/lib/otp/shared/frames",
			Consume:      true,
			IdleInterval: 100 * time.Millisecond,
			// A twelfth, unchanged from when this was a constant: the same behaviour for every deployment that
			// does not ask for something else.
			MinToneFraction: 0,
			// Four, matching the sender's default tiling. A capture holding one frame costs one extra
			// fiducial search and behaves identically, so this is safe when the sender is not tiling.
			Lanes: 4,
			RGB: RGBCamera{
				// Off, because the answer for a laptop and a phone is "no" and a planner reporting on
				// hardware nobody owns is worse than one that says nothing.
				Enabled: false,
				// The panel a production install is specified against, so the feasibility answer means
				// something the moment the camera is filled in.
				PanelWidth:  3840,
				PanelHeight: 2160,
				// Not one. A panel filling the frame exactly leaves no room for the fiducials to survive a
				// nudge, and every real installation is framed looser than the specification imagines.
				PanelFill:   0.9,
				TriggerMode: "hardware",
				PixelFormat: "RGB8",
			},
		},
		Decoder: Decoder{
			MinFinderScore: 0.75,
			MinTimingScore: 0.6,
			Recovery: Recovery{
				Enabled:        true,
				MaxCells:       12,
				MaxCandidates:  4096,
				Budget:         50 * time.Millisecond,
				Engine:         "go",
				SidecarTimeout: 2 * time.Second,
			},
		},
		Ack: Ack{
			Dir: "/var/lib/otp/shared",
		},
		Callback: Callback{
			Timeout:      30 * time.Second,
			RetryDelay:   10 * time.Second,
			MaxAttempts:  8,
			MaxBodyBytes: 8 << 30,
		},
		Auth: Auth{TokenTTL: 12 * time.Hour},
		Log:  Log{Level: "info", Format: "json"},
		Metrics: Metrics{
			Enabled: true,
			Path:    "/metrics",
		},
	}
}

// ErrInvalid means the configuration cannot be used.
var ErrInvalid = errors.New("config: invalid")

// Load reads configuration from a file, applies environment overrides, and validates it.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return Config{}, fmt.Errorf("config: %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
		default:
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Addr is the listen address.
func (c Config) Addr() string { return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port) }

// EncryptionKey returns the configured payload key, or nil when payloads arrive in the clear.
func (c Config) EncryptionKey() []byte {
	if c.Decoder.EncryptionKeyHex == "" {
		return nil
	}
	key, err := hex.DecodeString(c.Decoder.EncryptionKeyHex)
	if err != nil {
		return nil
	}
	return key
}

// LocateOptions is the decoder configuration the protocol package takes.
func (c Config) LocateOptions() protocol.LocateOptions {
	return protocol.LocateOptions{CellPixelsHint: c.Decoder.CellPixelsHint}
}

// Validate checks the configuration describes a receiver that can work.
func (c Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		add("server.port %d is not a port", c.Server.Port)
	}
	if c.Database.URL == "" {
		add("database.url is required")
	}
	if c.Database.MaxConns < 1 {
		add("database.max_conns must be at least 1")
	}

	switch c.Storage.Backend {
	case "filesystem":
		if c.Storage.Root == "" {
			add("storage.root is required for the filesystem backend")
		}
	case "minio":
		if c.Storage.MinIO.Endpoint == "" || c.Storage.MinIO.Bucket == "" {
			add("storage.minio needs an endpoint and a bucket")
		}
	default:
		add("storage.backend %q is not one of filesystem, minio", c.Storage.Backend)
	}

	switch c.Capture.Source {
	case "file":
		if c.Capture.Dir == "" {
			add("capture.dir is required for the file source")
		}
	case "browser":
		// Frames posted by a browser holding the camera. Nothing to validate: it needs no device, because the
		// page owns the camera and the operating system asked its own permission.
	case "camera":
		// A real camera through Video4Linux. The device may be empty: the receiver then picks the
		// lowest-numbered one that actually declares video capture, in its largest mode, so a machine with a
		// camera attached needs no configuration at all.
	case "gocv":
		// Availability is a build-tag matter, reported by the source itself at startup.
	default:
		add("capture.source %q is not one of file, browser, camera, gocv", c.Capture.Source)
	}
	if c.Capture.IdleInterval <= 0 {
		add("capture.idle_interval must be positive")
	}
	switch c.Capture.Simulate {
	case "", "clean", "typical", "harsh", "rolling-shutter":
	default:
		add("capture.simulate %q is not one of clean, typical, harsh, rolling-shutter", c.Capture.Simulate)
	}

	if c.Decoder.CellPixelsHint < 0 {
		add("decoder.cell_pixels_hint cannot be negative")
	}
	for name, value := range map[string]float64{
		"decoder.min_finder_score": c.Decoder.MinFinderScore,
		"decoder.min_timing_score": c.Decoder.MinTimingScore,
	} {
		if value < 0 || value > 1 {
			add("%s %v must be between 0 and 1", name, value)
		}
	}
	if c.Decoder.Recovery.Enabled {
		// Twenty is already 2^20 subsets, far past any budget; the cap is here so a typo cannot ask
		// for a search that would never finish.
		if c.Decoder.Recovery.MaxCells < 1 || c.Decoder.Recovery.MaxCells > 20 {
			add("decoder.recovery.max_cells %d is outside 1..20; the search space is 2^n",
				c.Decoder.Recovery.MaxCells)
		}
		if c.Decoder.Recovery.MaxCandidates < 1 {
			add("decoder.recovery.max_candidates %d must be at least 1",
				c.Decoder.Recovery.MaxCandidates)
		}
		// Zero is valid and means no time bound, leaving max_candidates as the only limit. Negative
		// is not: it would silently disable the search on the first candidate.
		if c.Decoder.Recovery.Budget < 0 {
			add("decoder.recovery.budget %s cannot be negative", c.Decoder.Recovery.Budget)
		}
		switch c.Decoder.Recovery.Engine {
		case "", "go", "none":
		case "sidecar", "classifier":
			if c.Decoder.Recovery.SidecarURL == "" {
				add("decoder.recovery.engine is %s but no sidecar_url is set", c.Decoder.Recovery.Engine)
			}
		default:
			add("decoder.recovery.engine %q is not one of %v",
				c.Decoder.Recovery.Engine, engine.AvailableEngines())
		}
	}
	if c.Decoder.EncryptionKeyHex != "" {
		key, err := hex.DecodeString(c.Decoder.EncryptionKeyHex)
		switch {
		case err != nil:
			add("decoder.encryption_key_hex is not hexadecimal")
		case len(key) != protocol.KeySize:
			add("decoder.encryption_key_hex decodes to %d bytes, and must be %d",
				len(key), protocol.KeySize)
		}
	}

	if c.Ack.Dir == "" {
		add("ack.dir is required")
	}
	if c.Ack.Secret == "" {
		// The same reasoning as on the sender, from the other end: an unsigned acknowledgement
		// channel lets anything able to write that directory tell the sender whatever it likes about
		// what arrived.
		add("ack.secret is required and has no default")
	}

	if c.Callback.Timeout <= 0 {
		add("callback.timeout must be positive")
	}
	if c.Callback.MaxAttempts < 1 {
		add("callback.max_attempts must be at least 1")
	}
	if c.Callback.MaxBodyBytes <= 0 {
		add("callback.max_body_bytes must be positive")
	}
	if !c.Callback.AllowAnyHost && len(c.Callback.AllowedHosts) == 0 {
		// Not an error: a receiver with no allowlist and no override simply delivers nothing, which
		// is the safe default. It is worth saying out loud though, because a deployment that
		// expected deliveries would otherwise see them silently refused.
		problems = append(problems,
			"callback.allowed_hosts is empty and callback.allow_any_host is false, so no merged file will be delivered anywhere")
	}

	if c.Auth.JWTSecret == "" {
		add("auth.jwt_secret is required and has no default")
	} else if len(c.Auth.JWTSecret) < 32 {
		add("auth.jwt_secret is %d bytes; HS256 tokens need at least 32", len(c.Auth.JWTSecret))
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level %q is not one of debug, info, warn, error", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "console":
	default:
		add("log.format %q is not one of json, console", c.Log.Format)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  - %s", ErrInvalid, strings.Join(problems, "\n  - "))
}

const envPrefix = "OTP_RECEIVER_"

// applyEnv overlays environment variables onto a configuration.
func applyEnv(c *Config) error {
	var errs []string
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			*dst = v
		}
	}
	integer := func(key string, dst *int) {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s%s: %s is not a number", envPrefix, key, v))
				return
			}
			*dst = n
		}
	}
	integer64 := func(key string, dst *int64) {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s%s: %s is not a number", envPrefix, key, v))
				return
			}
			*dst = n
		}
	}
	float := func(key string, dst *float64) {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s%s: %s is not a number", envPrefix, key, v))
				return
			}
			*dst = f
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s%s: %s is not a boolean", envPrefix, key, v))
				return
			}
			*dst = b
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s%s: %s is not a duration", envPrefix, key, v))
				return
			}
			*dst = d
		}
	}
	list := func(key string, dst *[]string) {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			parts := strings.Split(v, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				if t := strings.TrimSpace(p); t != "" {
					out = append(out, t)
				}
			}
			*dst = out
		}
	}

	str("HOST", &c.Server.Host)
	integer("PORT", &c.Server.Port)
	list("CORS_ORIGINS", &c.Server.CORSOrigins)
	str("TLS_CERT_FILE", &c.Server.TLSCertFile)
	str("TLS_KEY_FILE", &c.Server.TLSKeyFile)

	str("DATABASE_URL", &c.Database.URL)
	boolean("DB_MIGRATE_ON_START", &c.Database.MigrateOnStart)

	str("STORAGE_BACKEND", &c.Storage.Backend)
	str("STORAGE_ROOT", &c.Storage.Root)
	str("MINIO_ENDPOINT", &c.Storage.MinIO.Endpoint)
	str("MINIO_ACCESS_KEY", &c.Storage.MinIO.AccessKey)
	str("MINIO_SECRET_KEY", &c.Storage.MinIO.SecretKey)
	str("MINIO_BUCKET", &c.Storage.MinIO.Bucket)
	boolean("MINIO_USE_SSL", &c.Storage.MinIO.UseSSL)
	str("MINIO_REGION", &c.Storage.MinIO.Region)

	str("CAPTURE_SOURCE", &c.Capture.Source)
	str("CAPTURE_DIR", &c.Capture.Dir)
	boolean("CAPTURE_CONSUME", &c.Capture.Consume)
	dur("CAPTURE_IDLE_INTERVAL", &c.Capture.IdleInterval)
	float("CAPTURE_MIN_TONE_FRACTION", &c.Capture.MinToneFraction)
	boolean("CAPTURE_RETAIN_FRAMES", &c.Capture.RetainFrames)
	str("CAPTURE_SIMULATE", &c.Capture.Simulate)
	str("PEER_SENDER_UI_URL", &c.Peer.SenderUIURL)
	str("CAPTURE_DEVICE", &c.Capture.Device)
	str("CAPTURE_FORMAT", &c.Capture.Format)
	integer("CAPTURE_LANES", &c.Capture.Lanes)
	boolean("CAPTURE_RGB_ENABLED", &c.Capture.RGB.Enabled)
	str("CAPTURE_RGB_MODEL", &c.Capture.RGB.Model)
	integer("CAPTURE_RGB_SENSOR_WIDTH", &c.Capture.RGB.SensorWidth)
	integer("CAPTURE_RGB_SENSOR_HEIGHT", &c.Capture.RGB.SensorHeight)
	float("CAPTURE_RGB_FPS", &c.Capture.RGB.FPS)
	float("CAPTURE_RGB_EXPOSURE_MICROS", &c.Capture.RGB.ExposureMicros)
	str("CAPTURE_RGB_PIXEL_FORMAT", &c.Capture.RGB.PixelFormat)
	str("CAPTURE_RGB_INTERFACE", &c.Capture.RGB.Interface)
	str("CAPTURE_RGB_TRIGGER_MODE", &c.Capture.RGB.TriggerMode)
	integer("CAPTURE_RGB_PANEL_WIDTH", &c.Capture.RGB.PanelWidth)
	integer("CAPTURE_RGB_PANEL_HEIGHT", &c.Capture.RGB.PanelHeight)
	float("CAPTURE_RGB_PANEL_FILL", &c.Capture.RGB.PanelFill)
	integer("CAPTURE_DECODE_WORKERS", &c.Capture.DecodeWorkers)
	integer("CAPTURE_WIDTH", &c.Capture.Width)
	integer("CAPTURE_HEIGHT", &c.Capture.Height)
	float("CAPTURE_FPS", &c.Capture.FPS)

	integer("DECODER_CELL_PIXELS_HINT", &c.Decoder.CellPixelsHint)
	float("DECODER_MIN_FINDER_SCORE", &c.Decoder.MinFinderScore)
	float("DECODER_MIN_TIMING_SCORE", &c.Decoder.MinTimingScore)
	boolean("DECODER_RECOVERY_ENABLED", &c.Decoder.Recovery.Enabled)
	integer("DECODER_RECOVERY_MAX_CELLS", &c.Decoder.Recovery.MaxCells)
	integer("DECODER_RECOVERY_MAX_CANDIDATES", &c.Decoder.Recovery.MaxCandidates)
	dur("DECODER_RECOVERY_BUDGET", &c.Decoder.Recovery.Budget)
	str("DECODER_RECOVERY_ENGINE", &c.Decoder.Recovery.Engine)
	str("DECODER_RECOVERY_SIDECAR_URL", &c.Decoder.Recovery.SidecarURL)
	dur("DECODER_RECOVERY_SIDECAR_TIMEOUT", &c.Decoder.Recovery.SidecarTimeout)
	str("ENCRYPTION_KEY_HEX", &c.Decoder.EncryptionKeyHex)

	str("ACK_DIR", &c.Ack.Dir)
	str("ACK_SECRET", &c.Ack.Secret)

	list("CALLBACK_ALLOWED_HOSTS", &c.Callback.AllowedHosts)
	boolean("CALLBACK_ALLOW_ANY_HOST", &c.Callback.AllowAnyHost)
	dur("CALLBACK_TIMEOUT", &c.Callback.Timeout)
	dur("CALLBACK_RETRY_DELAY", &c.Callback.RetryDelay)
	integer("CALLBACK_MAX_ATTEMPTS", &c.Callback.MaxAttempts)
	integer64("CALLBACK_MAX_BODY_BYTES", &c.Callback.MaxBodyBytes)

	str("JWT_SECRET", &c.Auth.JWTSecret)
	dur("TOKEN_TTL", &c.Auth.TokenTTL)

	str("LOG_LEVEL", &c.Log.Level)
	str("LOG_FORMAT", &c.Log.Format)
	boolean("METRICS_ENABLED", &c.Metrics.Enabled)
	str("METRICS_PATH", &c.Metrics.Path)

	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Watcher holds the live configuration.
//
// The receiver's reloadable subset is smaller than the sender's: the log level, how long to wait
// on an idle channel, and the two decoder confidence floors. The floors are the interesting ones —
// they are what an operator adjusts while watching a marginal camera, and needing a restart to try
// a different threshold would make tuning a capture almost impossible.
type Watcher struct {
	path    string
	current atomic.Pointer[Config]
}

// NewWatcher returns a watcher over an already-loaded configuration.
func NewWatcher(path string, initial Config) *Watcher {
	w := &Watcher{path: path}
	w.current.Store(&initial)
	return w
}

// Current returns the configuration as it stands.
func (w *Watcher) Current() Config { return *w.current.Load() }

// Reload re-reads the file and applies the reloadable subset.
func (w *Watcher) Reload() error {
	if w.path == "" {
		return nil
	}
	loaded, err := Load(w.path)
	if err != nil {
		return err
	}

	next := w.Current()
	next.Log.Level = loaded.Log.Level
	next.Capture.IdleInterval = loaded.Capture.IdleInterval
	next.Decoder.MinFinderScore = loaded.Decoder.MinFinderScore
	next.Decoder.MinTimingScore = loaded.Decoder.MinTimingScore
	// Reloadable for the same reason the floors are: these are what an operator adjusts while
	// watching a marginal camera, and needing a restart to try a wider search defeats the point.
	next.Decoder.Recovery = loaded.Decoder.Recovery
	w.current.Store(&next)
	return nil
}

// SetSource records the capture source now in use.
//
// Called after the source has actually been swapped, not before — so what the API reports is what the receiver
// is really reading from. Reporting an intention as a fact is how an operator comes to believe a camera is
// running when it is not.
func (w *Watcher) SetSource(source string) Config {
	next := w.Current()
	next.Capture.Source = source
	w.current.Store(&next)
	return next
}

// SetCamera applies a camera selection without a restart.
//
// It is a method rather than a reload because the choice does not come from the configuration file: it
// comes from an operator clicking a device in a list, and the file is their document to edit rather than
// this service's to rewrite. Persistence is handled separately, by the caller, in a file of its own.
//
// The device fields are the only ones this may touch. Everything else in Capture either cannot change
// while a session is open or is not the camera's business.
func (w *Watcher) SetCamera(device, format string, width, height int, fps float64) Config {
	next := w.Current()
	next.Capture.Device = device
	next.Capture.Format = format
	next.Capture.Width = width
	next.Capture.Height = height
	next.Capture.FPS = fps
	w.current.Store(&next)
	return next
}

// SetMinToneFraction applies the blank-screen threshold without a restart.
//
// A method for the same reason as SetCamera: the value comes from an operator adjusting a control while aiming a
// camera, not from the configuration file, and the file is theirs to edit rather than this service's to rewrite.
// Deliberately live-only — the configured value in .env or sender.yaml is what a restart comes back to, which is
// the right shape for a debugging override. Unlike the sender's display sink, this setting takes effect on the
// very next frame, so there is no restart for the change to be lost across.
//
// Clamped rather than validated, because a caller cannot usefully be told off here: this is reached from a
// slider, and a negative value has an obvious meaning — turn the gate off.
// SetRGBCamera records what the production camera is, taking effect immediately.
//
// Live rather than persisted, matching the capture gate beside it: the configured value is what a restart
// returns to. That is the right shape here because nothing depends on it surviving — the description drives
// no capture, so a value lost on restart costs an operator the planner's answer and nothing else. A
// deployment that wants it durable sets it in the environment, where it belongs alongside the panel it
// describes.
func (w *Watcher) SetRGBCamera(camera RGBCamera) Config {
	next := w.Current()
	next.Capture.RGB = camera
	w.current.Store(&next)
	return next
}

func (w *Watcher) SetMinToneFraction(fraction float64) Config {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 0.5 {
		// Above a half is unsatisfiable: dark and light are disjoint, so both cannot exceed 50% of the samples.
		fraction = 0.5
	}
	next := w.Current()
	next.Capture.MinToneFraction = fraction
	w.current.Store(&next)
	return next
}
