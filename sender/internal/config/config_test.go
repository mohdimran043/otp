package config_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// writeConfig puts a configuration file in a temporary directory and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sender.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// minimal is a configuration with the two fields that have no default filled in.
const minimal = `
ack:
  secret: an acknowledgement secret
auth:
  jwt_secret: a jwt secret long enough to sign with
`

func TestDefaultsAreUsableOnceSecretsAreSupplied(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, minimal))
	require.NoError(t, err)

	require.Equal(t, 8080, cfg.Server.Port)
	require.Equal(t, "color8", cfg.Optical.Encoder)
	require.Equal(t, "zstd", cfg.Optical.Compression)
	require.Equal(t, "filesystem", cfg.Storage.Backend)
	require.Equal(t, "internal", cfg.Broker.Backend)
	require.Equal(t, "0.0.0.0:8080", cfg.Addr())
	require.False(t, cfg.TLSEnabled())

	// The default geometry must actually build a layout, since every frame depends on it.
	layout, err := cfg.Layout()
	require.NoError(t, err)
	require.Equal(t, 128, layout.GridWidth)

	require.Equal(t, 100*time.Millisecond, cfg.FrameInterval(), "ten frames a second")
	require.Nil(t, cfg.EncryptionKey(), "payloads travel in the clear unless a key is set")

	require.Equal(t, time.Hour, cfg.Retention.Interval, "the sweep runs hourly by default")
	require.Equal(t, 24*time.Hour, cfg.Retention.MaxAge, "a transfer gets a day to complete before it is reaped")
}

// TestMissingFileIsNotAnError covers the container deployment, which configures everything
// through the environment. Requiring an empty file to be mounted alongside it would be
// pure ceremony.
func TestMissingFileIsNotAnError(t *testing.T) {
	t.Setenv("OTP_SENDER_ACK_SECRET", "from the environment")
	t.Setenv("OTP_SENDER_JWT_SECRET", "a jwt secret long enough to sign with")

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, err)
	require.Equal(t, "from the environment", cfg.Ack.Secret)
}

// TestUnparseableFileIsAnError is the other side of that: a file somebody wrote must never
// be silently ignored.
func TestUnparseableFileIsAnError(t *testing.T) {
	_, err := config.Load(writeConfig(t, "server: [this is not a mapping"))
	require.Error(t, err)
}

func TestEnvironmentOverridesFile(t *testing.T) {
	path := writeConfig(t, minimal+`
server:
  port: 9000
optical:
  encoder: binary
display:
  fps: 4
`)

	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, 9000, cfg.Server.Port)
	require.Equal(t, "binary", cfg.Optical.Encoder)

	t.Setenv("OTP_SENDER_PORT", "9100")
	t.Setenv("OTP_SENDER_ENCODER", "color16")
	t.Setenv("OTP_SENDER_BIT_DEPTH", "4")
	t.Setenv("OTP_SENDER_DISPLAY_FPS", "24.5")
	t.Setenv("OTP_SENDER_CORS_ORIGINS", "https://a.example, https://b.example")
	t.Setenv("OTP_SENDER_DB_MIGRATE_ON_START", "false")
	t.Setenv("OTP_SENDER_ACK_TIMEOUT", "45s")
	t.Setenv("OTP_SENDER_RETENTION_INTERVAL", "30m")
	t.Setenv("OTP_SENDER_RETENTION_MAX_AGE", "48h")

	cfg, err = config.Load(path)
	require.NoError(t, err)
	require.Equal(t, 9100, cfg.Server.Port)
	require.Equal(t, "color16", cfg.Optical.Encoder)
	require.Equal(t, 24.5, cfg.Display.FPS)
	require.Equal(t, []string{"https://a.example", "https://b.example"}, cfg.Server.CORSOrigins)
	require.False(t, cfg.Database.MigrateOnStart)
	require.Equal(t, 45*time.Second, cfg.Ack.Timeout)
	require.Equal(t, 30*time.Minute, cfg.Retention.Interval)
	require.Equal(t, 48*time.Hour, cfg.Retention.MaxAge)
}

