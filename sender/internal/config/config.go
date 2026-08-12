// Package config loads the sender's configuration from YAML, applies environment
// overrides, and reloads the parts of it that can safely change while running.
//
// The distinction between what can and cannot be reloaded is the whole design. A
// running transmission has state that configuration participates in: the grid geometry
// is written into every frame's header, the chunk size was derived from it and the
// chunks already exist, the database pool has open connections. Changing any of those
// under a live transmission does not reconfigure it, it corrupts it. So the reloadable
// subset is enumerated explicitly and everything else takes effect only on restart —
// and an operator who edits a non-reloadable field is told so rather than left to
// wonder why nothing happened.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the sender's complete configuration.
type Config struct {
	Server    Server    `yaml:"server"`
	Database  Database  `yaml:"database"`
	Storage   Storage   `yaml:"storage"`
	Broker    Broker    `yaml:"broker"`
	Jobs      Jobs      `yaml:"jobs"`
	Optical   Optical   `yaml:"optical"`
	Display   Display   `yaml:"display"`
	Ack       Ack       `yaml:"ack"`
	Auth      Auth      `yaml:"auth"`
	Retention Retention `yaml:"retention"`
	Log       Log       `yaml:"log"`
	Metrics   Metrics   `yaml:"metrics"`
	Tracing   Tracing   `yaml:"tracing"`
}

// Server is the HTTP listener.
type Server struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	// ReadTimeout and WriteTimeout bound a request. Uploads are large, so the write
	// timeout is generous and the read timeout more so.
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`

	// ShutdownGrace is how long in-flight requests have to finish when the process is
	// asked to stop.
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`

	// MaxUploadBytes bounds a single uploaded file.
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`

	// TLSCertFile and TLSKeyFile enable TLS in-process. Empty means plain HTTP, which
	// is correct behind a proxy that terminates TLS itself.
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`

	// CORSOrigins lists the origins the browser UI may be served from.
	CORSOrigins []string `yaml:"cors_origins"`
}

// Database is the Postgres connection.
type Database struct {
	// URL is the full connection string. It is the only way to configure the database,
	// rather than a host/port/user/password set, so a password never has to appear in
	// more than one place.
	URL string `yaml:"url"`

	MaxConns        int32         `yaml:"max_conns"`
	MinConns        int32         `yaml:"min_conns"`
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime"`
	MaxConnIdleTime time.Duration `yaml:"max_conn_idle_time"`
	ConnectTimeout  time.Duration `yaml:"connect_timeout"`

	// MigrateOnStart applies pending migrations at startup. It is on by default for
	// single-instance deployments and worth turning off where a deployment pipeline
	// runs migrations as its own step.
	MigrateOnStart bool `yaml:"migrate_on_start"`
}

// Storage selects and configures the object store.
type Storage struct {
	// Backend is "filesystem" or "minio".
	Backend string `yaml:"backend"`

	// Root is the filesystem backend's base directory.
	Root string `yaml:"root"`

	// MinIO configures the S3-compatible backend.
	MinIO MinIO `yaml:"minio"`
}

