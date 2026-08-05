package encoding_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// testLayout is the grid every test renders at unless it says otherwise: large
// enough that the header band leaves a real payload region, and a cell size the
// operating envelope shows is comfortable for all five encodings.
func testLayout(t *testing.T) protocol.Layout {
	t.Helper()
	l, err := protocol.NewLayout(96, 96, 8)
	require.NoError(t, err)
	return l
}

// variant is one encoder at one of its bit depths.
type variant struct {
	name  string
	enc   encoding.Encoder
	depth uint8
}

// variants enumerates every encoder at every depth it accepts, so a new encoding
// or a new depth is covered by the whole suite the moment it is registered rather
// than when somebody remembers to add it to a table.
func variants(t *testing.T) []variant {
	t.Helper()
	var out []variant
	for _, e := range encoding.All() {
		for _, d := range e.SupportedBitDepths() {
			out = append(out, variant{fmt.Sprintf("%s/d%d", e.Name(), d), e, d})
		}
	}
	require.NotEmpty(t, out)
	return out
}

// frameFor builds a frame whose payload fills the variant's capacity exactly,
// with pseudo-random bytes: a payload that compresses or repeats would let a
// broken modulation still round-trip by accident.
func frameFor(t *testing.T, v variant, l protocol.Layout, seed int64) *protocol.Frame {
	t.Helper()
	capacity, err := v.enc.EstimateCapacity(l, v.depth)
	require.NoError(t, err)

	payload := make([]byte, capacity.PayloadBytes)
	rand.New(rand.NewSource(seed)).Read(payload)

	return protocol.NewFrame(protocol.Header{
		Flags:          protocol.FlagLastChunk,
		CompressionID:  2,
		FECID:          1,
		TransmissionID: uuid.MustParse("6f9619ff-8b86-d011-b42d-00cf4fc964ff"),
		SessionID:      uuid.MustParse("9c1e5f42-0d3a-4b21-8f77-2b1d5a6c9e01"),
		FrameNumber:    7,
		TotalFrames:    64,
		ChunkNumber:    6,
		TotalChunks:    63,
		TimestampMS:    1754300000000,
	}, payload)
}

func TestRegistryHoldsEveryEncoder(t *testing.T) {
	require.Equal(t, []string{"binary", "grayscale", "color8", "color16", "rolling"},
		encoding.Names(), "registry order is by wire id, and the ids are wire values")

	for _, e := range encoding.All() {
		byID, err := encoding.ByID(e.ID())
		require.NoError(t, err)
		require.Same(t, e, byID)

		byName, err := encoding.ByName(e.Name())
		require.NoError(t, err)
		require.Same(t, e, byName)

		require.NotEmpty(t, e.Description())
		require.Contains(t, e.SupportedBitDepths(), e.DefaultBitDepth(),
			"%s must accept its own default depth", e.Name())
	}

	_, err := encoding.ByID(200)
	require.ErrorIs(t, err, encoding.ErrUnknownEncoder)
	_, err = encoding.ByName("qr")
	require.ErrorIs(t, err, encoding.ErrUnknownEncoder)
}

// TestCapacityOrdering pins the property the sender's chunk sizing depends on:
// more bits per cell means more payload per frame on the same grid, and two
// encodings of the same density carry the same amount unless one of them spends
// cells on something else.
func TestCapacityOrdering(t *testing.T) {
	l := testLayout(t)

	var table strings.Builder
	table.WriteString("\ncapacity at " + l.String() + "\n")

	// Capacity at a given density is a property of the grid, not of the palette,
	// so every plain encoding at the same depth must agree to the byte. Rolling is
	// excluded: it reserves cells for its band checksums.
	byDepth := map[uint8]int{}

	for _, v := range variants(t) {
		c, err := v.enc.EstimateCapacity(l, v.depth)
		require.NoError(t, err)
		fmt.Fprintf(&table, "  %-14s %s\n", v.name, c)

		require.Equal(t, int(v.depth), c.BitsPerCell, "%s: depth is the symbol width", v.name)
		require.Positive(t, c.PayloadBytes)
		require.Equal(t, l.GridWidth*l.GridHeight, c.GridCells)
		require.Equal(t, c.GridCells-c.PayloadCells, c.OverheadCells)
		require.Greater(t, c.Efficiency, 0.0)
		require.LessOrEqual(t, c.Efficiency, 1.0)

		if v.enc.Name() == "rolling" {
			plain, err := encoding.Binary.EstimateCapacity(l, v.depth)
			require.NoError(t, err)
			require.Less(t, c.PayloadBytes, plain.PayloadBytes,
				"rolling pays for its band checksums")
			continue
		}

		require.Equal(t, l.PayloadCellCount(), c.PayloadCells,
			"%s: a plain encoding uses the whole payload region", v.name)
		if prev, seen := byDepth[v.depth]; seen {
			require.Equal(t, prev, c.PayloadBytes,
				"%s: capacity at a given depth is set by the grid, not the palette", v.name)
		}
		byDepth[v.depth] = c.PayloadBytes
	}
	t.Log(table.String())

	depths := []uint8{1, 2, 3, 4}
	for i := 1; i < len(depths); i++ {
		require.Greater(t, byDepth[depths[i]], byDepth[depths[i-1]],
			"depth %d must carry more than depth %d", depths[i], depths[i-1])
	}
}

