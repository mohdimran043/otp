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

	"github.com/opticaltransport/otp/shared/protocol"
)

// Config is the receiver's complete configuration.
type Config struct {
	Server   Server   `yaml:"server"`
	Database Database `yaml:"database"`
	Storage  Storage  `yaml:"storage"`
	Capture  Capture  `yaml:"capture"`
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
	// Source is "file", or "gocv" in a build that includes it.
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
			Source:       "file",
			Dir:          "/var/lib/otp/shared/frames",
			Consume:      true,
			IdleInterval: 100 * time.Millisecond,
		},
		Decoder: Decoder{
			MinFinderScore: 0.75,
			MinTimingScore: 0.6,
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
	case "gocv":
		// Availability is a build-tag matter, reported by the source itself at startup.
	default:
		add("capture.source %q is not one of file, gocv", c.Capture.Source)
	}
	if c.Capture.IdleInterval <= 0 {
		add("capture.idle_interval must be positive")
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

	str("CAPTURE_SOURCE", &c.Capture.Source)
	str("CAPTURE_DIR", &c.Capture.Dir)
	boolean("CAPTURE_CONSUME", &c.Capture.Consume)
	dur("CAPTURE_IDLE_INTERVAL", &c.Capture.IdleInterval)
	boolean("CAPTURE_RETAIN_FRAMES", &c.Capture.RetainFrames)

	integer("DECODER_CELL_PIXELS_HINT", &c.Decoder.CellPixelsHint)
	float("DECODER_MIN_FINDER_SCORE", &c.Decoder.MinFinderScore)
	float("DECODER_MIN_TIMING_SCORE", &c.Decoder.MinTimingScore)
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
	w.current.Store(&next)
	return nil
}
