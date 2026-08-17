package pipeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// How much parity a block deserves.
//
// The configuration names a ratio by naming a pair, and it was being read as a flat count: "15 per 100"
// gave a five-chunk file fifteen parity shards, so a transfer that needed five frames of data went out as
// twenty of parity beside it. Three hundred percent overhead where fifteen was asked for, and four times
// the frames to photograph.

// Note these figures apply to a single-block transfer only. See the caller in fecEncode: a multi-block
// transfer keeps the flat count, because the receiver locates a parity shard by dividing its ESI by that
// count and a varying one would misplace every shard after the first block.
func TestParityFollowsTheBlockRatherThanTheConfiguredMaximum(t *testing.T) {
	const data, parity = 100, 15

	cases := []struct {
		block int
		want  int
		why   string
	}{
		{block: 1, want: 1, why: "a single chunk asked for 15% and used to get 1500%"},
		{block: 5, want: 1, why: "the case reported: 5 chunks, 21 frames, of which 15 were parity"},
		{block: 20, want: 3, why: "15% of twenty, rounded up"},
		{block: 50, want: 8, why: "half a block, half the parity"},
		{block: 100, want: 15, why: "a full block is unchanged, which is what makes this only a reduction"},
	}

	for _, tc := range cases {
		got := proportionalParity(tc.block, data, parity)
		assert.Equal(t, tc.want, got, "block of %d: %s", tc.block, tc.why)
	}
}

// Never more than configured, so no block can be made to emit more than the operator asked for.
func TestParityNeverExceedsWhatWasConfigured(t *testing.T) {
	for _, block := range []int{1, 50, 100, 250, 10000} {
		assert.LessOrEqual(t, proportionalParity(block, 100, 15), 15,
			"a block of %d must not out-produce the configuration", block)
	}
}

// A block always keeps some protection, because the operator asked for redundancy.
func TestABlockIsNeverLeftUnprotected(t *testing.T) {
	// 1 chunk at a 1% ratio rounds to nothing without the floor, which would quietly turn redundancy off
	// for exactly the transfers least able to lose a frame.
	assert.Equal(t, 1, proportionalParity(1, 1000, 10))
	assert.Equal(t, 1, proportionalParity(2, 100, 15))
}

// Parity turned off stays off. The floor must not resurrect it.
func TestNoParityConfiguredMeansNone(t *testing.T) {
	assert.Zero(t, proportionalParity(5, 100, 0))
	assert.Zero(t, proportionalParity(0, 100, 15), "an empty block has nothing to protect")
}

// A degenerate configuration falls back rather than dividing by zero.
func TestADegenerateConfigurationIsSafe(t *testing.T) {
	assert.Equal(t, 15, proportionalParity(5, 0, 15))
	assert.Equal(t, 15, proportionalParity(5, -1, 15))
}
