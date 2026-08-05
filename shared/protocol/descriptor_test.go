package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescriptorRoundTrip(t *testing.T) {
	cases := []Descriptor{
		{GridWidth: 192, GridHeight: 108, CellPixels: 8},
		{GridWidth: 48, GridHeight: 96, CellPixels: 1},
		{GridWidth: 4095, GridHeight: 4095, CellPixels: 255, Flags: 0x0F},
		{GridWidth: 1024, GridHeight: 768, CellPixels: 3, Flags: 0x0A},
	}
	for _, in := range cases {
		b, err := in.MarshalBits()
		require.NoError(t, err)
		require.Len(t, b, (DescriptorBits+7)/8)

		var out Descriptor
		require.NoError(t, out.UnmarshalBits(b))
		assert.Equal(t, in, out)
	}
}

func TestDescriptorFitsItsBlock(t *testing.T) {
	assert.LessOrEqual(t, DescriptorBits, DescriptorCells,
		"a descriptor must fit the block reserved for it at each corner")
}

func TestDescriptorRejectsOutOfRangeFields(t *testing.T) {
	_, err := Descriptor{GridWidth: 4096, GridHeight: 100, CellPixels: 8}.MarshalBits()
	assert.ErrorIs(t, err, ErrGridBounds)

	_, err = Descriptor{GridWidth: 100, GridHeight: 4096, CellPixels: 8}.MarshalBits()
	assert.ErrorIs(t, err, ErrGridBounds)

	_, err = Descriptor{GridWidth: 100, GridHeight: 100, CellPixels: 8, Flags: 0x10}.MarshalBits()
	assert.ErrorIs(t, err, ErrGridBounds)
}

// The checksum is what makes the eight-way orientation search safe: a wrong
// hypothesis reads the wrong cells, and the CRC has to reject that rather than
// let a plausible-looking geometry through.
func TestDescriptorCRCCatchesEverySingleBitFlip(t *testing.T) {
	good, err := Descriptor{GridWidth: 192, GridHeight: 108, CellPixels: 8}.MarshalBits()
	require.NoError(t, err)

	for bit := 0; bit < DescriptorBits; bit++ {
		corrupted := make([]byte, len(good))
		copy(corrupted, good)
		corrupted[bit/8] ^= 1 << uint(7-bit%8)

		var d Descriptor
		err := d.UnmarshalBits(corrupted)
		assert.Error(t, err, "flipping bit %d must be detected", bit)
	}
}

func TestDescriptorForMatchesLayout(t *testing.T) {
	l, err := NewLayout(320, 180, 6)
	require.NoError(t, err)

	d := DescriptorFor(l)
	assert.Equal(t, uint16(320), d.GridWidth)
	assert.Equal(t, uint16(180), d.GridHeight)
	assert.Equal(t, uint8(6), d.CellPixels)

	back, err := d.Layout()
	require.NoError(t, err)
	assert.Equal(t, l, back, "a descriptor must reproduce the layout it came from")
}

// Every corner's block must occupy distinct cells, and all must sit inside a
// fixed band where the decoder's local frame stays accurate.
func TestDescriptorBlocksAreDistinctAndCornerLocal(t *testing.T) {
	l, err := NewLayout(DefaultGridWidth, DefaultGridHeight, DefaultCellPixels)
	require.NoError(t, err)

	seen := map[Cell]int{}
	for corner := 0; corner < 4; corner++ {
		cells := l.DescriptorCellsAt(corner)
		require.Len(t, cells, DescriptorCells)
		for _, c := range cells {
			if prev, dup := seen[c]; dup {
				t.Fatalf("cell %v claimed by descriptor blocks %d and %d", c, prev, corner)
			}
			seen[c] = corner
			assert.False(t, l.inFinderBox(c), "descriptor cell %v must not overlap a fiducial", c)
			assert.True(t, l.inDescriptorBlock(c))
		}

		// The first cell of each block must be the one nearest its own fiducial,
		// since the ordering is defined in the corner's frame rather than the
		// grid's. Getting this backwards reverses the bit order at three corners.
		first := cells[0]
		fid := l.FinderOrigins()[corner]
		assert.LessOrEqual(t, abs(first.Y-fid.Y), FinderPattern,
			"block %d should start in the row band of its own fiducial", corner)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestCRC16IsCCITTFalse(t *testing.T) {
	// The canonical check value for CRC-16/CCITT-FALSE over "123456789".
	assert.Equal(t, uint16(0x29B1), crc16([]byte("123456789")))
}