func TestMalformedEnvironmentValuesAreReported(t *testing.T) {
	path := writeConfig(t, minimal)

	t.Setenv("OTP_SENDER_PORT", "eighty")
	_, err := config.Load(path)
	require.ErrorContains(t, err, "OTP_SENDER_PORT")

	t.Setenv("OTP_SENDER_PORT", "8080")
	t.Setenv("OTP_SENDER_ACK_TIMEOUT", "half an hour")
	_, err = config.Load(path)
	require.ErrorContains(t, err, "OTP_SENDER_ACK_TIMEOUT")

	t.Setenv("OTP_SENDER_ACK_TIMEOUT", "45s")
	t.Setenv("OTP_SENDER_RETENTION_MAX_AGE", "a fortnight")
	_, err = config.Load(path)
	require.ErrorContains(t, err, "OTP_SENDER_RETENTION_MAX_AGE")
}

// TestRetentionDurationsMustBePositive covers the sweep's two durations the same way every
// other duration in this configuration is checked: a zero or negative value is not a fast
// retry or an instant sweep, it is a value that makes the underlying arithmetic (now minus
// MaxAge, now plus Interval) meaningless, and the ticker or interval built from it would
// misbehave in ways an operator would only discover once transfers started vanishing early or
// the sweep started spinning.
func TestRetentionDurationsMustBePositive(t *testing.T) {
	cfg := config.Default()
	cfg.Ack.Secret = "an acknowledgement secret"
	cfg.Auth.JWTSecret = "a jwt secret long enough to sign with"

	cfg.Retention.Interval = 0
	err := cfg.Validate()
	require.ErrorContains(t, err, "retention.interval must be positive")

	cfg.Retention.Interval = time.Hour
	cfg.Retention.MaxAge = -time.Second
	err = cfg.Validate()
	require.ErrorContains(t, err, "retention.max_age must be positive")

	cfg.Retention.MaxAge = 24 * time.Hour
	require.NoError(t, cfg.Validate())
}

// TestSecretsHaveNoDefault is a security test. A default signing secret is not a secret,
// and the acknowledgement channel is the one input the sender takes from outside itself.
func TestSecretsHaveNoDefault(t *testing.T) {
	_, err := config.Load(writeConfig(t, "log:\n  level: info\n"))
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "ack.secret is required")
	require.ErrorContains(t, err, "auth.jwt_secret is required")

	// And a JWT secret too short to be worth signing with is refused rather than accepted
	// quietly.
	_, err = config.Load(writeConfig(t, `
ack:
  secret: fine
auth:
  jwt_secret: short
`))
	require.ErrorContains(t, err, "auth.jwt_secret is 5 bytes")
}

// TestValidationChecksNamesAgainstTheRegistries is what stops a typo becoming a runtime
// failure halfway through a transmission. The names come from the shared registries, so the
// error also tells the operator what the alternatives are.
func TestValidationChecksNamesAgainstTheRegistries(t *testing.T) {
	cases := map[string]string{
		"optical:\n  encoder: qr-code\n":                "optical.encoder",
		"optical:\n  compression: lzma\n":               "optical.compression",
		"optical:\n  fec:\n    codec: turbo\n":          "optical.fec.codec",
		"storage:\n  backend: dropbox\n":                "storage.backend",
		"broker:\n  backend: kafka\n":                   "broker.backend",
		"display:\n  sink: hologram\n":                  "display.sink",
		"log:\n  level: verbose\n":                      "log.level",
		"optical:\n  encoder: binary\n  bit_depth: 4\n": "optical.bit_depth",
	}
	for body, want := range cases {
		t.Run(strings.TrimSpace(want), func(t *testing.T) {
			_, err := config.Load(writeConfig(t, minimal+body))
			require.ErrorIs(t, err, config.ErrInvalid)
			require.ErrorContains(t, err, want)
		})
	}

	// A valid but unusual combination must be accepted: the grey ramp at three bits is
	// documented as needing a controlled installation, not as invalid.
	cfg, err := config.Load(writeConfig(t, minimal+"optical:\n  encoder: grayscale\n  bit_depth: 3\n"))
	require.NoError(t, err)
	require.Equal(t, "grayscale", cfg.Optical.Encoder)
}

