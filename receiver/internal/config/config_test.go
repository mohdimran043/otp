package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// writeConfig puts a configuration file in a temporary directory and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "receiver.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// minimal is a configuration with the fields that have no default filled in: the two
// secrets, and an explicit callback policy so Validate does not flag the safe-default
// empty allowlist as a problem worth failing the test over.
const minimal = `
ack:
  secret: an acknowledgement secret
auth:
  jwt_secret: a jwt secret long enough to sign with
callback:
  allow_any_host: true
`

// TestDefaultsMatchTheReceiverRatherThanTheSender pins the one field that is easy to get
// wrong by copying the sender's config: the default MinIO bucket. A receiver and a sender
// pointed at the same MinIO endpoint must not default into writing each other's bucket.
func TestDefaultsMatchTheReceiverRatherThanTheSender(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, minimal))
	require.NoError(t, err)

	require.Equal(t, "filesystem", cfg.Storage.Backend)
	require.Equal(t, "otp-receiver", cfg.Storage.MinIO.Bucket)
	require.Equal(t, "us-east-1", cfg.Storage.MinIO.Region)
}

// TestMinIOEnvironmentOverridesMatchTheSender covers the two bindings the receiver was
// missing relative to the sender: without them, an operator who set
// OTP_RECEIVER_MINIO_USE_SSL or OTP_RECEIVER_MINIO_REGION expecting the same behaviour as
// the sender's OTP_SENDER_ equivalents would have the value silently ignored.
func TestMinIOEnvironmentOverridesMatchTheSender(t *testing.T) {
	path := writeConfig(t, minimal)

	t.Setenv("OTP_RECEIVER_STORAGE_BACKEND", "minio")
	t.Setenv("OTP_RECEIVER_MINIO_ENDPOINT", "minio:9000")
	t.Setenv("OTP_RECEIVER_MINIO_ACCESS_KEY", "an-access-key")
	t.Setenv("OTP_RECEIVER_MINIO_SECRET_KEY", "a-secret-key")
	t.Setenv("OTP_RECEIVER_MINIO_BUCKET", "otp-receiver-custom")
	t.Setenv("OTP_RECEIVER_MINIO_USE_SSL", "true")
	t.Setenv("OTP_RECEIVER_MINIO_REGION", "eu-west-1")

	cfg, err := config.Load(path)
	require.NoError(t, err)

	require.Equal(t, "minio", cfg.Storage.Backend)
	require.Equal(t, "minio:9000", cfg.Storage.MinIO.Endpoint)
	require.Equal(t, "an-access-key", cfg.Storage.MinIO.AccessKey)
	require.Equal(t, "a-secret-key", cfg.Storage.MinIO.SecretKey)
	require.Equal(t, "otp-receiver-custom", cfg.Storage.MinIO.Bucket)
	require.True(t, cfg.Storage.MinIO.UseSSL, "OTP_RECEIVER_MINIO_USE_SSL must be honoured, as it already is on the sender")
	require.Equal(t, "eu-west-1", cfg.Storage.MinIO.Region, "OTP_RECEIVER_MINIO_REGION must be honoured, as it already is on the sender")
}

// TestMalformedMinIOUseSSLIsReported covers the boolean parse failure path the new
// binding introduces.
func TestMalformedMinIOUseSSLIsReported(t *testing.T) {
	t.Setenv("OTP_RECEIVER_MINIO_USE_SSL", "not-a-bool")
	_, err := config.Load(writeConfig(t, minimal))
	require.ErrorContains(t, err, "OTP_RECEIVER_MINIO_USE_SSL")
}
