package fec_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/fec"
)

// Choosing "no error correction" must not require also restating a parity count.
//
// The shard pair is inherited from the deployment's defaults, and the codec is chosen per transfer. Those
// two facts together produced a dead end: selecting `none` in the sender's dropdown sent a codec of none
// and a parity count of fifteen, the none codec correctly refused to make fifteen parity shards, and the
// transfer was rejected with an error naming a field the interface does not offer. The same pair set in
// the environment stopped the sender booting at all.
//
// So the rule lives here, once, rather than as a special case written twice at the two places that
// inherit the pair.

func TestNoParityCodecTakesNoParity(t *testing.T) {
	none, err := fec.ByName("none")
	require.NoError(t, err)

	data, parity := fec.NormaliseShards(none, 100, 15)
	assert.Equal(t, 0, parity, "a codec that makes no parity is handed none to make")
	assert.Equal(t, 100, data,
		"the source count is left alone: noneCodec accepts it, and zeroing it would divide by it downstream")

	require.NoError(t, none.Validate(data, parity),
		"the whole point: the pair this returns must be one the codec accepts")
}

// Every other codec is untouched, because for them the pair is the operator's actual instruction.
func TestCodecsThatMakeParityKeepTheirGeometry(t *testing.T) {
	for _, name := range fec.Names() {
		if name == "none" {
			continue
		}
		codec, err := fec.ByName(name)
		require.NoError(t, err)

		data, parity := fec.NormaliseShards(codec, 100, 15)
		assert.Equal(t, 100, data, "%s data shards", name)
		assert.Equal(t, 15, parity, "%s parity shards", name)
	}
}

// A nil codec is what an unknown name resolves to at the call sites, and normalising must not panic
// before the caller has had the chance to report the real error.
func TestANilCodecIsLeftAlone(t *testing.T) {
	data, parity := fec.NormaliseShards(nil, 100, 15)
	assert.Equal(t, 100, data)
	assert.Equal(t, 15, parity)
}

// Already-zero parity is not disturbed, and a none codec with zero stays valid.
func TestZeroParityIsIdempotent(t *testing.T) {
	none, err := fec.ByName("none")
	require.NoError(t, err)

	data, parity := fec.NormaliseShards(none, 0, 0)
	assert.Equal(t, 0, data)
	assert.Equal(t, 0, parity)
	require.NoError(t, none.Validate(data, parity))
}