// TestValidationChecksTheGeometryByBuildingIt catches a grid that cannot hold its own
// header band — which would otherwise fail when the first frame was rendered, long after
// the file had been uploaded, compressed, and chunked.
func TestValidationChecksTheGeometryByBuildingIt(t *testing.T) {
	_, err := config.Load(writeConfig(t, minimal+`
optical:
  grid_width: 48
  grid_height: 48
`))
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "optical grid")

	_, err = config.Load(writeConfig(t, minimal+"optical:\n  grid_width: 8\n"))
	require.ErrorIs(t, err, config.ErrInvalid)
}

// TestValidationChecksTheFECGeometry uses the codec's own opinion, so the error explains
// why rather than merely that.
func TestValidationChecksTheFECGeometry(t *testing.T) {
	// Reed-Solomon runs out of field past 256 shards.
	_, err := config.Load(writeConfig(t, minimal+`
optical:
  fec:
    codec: reed-solomon
    data_shards: 250
    parity_shards: 32
`))
	require.ErrorContains(t, err, "optical.fec")

	// The sparse code needs a minimum parity count to be worth anything.
	_, err = config.Load(writeConfig(t, minimal+`
optical:
  fec:
    codec: ldpc
    data_shards: 64
    parity_shards: 4
`))
	require.ErrorContains(t, err, "optical.fec")

	// And the same geometry is fine under a code that can serve it.
	cfg, err := config.Load(writeConfig(t, minimal+`
optical:
  fec:
    codec: reed-solomon
    data_shards: 64
    parity_shards: 4
`))
	require.NoError(t, err)
	require.Equal(t, 4, cfg.Optical.FEC.ParityShards)
}

func TestEncryptionKeyIsCheckedAndDecoded(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	cfg, err := config.Load(writeConfig(t, minimal+"optical:\n  encryption_key_hex: "+valid+"\n"))
	require.NoError(t, err)
	require.Len(t, cfg.EncryptionKey(), 32)

	_, err = config.Load(writeConfig(t, minimal+"optical:\n  encryption_key_hex: not-hex\n"))
	require.ErrorContains(t, err, "hexadecimal")

	_, err = config.Load(writeConfig(t, minimal+"optical:\n  encryption_key_hex: abcd\n"))
	require.ErrorContains(t, err, "must be 32")
}

// TestReloadAppliesOnlyTheSafeSubset is the point of the whole reload design. A frame rate
// may change under a running transmission; the grid it is rendered at may not, because the
// geometry is written into every frame header and the chunk size was derived from it.
func TestReloadAppliesOnlyTheSafeSubset(t *testing.T) {
	path := writeConfig(t, minimal+`
jobs:
  concurrency: 4
display:
  fps: 10
  window_size: 64
log:
  level: info
`)
	initial, err := config.Load(path)
	require.NoError(t, err)

	watcher := config.NewWatcher(path, initial)

	var ignored []string
	watcher.OnIgnored(func(fields []string) { ignored = fields })

	changed := make(chan config.Config, 1)
	watcher.OnChange(func(c config.Config) { changed <- c })

	// Change one reloadable field from each section, and one that is not.
	require.NoError(t, os.WriteFile(path, []byte(minimal+`
jobs:
  concurrency: 12
display:
  fps: 30
  window_size: 128
  brightness: -12
  gamma: 1.4
log:
  level: debug
server:
  port: 9999
optical:
  grid_width: 256
`), 0o600))

	watcher.Reload()

	select {
	case got := <-changed:
		require.Equal(t, 12, got.Jobs.Concurrency)
		require.Equal(t, 30.0, got.Display.FPS)
		require.Equal(t, 128, got.Display.WindowSize)
		require.Equal(t, -12.0, got.Display.Brightness)
		require.Equal(t, 1.4, got.Display.Gamma)
		require.Equal(t, "debug", got.Log.Level)

		// And the fields that cannot be reloaded are unchanged.
		require.Equal(t, 8080, got.Server.Port, "the listener cannot move under a running server")
		require.Equal(t, 128, got.Optical.GridWidth, "the grid is written into every frame header")
	default:
		t.Fatal("the reload did not notify its subscriber")
	}

	require.Contains(t, strings.Join(ignored, " "), "server")
	require.Contains(t, strings.Join(ignored, " "), "optical")
}