// TestRoundTripPristine is the baseline: every encoding, at every depth, carrying
// a full-capacity payload through a lossless channel.
func TestRoundTripPristine(t *testing.T) {
	l := testLayout(t)

	for _, v := range variants(t) {
		t.Run(v.name, func(t *testing.T) {
			want := frameFor(t, v, l, 42)
			img, err := v.enc.Encode(want, l, v.depth)
			require.NoError(t, err)
			require.Equal(t, l.Bounds(), img.Bounds())

			got, err := v.enc.Decode(img, protocol.LocateOptions{})
			require.NoError(t, err)
			require.Equal(t, want.Payload, got.Payload)
			require.Equal(t, want.Header, got.Header)
			require.Equal(t, want.Footer, got.Footer)
		})
	}
}

// TestDecodeDispatchesOnHeader is the adaptive-grid claim made concrete: a
// receiver told nothing about the frame reads its header, learns which encoding
// produced it, and decodes it. An operator changing encoding profile mid-stream
// needs no receiver-side change, and this is the test that says so.
func TestDecodeDispatchesOnHeader(t *testing.T) {
	l := testLayout(t)

	for _, v := range variants(t) {
		t.Run(v.name, func(t *testing.T) {
			want := frameFor(t, v, l, 7)
			img, err := v.enc.Encode(want, l, v.depth)
			require.NoError(t, err)

			got, err := encoding.Decode(img, protocol.LocateOptions{})
			require.NoError(t, err)
			require.Equal(t, want.Payload, got.Payload)
			require.Equal(t, v.enc.ID(), got.Header.EncoderID)
			require.Equal(t, v.depth, got.Header.BitDepth)
		})
	}
}

// TestDecodeRefusesForeignFrame checks the mismatch is reported as such rather
// than as corruption, since the receiver distinguishes the two: a mismatch is
// dispatched elsewhere, corruption is retransmitted.
func TestDecodeRefusesForeignFrame(t *testing.T) {
	l := testLayout(t)
	v := variant{"color8", encoding.Color8, 3}

	img, err := encoding.Color8.Encode(frameFor(t, v, l, 1), l, 3)
	require.NoError(t, err)

	_, err = encoding.Binary.Decode(img, protocol.LocateOptions{})
	require.ErrorIs(t, err, encoding.ErrEncoderMismatch)
}

func TestEncodeRejectsBadDepthAndOversizedPayload(t *testing.T) {
	l := testLayout(t)

	_, err := encoding.Color8.EstimateCapacity(l, 2)
	require.ErrorIs(t, err, encoding.ErrUnsupportedBitDepth)

	_, err = encoding.Grayscale.EstimateCapacity(l, 4)
	require.ErrorIs(t, err, encoding.ErrUnsupportedBitDepth)

	c, err := encoding.Binary.EstimateCapacity(l, 1)
	require.NoError(t, err)

	f := protocol.NewFrame(protocol.Header{}, make([]byte, c.PayloadBytes+1))
	require.ErrorIs(t, encoding.Binary.Validate(f, l, 1), protocol.ErrPayloadTooLarge)
	_, err = encoding.Binary.Encode(f, l, 1)
	require.ErrorIs(t, err, protocol.ErrPayloadTooLarge)

	// A header that disagrees with the payload it accompanies is refused rather
	// than rendered, because the resulting frame would decode to a different
	// length than it carries.
	bad := protocol.NewFrame(protocol.Header{}, []byte("payload"))
	bad.Header.PayloadLength = 3
	require.Error(t, encoding.Binary.Validate(bad, l, 1))
}

// TestEncodeIsDeterministic underwrites the golden vectors: the same frame must
// render to the same pixels, or a pinned image proves nothing.
func TestEncodeIsDeterministic(t *testing.T) {
	l := testLayout(t)

	for _, v := range variants(t) {
		t.Run(v.name, func(t *testing.T) {
			first, err := v.enc.Encode(frameFor(t, v, l, 3), l, v.depth)
			require.NoError(t, err)
			second, err := v.enc.Encode(frameFor(t, v, l, 3), l, v.depth)
			require.NoError(t, err)
			require.Equal(t, first.Pix, second.Pix)
		})
	}
}

// TestEncodeFillsHeaderGeometry checks the encoder owns the header fields that
// describe how it rendered. A caller cannot get them wrong, because a caller does
// not set them.
func TestEncodeFillsHeaderGeometry(t *testing.T) {
	l := testLayout(t)
	v := variant{"grayscale", encoding.Grayscale, 3}

	f := frameFor(t, v, l, 5)
	f.Header.EncoderID = 99
	f.Header.BitDepth = 1
	f.Header.GridWidth = 1
	f.Header.GridHeight = 1
	f.Header.CellPixels = 1

	_, err := encoding.Grayscale.Encode(f, l, 3)
	require.NoError(t, err)

	require.Equal(t, encoding.IDGrayscale, f.Header.EncoderID)
	require.Equal(t, uint8(3), f.Header.BitDepth)
	require.Equal(t, uint16(l.GridWidth), f.Header.GridWidth)
	require.Equal(t, uint16(l.GridHeight), f.Header.GridHeight)
	require.Equal(t, uint16(l.CellPixels), f.Header.CellPixels)
	require.Equal(t, protocol.Current, f.Header.Version)
}

// TestDefaultDepthApplies covers depth 0 meaning "whatever the encoding prefers",
// which is how an unset configuration field reaches the encoder.
func TestDefaultDepthApplies(t *testing.T) {
	l := testLayout(t)

	for _, e := range encoding.All() {
		unset, err := e.EstimateCapacity(l, 0)
		require.NoError(t, err)
		explicit, err := e.EstimateCapacity(l, e.DefaultBitDepth())
		require.NoError(t, err)
		require.Equal(t, explicit, unset, "%s: depth 0 means its default", e.Name())
	}
}
