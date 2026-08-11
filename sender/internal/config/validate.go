package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/opticaltransport/otp/shared/compress"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/fec"
	"github.com/opticaltransport/otp/shared/protocol"
)

// ErrInvalid means the configuration cannot be used.
var ErrInvalid = errors.New("config: invalid")

// Validate checks the configuration describes a deployment that can work.
//
// It is deliberately thorough, because the alternative is worse. Every check here
// stands for a failure that would otherwise happen later and further away: an encoder
// name that no encoder answers to fails when the first transmission is created, a grid
// too small for its header band fails when the first frame is rendered, a FEC geometry
// its codec cannot encode fails after the file has already been chunked. Refusing to
// start is a better outcome than any of those, and it puts the error in front of
// whoever is holding the configuration file.
//
// The names are checked against the shared registries rather than against a list kept
// here, so adding an encoder to the protocol makes it configurable without anybody
// remembering to update this file — and a list here would eventually disagree with the
// code, which is the failure mode this avoids.
func (c Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// Server.
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		add("server.port %d is not a port", c.Server.Port)
	}
	if c.Server.MaxUploadBytes <= 0 {
		add("server.max_upload_bytes must be positive")
	}
	if (c.Server.TLSCertFile == "") != (c.Server.TLSKeyFile == "") {
		add("server.tls_cert_file and server.tls_key_file must be set together")
	}

	// Database.
	if c.Database.URL == "" {
		add("database.url is required")
	}
	if c.Database.MaxConns < 1 {
		add("database.max_conns must be at least 1")
	}
	if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		add("database.min_conns %d must be between 0 and max_conns %d",
			c.Database.MinConns, c.Database.MaxConns)
	}

	// Storage and broker backends.
	switch c.Storage.Backend {
	case "filesystem":
		if c.Storage.Root == "" {
			add("storage.root is required for the filesystem backend")
		}
	case "minio":
		if c.Storage.MinIO.Endpoint == "" {
			add("storage.minio.endpoint is required for the minio backend")
		}
		if c.Storage.MinIO.Bucket == "" {
			add("storage.minio.bucket is required for the minio backend")
		}
		if c.Storage.MinIO.AccessKey == "" || c.Storage.MinIO.SecretKey == "" {
			add("storage.minio access_key and secret_key are required for the minio backend")
		}
	default:
		add("storage.backend %q is not one of filesystem, minio", c.Storage.Backend)
	}

	switch c.Broker.Backend {
	case "internal":
	case "rabbitmq":
		if c.Broker.RabbitMQ.URL == "" {
			add("broker.rabbitmq.url is required for the rabbitmq backend")
		}
		if c.Broker.RabbitMQ.Prefetch < 1 {
			add("broker.rabbitmq.prefetch must be at least 1")
		}
	default:
		add("broker.backend %q is not one of internal, rabbitmq", c.Broker.Backend)
	}

	// Jobs.
	if c.Jobs.Concurrency < 1 {
		add("jobs.concurrency must be at least 1")
	}
	if c.Jobs.MaxAttempts < 1 {
		add("jobs.max_attempts must be at least 1")
	}
	if c.Jobs.PollInterval <= 0 {
		add("jobs.poll_interval must be positive")
	}
	if c.Jobs.BackoffBase <= 0 {
		add("jobs.backoff_base must be positive")
	}
	if c.Jobs.BackoffMax < c.Jobs.BackoffBase {
		add("jobs.backoff_max must be at least backoff_base")
	}

	// The optical profile, checked against the registries that will have to serve it.
	enc, err := encoding.ByName(c.Optical.Encoder)
	if err != nil {
		add("optical.encoder %q is not one of %s", c.Optical.Encoder,
			strings.Join(encoding.Names(), ", "))
	} else if c.Optical.BitDepth != 0 {
		depths := enc.SupportedBitDepths()
		if !containsUint8(depths, uint8(c.Optical.BitDepth)) {
			add("optical.bit_depth %d is not one the %s encoder offers (%v)",
				c.Optical.BitDepth, c.Optical.Encoder, depths)
		}
	}

	if _, err := compress.ByName(c.Optical.Compression); err != nil {
		add("optical.compression %q is not one of %s", c.Optical.Compression,
			strings.Join(compress.Names(), ", "))
	}
	if c.Optical.Level < 0 || c.Optical.Level > 9 {
		add("optical.level %d must be between 0 and 9", c.Optical.Level)
	}

	codec, err := fec.ByName(c.Optical.FEC.Codec)
	if err != nil {
		add("optical.fec.codec %q is not one of %s", c.Optical.FEC.Codec,
			strings.Join(fec.Names(), ", "))
	} else if err := codec.Validate(c.Optical.FEC.DataShards, c.Optical.FEC.ParityShards); err != nil {
		// The codec's own words, because it knows why: Reed-Solomon runs out of field,
		// the sparse code needs a minimum parity count to be worth anything.
		add("optical.fec: %s", err)
	}

	// The grid, checked by building the layout the frames will actually use.
	layout, err := protocol.NewLayoutQuiet(
		c.Optical.GridWidth, c.Optical.GridHeight, c.Optical.CellPixels, c.Optical.QuietZone)
	if err != nil {
		add("optical grid: %s", err)
	} else if enc != nil && err == nil {
		// And by asking the encoder what it could carry there, which catches a grid that
		// is valid but too small to be worth displaying.
		capacity, err := enc.EstimateCapacity(layout, uint8(c.Optical.BitDepth))
		if err != nil {
			add("optical grid: %s", err)
		} else if capacity.PayloadBytes < protocol.HeaderSize {
			add("optical grid carries only %d bytes a frame, too little to be useful",
				capacity.PayloadBytes)
		}
	}

	if c.Optical.EncryptionKeyHex != "" {
		key, err := hex.DecodeString(c.Optical.EncryptionKeyHex)
		switch {
		case err != nil:
			add("optical.encryption_key_hex is not hexadecimal")
		case len(key) != protocol.KeySize:
			add("optical.encryption_key_hex decodes to %d bytes, and must be %d",
				len(key), protocol.KeySize)
		}
	}
	if c.Optical.ManifestInterval < 1 {
		add("optical.manifest_interval must be at least 1, or a late receiver can never join")
	}

	// Display.
	switch c.Display.Sink {
	case "file":
		if c.Display.Dir == "" {
			add("display.dir is required for the file sink")
		}
	case "opengl":
		// Availability is a build-tag matter, reported by the sink itself at startup.
	case "none":
		// The discard sink needs nothing: camera-only mode watches the physical display instead.
	default:
		add("display.sink %q is not one of file, opengl, none", c.Display.Sink)
	}
	if c.Display.FPS <= 0 {
		add("display.fps must be positive")
	}
	if c.Display.Gamma <= 0 {
		add("display.gamma must be positive")
	}
	if c.Display.Brightness < -255 || c.Display.Brightness > 255 {
		add("display.brightness %v is outside the range of a pixel", c.Display.Brightness)
	}
	if c.Display.WindowSize < 1 {
		add("display.window_size must be at least 1")
	}

	// Acknowledgements.
	if c.Ack.Dir == "" {
		add("ack.dir is required")
	}
	if c.Ack.Secret == "" {
		// No default is possible. A default signing secret is not a secret, and the
		// acknowledgement channel is the one input the sender takes from outside itself:
		// anything able to write that directory could report chunks as delivered that
		// never arrived, silently truncating every transmission.
		add("ack.secret is required and has no default")
	}
	if c.Ack.Timeout <= 0 {
		add("ack.timeout must be positive")
	}
	if c.Ack.PollInterval <= 0 {
		add("ack.poll_interval must be positive")
	}
	if c.Ack.MaxRetries < 1 {
		add("ack.max_retries must be at least 1")
	}

	// Authentication.
	if c.Auth.JWTSecret == "" {
		add("auth.jwt_secret is required and has no default")
	} else if len(c.Auth.JWTSecret) < 32 {
		add("auth.jwt_secret is %d bytes; HS256 tokens need at least 32 to be worth signing",
			len(c.Auth.JWTSecret))
	}
	if c.Auth.TokenTTL <= 0 {
		add("auth.token_ttl must be positive")
	}

	// Retention.
	if c.Retention.Interval <= 0 {
		add("retention.interval must be positive")
	}
	if c.Retention.MaxAge <= 0 {
		add("retention.max_age must be positive")
	}

	// Logging, metrics, tracing.
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
	if c.Metrics.Enabled && !strings.HasPrefix(c.Metrics.Path, "/") {
		add("metrics.path %q must begin with a slash", c.Metrics.Path)
	}
	if c.Tracing.Enabled && c.Tracing.Endpoint == "" {
		add("tracing.endpoint is required when tracing is enabled")
	}
	if c.Tracing.SampleRatio < 0 || c.Tracing.SampleRatio > 1 {
		add("tracing.sample_ratio %v must be between 0 and 1", c.Tracing.SampleRatio)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  - %s", ErrInvalid, strings.Join(problems, "\n  - "))
}

func containsUint8(haystack []uint8, needle uint8) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// EncryptionKey returns the configured payload key, or nil when payloads travel in
// the clear. Validate has already checked the length, so this cannot fail.
func (c Config) EncryptionKey() []byte {
	if c.Optical.EncryptionKeyHex == "" {
		return nil
	}
	key, err := hex.DecodeString(c.Optical.EncryptionKeyHex)
	if err != nil {
		return nil
	}
	return key
}

// Layout returns the frame geometry.
func (c Config) Layout() (protocol.Layout, error) {
	return protocol.NewLayoutQuiet(
		c.Optical.GridWidth, c.Optical.GridHeight, c.Optical.CellPixels, c.Optical.QuietZone)
}
