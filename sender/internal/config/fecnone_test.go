package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// Setting only the codec must not stop the sender starting.
//
// An operator turning error correction off sets one variable. The parity count beside it stays at the
// built-in default of fifteen, which they never wrote and have no reason to think about — and the sender
// refused to boot, reporting a shard geometry error against that untouched default. Load now drops the
// leftover before validating, so this is a configuration that starts.
func TestNoneCodecStartsWithoutAlsoZeroingParity(t *testing.T) {
	t.Setenv("OTP_SENDER_FEC_CODEC", "none")
	t.Setenv("OTP_SENDER_ACK_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("OTP_SENDER_JWT_SECRET", "0123456789abcdef0123456789abcdef")

	cfg, err := config.Load("")
	require.NoError(t, err, "a codec of none with the default parity count must load")
	assert.Equal(t, "none", cfg.Optical.FEC.Codec)
	assert.Zero(t, cfg.Optical.FEC.ParityShards, "the inherited count is dropped, not honoured")
}

// A geometry a codec genuinely cannot satisfy must still fail, or this fix has swallowed real errors.
func TestAnImpossibleGeometryStillFailsToLoad(t *testing.T) {
	t.Setenv("OTP_SENDER_FEC_CODEC", "reed-solomon")
	t.Setenv("OTP_SENDER_FEC_DATA_SHARDS", "250")
	t.Setenv("OTP_SENDER_FEC_PARITY_SHARDS", "250")
	t.Setenv("OTP_SENDER_ACK_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("OTP_SENDER_JWT_SECRET", "0123456789abcdef0123456789abcdef")

	_, err := config.Load("")
	require.Error(t, err, "Reed-Solomon runs out of field at 500 shards and must say so")
	assert.Contains(t, err.Error(), "optical.fec")
}

// And a codec that does make parity keeps the count it was configured with.
func TestAParityCodecKeepsItsConfiguredCount(t *testing.T) {
	t.Setenv("OTP_SENDER_FEC_CODEC", "raptorq")
	t.Setenv("OTP_SENDER_FEC_PARITY_SHARDS", "15")
	t.Setenv("OTP_SENDER_ACK_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("OTP_SENDER_JWT_SECRET", "0123456789abcdef0123456789abcdef")

	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, 15, cfg.Optical.FEC.ParityShards)
}