// TestFailedReloadKeepsTheRunningConfiguration covers the common case in practice: an
// editor saves a file halfway through. A process that adopted that intermediate state, or
// fell back to defaults, would take itself down over a keystroke.
func TestFailedReloadKeepsTheRunningConfiguration(t *testing.T) {
	path := writeConfig(t, minimal+"display:\n  fps: 10\n")
	initial, err := config.Load(path)
	require.NoError(t, err)

	watcher := config.NewWatcher(path, initial)
	var failures []error
	watcher.OnError(func(err error) { failures = append(failures, err) })

	require.NoError(t, os.WriteFile(path, []byte("display:\n  fps: [broken"), 0o600))
	watcher.Reload()

	require.NotEmpty(t, failures, "a failed reload must be reported")
	require.Equal(t, 10.0, watcher.Current().Display.FPS, "the running configuration must survive")

	// A file that parses but does not validate is equally refused.
	require.NoError(t, os.WriteFile(path, []byte(minimal+"display:\n  fps: -5\n"), 0o600))
	watcher.Reload()
	require.Len(t, failures, 2)
	require.Equal(t, 10.0, watcher.Current().Display.FPS)
}

// TestWatchNoticesAReplacedFile is why the watcher watches the directory. Editors and
// deployment tooling replace configuration files rather than writing into them, and a watch
// on the file's inode stops receiving events the moment that happens.
func TestWatchNoticesAReplacedFile(t *testing.T) {
	path := writeConfig(t, minimal+"display:\n  fps: 10\n")
	initial, err := config.Load(path)
	require.NoError(t, err)

	watcher := config.NewWatcher(path, initial)
	changed := make(chan config.Config, 4)
	watcher.OnChange(func(c config.Config) { changed <- c })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { require.NoError(t, watcher.Watch(ctx)) }()

	// Replace rather than rewrite: a temporary file plus a rename, exactly as an editor or
	// a config-map update does it. The replacement is retried because Watch registers its
	// interest in the directory asynchronously, and a rename that landed first would be
	// missed for reasons that have nothing to do with what is being tested.
	replacement := filepath.Join(filepath.Dir(path), "sender.yaml.new")
	deadline := time.After(15 * time.Second)
	for {
		require.NoError(t, os.WriteFile(replacement, []byte(minimal+"display:\n  fps: 25\n"), 0o600))
		require.NoError(t, os.Rename(replacement, path))

		select {
		case got := <-changed:
			require.Equal(t, 25.0, got.Display.FPS)
			return
		case <-time.After(250 * time.Millisecond):
		case <-deadline:
			t.Fatal("a replaced configuration file did not trigger a reload")
		}
	}
}

// TestWatchWithoutAPathBlocksUntilCancelled covers the environment-only deployment, where
// there is no file to watch.
func TestWatchWithoutAPathBlocksUntilCancelled(t *testing.T) {
	watcher := config.NewWatcher("", config.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.NoError(t, watcher.Watch(ctx))
}