// MinIO is the S3-compatible object store's settings.
type MinIO struct {
	Endpoint  string `yaml:"endpoint"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
	UseSSL    bool   `yaml:"use_ssl"`
	Region    string `yaml:"region"`
}

// Broker selects and configures job dispatch.
type Broker struct {
	// Backend is "internal" or "rabbitmq".
	Backend string `yaml:"backend"`

	RabbitMQ RabbitMQ `yaml:"rabbitmq"`
}

// RabbitMQ is the external broker's settings.
type RabbitMQ struct {
	URL       string `yaml:"url"`
	Exchange  string `yaml:"exchange"`
	Queue     string `yaml:"queue"`
	Prefetch  int    `yaml:"prefetch"`
	Mandatory bool   `yaml:"mandatory"`
}

// Jobs configures the worker pool.
type Jobs struct {
	// Concurrency is how many jobs run at once. Reloadable.
	Concurrency int `yaml:"concurrency"`

	// PollInterval is how often a worker looks for claimable work when it found none
	// last time.
	PollInterval time.Duration `yaml:"poll_interval"`

	// ClaimTimeout is how long a claimed job may run before another worker may take it
	// over, on the assumption its owner died.
	ClaimTimeout time.Duration `yaml:"claim_timeout"`

	// MaxAttempts is how many times a failing job is retried, and BackoffBase the
	// delay before the first retry; each subsequent wait doubles.
	MaxAttempts int           `yaml:"max_attempts"`
	BackoffBase time.Duration `yaml:"backoff_base"`
	BackoffMax  time.Duration `yaml:"backoff_max"`

	// RetentionDays is how long finished jobs and their logs are kept.
	RetentionDays int `yaml:"retention_days"`
}

// Optical is the frame geometry and encoding profile defaults.
type Optical struct {
	// Encoder, BitDepth, Compression, and Level name the profile a new transmission
	// uses when the request does not choose one.
	//
	// BitDepth is zero by default, meaning whichever depth the chosen encoder prefers.
	// A concrete default would be a trap: it can only be right for one encoder, so an
	// operator who set nothing but `encoder: binary` would be told their bit depth was
	// invalid — a field they never touched, rejecting a change they made correctly.
	Encoder     string `yaml:"encoder"`
	BitDepth    int    `yaml:"bit_depth"`
	Compression string `yaml:"compression"`
	Level       int    `yaml:"level"`

	// FEC is the error-correction profile.
	FEC FEC `yaml:"fec"`

	// GridWidth, GridHeight, CellPixels, and QuietZone are the frame geometry. They
	// are not reloadable: they are written into every frame header, and the chunk size
	// is derived from them, so changing them mid-transmission would produce frames the
	// receiver assembles into the wrong file.
	GridWidth  int `yaml:"grid_width"`
	GridHeight int `yaml:"grid_height"`
	CellPixels int `yaml:"cell_pixels"`
	QuietZone  int `yaml:"quiet_zone"`

	// CameraShortSidePixels is the short side, in pixels, of the picture the receiving camera takes.
	//
	// The sender cannot measure it — the camera is on the other side of an air gap — so it is configured,
	// and it exists to answer one question before a transfer is spent rather than after: can the geometry
	// this transfer is about to commit to actually be read?
	//
	// A frame is square, so its width on the sensor is bounded by the short side of the picture. Divide
	// that by the grid plus its quiet zone and you have the most pixels per cell that camera can ever
	// produce, at any distance, with perfect aim. Measured, a 128-cell grid on a 1080-wide capture tops
	// out at 8.2 against the 10 colour8 needs — and the transfer failed after sending chunk 0 eleven times,
	// which is an expensive way to learn arithmetic.
	//
	// Zero disables the check, which is right for a file-loopback channel where there is no camera at all.
	CameraShortSidePixels int `yaml:"camera_short_side_pixels"`

	// CameraLongSidePixels is the other dimension of that picture.
	//
	// Both are needed rather than just the short one, because the difference between them is the single
	// cheapest fix available to an operator. A square frame is bounded by whichever side is shorter, so
	// turning the camera ninety degrees can be worth half again as many pixels per cell — measured, 8.2
	// against 14.5 at a 128-cell grid — without moving, reconfiguring or resending anything. Knowing only
	// the short side, the advice cannot be given at all.
	CameraLongSidePixels int `yaml:"camera_long_side_pixels"`

	// EncryptionKeyHex, when set, encrypts every payload. It is 64 hex characters.
	EncryptionKeyHex string `yaml:"encryption_key_hex"`

	// ManifestInterval re-emits the manifest every this many frames, so a receiver
	// that came online late can still join.
	ManifestInterval int `yaml:"manifest_interval"`
}

// FEC is the error-correction profile.
type FEC struct {
	Codec        string `yaml:"codec"`
	DataShards   int    `yaml:"data_shards"`
	ParityShards int    `yaml:"parity_shards"`
}

// Display configures the optical output.
type Display struct {
	// Sink is "none" for camera-only mode, "file", or "opengl" in a build that includes it.
	//
	// "none" is the default, and it is the protocol working as intended: nothing is written to the
	// shared directory, and the receiver watches the physical display with its own camera instead of
	// reading files off a volume. An optical transport that hands the receiver a file over a shared
	// mount has quietly stopped being one — the frames are real, the display is real, and the channel
	// between them is a directory. That is a useful thing to have for developing against, which is why
	// "file" exists, but defaulting to it means the ordinary way to run this system is the way that
	// bypasses it, and a deployment can transfer a file perfectly without the camera path ever having
	// been exercised.
	//
	// Not reloadable — the sink is opened once at process startup — so changing it here or through the
	// settings API takes effect on the next restart.
	Sink string `yaml:"sink"`

	// Dir is the file sink's output directory: the shared volume the receiver's file
	// camera source reads.
	Dir string `yaml:"dir"`

	// FPS is how many frames a second are displayed. Reloadable, because it is the
	// main knob an operator turns when the receiver reports it is falling behind.
	FPS float64 `yaml:"fps"`

	// Brightness and Gamma adjust the rendered frame for the panel in use. Both
	// reloadable: they are what an operator tunes while watching decode quality.
	Brightness float64 `yaml:"brightness"`
	Gamma      float64 `yaml:"gamma"`

	// WindowSize is how many unacknowledged chunks may be in flight. Reloadable.
	WindowSize int `yaml:"window_size"`

	// KeepAlive re-displays the oldest unacknowledged frame when the window is full
	// and nothing needs retransmitting, so the display never goes blank.
	KeepAlive bool `yaml:"keep_alive"`

	// RetainFrames keeps rendered frames on disk after a transmission finishes.
	RetainFrames bool `yaml:"retain_frames"`
}

// Ack configures the acknowledgement channel.
type Ack struct {
	// Dir is the shared directory the receiver writes acknowledgements into.
	Dir string `yaml:"dir"`

	// Secret signs and verifies records. Both applications must hold the same value.
	Secret string `yaml:"secret"`

	// PollInterval is the fallback for filesystem change notification, which is
	// unreliable across network filesystems — and a shared volume is very often one.
	PollInterval time.Duration `yaml:"poll_interval"`

	// Timeout is how long a chunk may go unacknowledged before it is retransmitted.
	Timeout time.Duration `yaml:"timeout"`

	// MaxRetries is how many times a chunk is retransmitted before the transmission
	// is failed. A chunk that will not arrive after this many tries is a fault an
	// operator needs to see, not something more attempts will fix.
	MaxRetries int `yaml:"max_retries"`
}

// Retention configures the sweep that deletes transfers which never completed.
//
// A transfer stuck at "pending" or "failed" still holds every chunk and frame it ever wrote,
// and nothing else in the sender ever revisits it — the pipeline moves forward or stops, it
// does not clean up after itself. Left alone, that is a slow, silent leak of storage for
// uploads nobody is coming back for. The sweep is what bounds it: anything older than MaxAge
// that never reached "completed" is reaped the same way an operator's DELETE request would
// reap it, on a fixed interval, without anyone having to ask.
type Retention struct {
	// Interval is how often the sweep runs.
	Interval time.Duration `yaml:"interval"`

	// MaxAge is how long a transfer may sit unfinished before it is deleted.
	MaxAge time.Duration `yaml:"max_age"`
}

// Auth configures authentication.
type Auth struct {
	// JWTSecret signs bearer tokens. There is no default: see Validate.
	JWTSecret string `yaml:"jwt_secret"`

	// TokenTTL is how long an issued token lasts.
	TokenTTL time.Duration `yaml:"token_ttl"`

	// BootstrapAdmin creates an initial administrator when the user table is empty,
	// so a fresh deployment can be logged into at all.
	BootstrapAdmin string `yaml:"bootstrap_admin"`
	BootstrapPass  string `yaml:"bootstrap_password"`
}

// Log configures structured logging.
type Log struct {
	// Level is one of debug, info, warn, error. Reloadable, and the reason the reload
	// mechanism exists at all: raising verbosity to diagnose a problem must not require
	// restarting the process the problem is happening in.
	Level string `yaml:"level"`

	// Format is "json" or "console".
	Format string `yaml:"format"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// Tracing configures OpenTelemetry export.
type Tracing struct {
	// Enabled turns on tracing. With it off, and with no endpoint configured, the
	// tracer is a no-op rather than absent, so instrumented code needs no branches.
	Enabled     bool    `yaml:"enabled"`
	Endpoint    string  `yaml:"endpoint"`
	ServiceName string  `yaml:"service_name"`
	SampleRatio float64 `yaml:"sample_ratio"`
	Insecure    bool    `yaml:"insecure"`
}

// Default returns the configuration a deployment gets before any file or environment
// variable is read.
//
// The defaults describe a working single-machine deployment over the virtual optical
// channel, because that is the configuration the platform can be verified in. The two
// secrets are left empty deliberately — see Validate.
func Default() Config {
	return Config{
		Server: Server{
			Host:           "0.0.0.0",
			Port:           8080,
			ReadTimeout:    15 * time.Minute,
			WriteTimeout:   5 * time.Minute,
			ShutdownGrace:  30 * time.Second,
			MaxUploadBytes: 32 << 30,
			CORSOrigins:    []string{"http://localhost:5173"},
		},
		Database: Database{
			URL:             "postgres://otp:otp@localhost:5432/otp_sender?sslmode=disable",
			MaxConns:        16,
			MinConns:        2,
			MaxConnLifetime: time.Hour,
			MaxConnIdleTime: 30 * time.Minute,
			ConnectTimeout:  10 * time.Second,
			MigrateOnStart:  true,
		},
		Storage: Storage{
			Backend: "filesystem",
			Root:    "/var/lib/otp/sender",
			MinIO: MinIO{
				Bucket: "otp-sender",
				Region: "us-east-1",
			},
		},
		Broker: Broker{
			Backend: "internal",
			RabbitMQ: RabbitMQ{
				Exchange: "otp",
				Queue:    "otp.sender.jobs",
				Prefetch: 8,
			},
		},
		Jobs: Jobs{
			Concurrency:   4,
			PollInterval:  time.Second,
			ClaimTimeout:  10 * time.Minute,
			MaxAttempts:   5,
			BackoffBase:   2 * time.Second,
			BackoffMax:    5 * time.Minute,
			RetentionDays: 30,
		},
		Optical: Optical{
			Encoder:     "color8",
			BitDepth:    0,
			Compression: "zstd",
			Level:       6,
			FEC: FEC{
				Codec:        "reed-solomon",
				DataShards:   32,
				ParityShards: 8,
			},
			GridWidth:  128,
			GridHeight: 128,
			CellPixels: 8,
			QuietZone:  2,
			// 1080, because the receiver browser capture is pinned to 1920x1080 and a square frame is
			// bounded by the short side however the device is held. Raise it for a higher-resolution
			// camera; zero disables the check, which is right for a file-loopback channel.
			CameraShortSidePixels: 1080,
			CameraLongSidePixels:  1920,
			ManifestInterval:      64,
		},
		Display: Display{
			Sink:       "none",
			Dir:        "/var/lib/otp/shared/frames",
			FPS:        10,
			Brightness: 0,
			Gamma:      1.0,
			WindowSize: 64,
			KeepAlive:  true,
		},
		Ack: Ack{
			Dir:          "/var/lib/otp/shared",
			PollInterval: 2 * time.Second,
			Timeout:      30 * time.Second,
			MaxRetries:   10,
		},
		Auth: Auth{
			TokenTTL:       12 * time.Hour,
			BootstrapAdmin: "admin",
		},
		Retention: Retention{
			Interval: time.Hour,
			MaxAge:   24 * time.Hour,
		},
		Log: Log{
			Level:  "info",
			Format: "json",
		},
		Metrics: Metrics{
			Enabled: true,
			Path:    "/metrics",
		},
		Tracing: Tracing{
			ServiceName: "otp-sender",
			SampleRatio: 0.1,
			Insecure:    true,
		},
	}
}

// Load reads configuration from a file, then applies environment overrides, then
// validates the result.
//
// A missing file is not an error: a container deployment configures everything through
// the environment, and requiring an empty file to be mounted alongside it would be
// pure ceremony. A file that exists but cannot be parsed *is* an error, because that
// means an operator wrote something and it is being ignored.
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
			// Fall through to the environment.
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

// TLSEnabled reports whether the server terminates TLS itself.
func (c Config) TLSEnabled() bool { return c.Server.TLSCertFile != "" && c.Server.TLSKeyFile != "" }

// FrameInterval is how long each frame is displayed for.
func (c Config) FrameInterval() time.Duration {
	if c.Display.FPS <= 0 {
		return time.Second
	}
	return time.Duration(float64(time.Second) / c.Display.FPS)
}

// envPrefix namespaces every override, so the sender and the receiver can be
// configured from one environment without colliding.
const envPrefix = "OTP_SENDER_"

// applyEnv overlays environment variables onto a configuration.
//
// Each variable is listed explicitly rather than derived from the struct by
// reflection. The mapping is part of the deployment interface — it appears in compose
// files and deployment manifests — so it should be greppable, and a field rename
// should not silently change the name of an environment variable somebody depends on.
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
	integer32 := func(key string, dst *int32) {
		n := int(*dst)
		integer(key, &n)
		*dst = int32(n)
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
	dur("READ_TIMEOUT", &c.Server.ReadTimeout)
	dur("WRITE_TIMEOUT", &c.Server.WriteTimeout)
	dur("SHUTDOWN_GRACE", &c.Server.ShutdownGrace)
	integer64("MAX_UPLOAD_BYTES", &c.Server.MaxUploadBytes)
	str("TLS_CERT_FILE", &c.Server.TLSCertFile)
	str("TLS_KEY_FILE", &c.Server.TLSKeyFile)
	list("CORS_ORIGINS", &c.Server.CORSOrigins)

	str("DATABASE_URL", &c.Database.URL)
	integer32("DB_MAX_CONNS", &c.Database.MaxConns)
	integer32("DB_MIN_CONNS", &c.Database.MinConns)
	dur("DB_MAX_CONN_LIFETIME", &c.Database.MaxConnLifetime)
	dur("DB_MAX_CONN_IDLE_TIME", &c.Database.MaxConnIdleTime)
	dur("DB_CONNECT_TIMEOUT", &c.Database.ConnectTimeout)
	boolean("DB_MIGRATE_ON_START", &c.Database.MigrateOnStart)

	str("STORAGE_BACKEND", &c.Storage.Backend)
	str("STORAGE_ROOT", &c.Storage.Root)
	str("MINIO_ENDPOINT", &c.Storage.MinIO.Endpoint)
	str("MINIO_ACCESS_KEY", &c.Storage.MinIO.AccessKey)
	str("MINIO_SECRET_KEY", &c.Storage.MinIO.SecretKey)
	str("MINIO_BUCKET", &c.Storage.MinIO.Bucket)
	boolean("MINIO_USE_SSL", &c.Storage.MinIO.UseSSL)
	str("MINIO_REGION", &c.Storage.MinIO.Region)

	str("BROKER_BACKEND", &c.Broker.Backend)
	str("RABBITMQ_URL", &c.Broker.RabbitMQ.URL)
	str("RABBITMQ_EXCHANGE", &c.Broker.RabbitMQ.Exchange)
	str("RABBITMQ_QUEUE", &c.Broker.RabbitMQ.Queue)
	integer("RABBITMQ_PREFETCH", &c.Broker.RabbitMQ.Prefetch)

	integer("JOB_CONCURRENCY", &c.Jobs.Concurrency)
	dur("JOB_POLL_INTERVAL", &c.Jobs.PollInterval)
	dur("JOB_CLAIM_TIMEOUT", &c.Jobs.ClaimTimeout)
	integer("JOB_MAX_ATTEMPTS", &c.Jobs.MaxAttempts)
	dur("JOB_BACKOFF_BASE", &c.Jobs.BackoffBase)
	dur("JOB_BACKOFF_MAX", &c.Jobs.BackoffMax)
	integer("JOB_RETENTION_DAYS", &c.Jobs.RetentionDays)

	str("ENCODER", &c.Optical.Encoder)
	integer("BIT_DEPTH", &c.Optical.BitDepth)
	str("COMPRESSION", &c.Optical.Compression)
	integer("COMPRESSION_LEVEL", &c.Optical.Level)
	str("FEC_CODEC", &c.Optical.FEC.Codec)
	integer("FEC_DATA_SHARDS", &c.Optical.FEC.DataShards)
	integer("FEC_PARITY_SHARDS", &c.Optical.FEC.ParityShards)
	integer("GRID_WIDTH", &c.Optical.GridWidth)
	integer("GRID_HEIGHT", &c.Optical.GridHeight)
	integer("CELL_PIXELS", &c.Optical.CellPixels)
	integer("QUIET_ZONE", &c.Optical.QuietZone)
	str("ENCRYPTION_KEY_HEX", &c.Optical.EncryptionKeyHex)
	integer("MANIFEST_INTERVAL", &c.Optical.ManifestInterval)

	str("DISPLAY_SINK", &c.Display.Sink)
	str("DISPLAY_DIR", &c.Display.Dir)
	float("DISPLAY_FPS", &c.Display.FPS)
	float("DISPLAY_BRIGHTNESS", &c.Display.Brightness)
	float("DISPLAY_GAMMA", &c.Display.Gamma)
	integer("DISPLAY_WINDOW_SIZE", &c.Display.WindowSize)
	boolean("DISPLAY_KEEP_ALIVE", &c.Display.KeepAlive)
	boolean("DISPLAY_RETAIN_FRAMES", &c.Display.RetainFrames)

	str("ACK_DIR", &c.Ack.Dir)
	str("ACK_SECRET", &c.Ack.Secret)
	dur("ACK_POLL_INTERVAL", &c.Ack.PollInterval)
	dur("ACK_TIMEOUT", &c.Ack.Timeout)
	integer("ACK_MAX_RETRIES", &c.Ack.MaxRetries)

	str("JWT_SECRET", &c.Auth.JWTSecret)
	dur("TOKEN_TTL", &c.Auth.TokenTTL)
	str("BOOTSTRAP_ADMIN", &c.Auth.BootstrapAdmin)
	str("BOOTSTRAP_PASSWORD", &c.Auth.BootstrapPass)

	dur("RETENTION_INTERVAL", &c.Retention.Interval)
	dur("RETENTION_MAX_AGE", &c.Retention.MaxAge)

	str("LOG_LEVEL", &c.Log.Level)
	str("LOG_FORMAT", &c.Log.Format)

	boolean("METRICS_ENABLED", &c.Metrics.Enabled)
	str("METRICS_PATH", &c.Metrics.Path)

	boolean("TRACING_ENABLED", &c.Tracing.Enabled)
	str("TRACING_ENDPOINT", &c.Tracing.Endpoint)
	str("TRACING_SERVICE_NAME", &c.Tracing.ServiceName)
	float("TRACING_SAMPLE_RATIO", &c.Tracing.SampleRatio)
	boolean("TRACING_INSECURE", &c.Tracing.Insecure)

	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}
