package encoding_test

import (
	"image"
	"math/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// Reading the payload of every lane, not just the payload of the strongest one.
//
// The lane tests that existed before this one all stop at LocateAll: they prove the receiver finds
// each lane's *geometry* and reads each lane's *header*, which is why they passed while no tiled
// transfer ever completed. Nothing exercised the step after that, where a located lane has its
// payload sampled — and that step was reading the wrong pixels.
//
// These compose lanes the way the sender's composeLanes actually does, with a zero gap and the
// frames' own quiet zones as the only separation, rather than the gap of 2 the protocol tests use.
// The composition under test has to be the composition that ships.

// tiledDisplay renders n distinguishable frames and tiles them as the sender does.
func tiledDisplay(t *testing.T, lanes, grid, cellPixels int) (image.Image, []*protocol.Frame) {
	return tiledDisplayGap(t, lanes, grid, cellPixels, protocol.DefaultLaneGapCells*cellPixels)
}

// tiledDisplayGap is the same with the lane gap chosen explicitly, for tests about the gap itself.
func tiledDisplayGap(t *testing.T, lanes, grid, cellPixels, gapPx int) (image.Image, []*protocol.Frame) {
	t.Helper()

	enc := encoding.Color8
	depth := enc.DefaultBitDepth()

	layout, err := protocol.NewLayoutQuiet(grid, grid, cellPixels, protocol.DefaultQuietZone)
	require.NoError(t, err)

	capacity, err := enc.EstimateCapacity(layout, depth)
	require.NoError(t, err)

	transmission, session := uuid.New(), uuid.New()

	var images []image.Image
	var want []*protocol.Frame
	for i := 0; i < lanes; i++ {
		// Distinct payloads, because the failure this catches returns a real frame that decodes and
		// verifies — just the same one every time. Identical payloads would hide it completely.
		payload := make([]byte, capacity.PayloadBytes)
		rand.New(rand.NewSource(int64(i) + 1)).Read(payload)

		f := protocol.NewFrame(protocol.Header{
			Flags:          protocol.FlagLastChunk,
			CompressionID:  2,
			FECID:          1,
			TransmissionID: transmission,
			SessionID:      session,
			FrameNumber:    uint32(100 + i),
			TotalFrames:    uint32(lanes),
			ChunkNumber:    uint32(i),
			TotalChunks:    uint32(lanes),
			TimestampMS:    1754300000000,
		}, payload)

		img, err := enc.Encode(f, layout, depth)
		require.NoError(t, err)

		images = append(images, img)
		want = append(want, f)
	}

	// Exactly what sender/internal/scheduler/lanes.go composeLanes builds: the lane geometry taken
	// from the rendered image's own bounds, one pixel per "cell", no quiet zone, and the gap in
	// pixels of the frame's own cell size.
	first := images[0].Bounds()
	tiled, err := protocol.NewLaneLayout(protocol.Layout{
		GridWidth:  first.Dx(),
		GridHeight: first.Dy(),
		CellPixels: 1,
		QuietZone:  0,
	}, lanes, gapPx)
	require.NoError(t, err)

	display, err := tiled.Compose(images)
	require.NoError(t, err)
	return display, want
}

// Every lane's own payload comes back, at 2 lanes and at 4.
//
// This is the whole point of tiling and it is what was broken: the receiver located each lane
// correctly and then sampled the payload from whichever lane the full-image search happened to find,
// so the lanes after the first carried a duplicate of the first lane's chunk under their own
// header — or, when the full-image search picked a quad spanning two lanes and failed its descriptor
// CRC, carried nothing at all and the transfer decoded zero frames.
func TestEveryLaneDecodesItsOwnPayload(t *testing.T) {
	for _, lanes := range []int{2, 4} {
		t.Run(laneName(lanes), func(t *testing.T) {
			const grid, cellPixels = 80, 8

			display, want := tiledDisplay(t, lanes, grid, cellPixels)

			opts := protocol.LocateOptions{CellPixelsHint: cellPixels}
			found := protocol.LocateAll(display, opts, lanes)
			require.Len(t, found, lanes, "every lane should be located in one capture")

			got := map[uint32][]byte{}
			for _, g := range found {
				frame, err := encoding.DecodeAt(g, display, opts)
				require.NoError(t, err, "a located lane must decode at its own geometry")
				got[frame.Header.FrameNumber] = frame.Payload
			}

			require.Len(t, got, lanes, "each lane must yield its own frame, not one frame %d times", lanes)
			for _, w := range want {
				assert.Equal(t, w.Payload, got[w.Header.FrameNumber],
					"frame %d must carry its own payload", w.Header.FrameNumber)
			}
		})
	}
}

// DecodeAt reads the lane it is given rather than the lane the picture leads with.
//
// Stated separately from the test above because this is the precise property the receiver's lane
// path needs and the precise one the whole-image Decode cannot offer: handed the same picture twice
// it must return two different frames, chosen by the geometry argument.
func TestDecodeAtReadsTheGeometryItIsGiven(t *testing.T) {
	const lanes, grid, cellPixels = 2, 80, 8

	display, _ := tiledDisplay(t, lanes, grid, cellPixels)

	opts := protocol.LocateOptions{CellPixelsHint: cellPixels}
	found := protocol.LocateAll(display, opts, lanes)
	require.Len(t, found, lanes)

	first, err := encoding.DecodeAt(found[0], display, opts)
	require.NoError(t, err)
	second, err := encoding.DecodeAt(found[1], display, opts)
	require.NoError(t, err)

	assert.NotEqual(t, first.Header.FrameNumber, second.Header.FrameNumber,
		"two lanes of one picture must not decode to the same frame")
	assert.NotEqual(t, first.Payload, second.Payload,
		"two lanes of one picture must not carry the same payload")
}

// Every lane survives a simulated camera, not just a clean render.
//
// The tests above compose and read the encoder's own pixels, which proves the geometry is carried
// back into capture coordinates correctly but says nothing about a photograph. This one puts the
// tiled display through the same degradation profiles the single-frame tests use — defocus, noise,
// a few degrees off square, vignetting, JPEG — and requires every lane back with its own payload.
//
// It is the test that speaks to the rig: a phone showing tiles and a laptop camera reading them.
func TestEveryLaneSurvivesTheOpticalChannel(t *testing.T) {
	profiles := []struct {
		name    string
		profile simulate.Profile
	}{
		{"clean", simulate.Clean},
		{"typical", simulate.Typical},
	}

	for _, lanes := range []int{2, 4} {
		for _, p := range profiles {
			t.Run(laneName(lanes)+", "+p.name, func(t *testing.T) {
				const grid, cellPixels = 80, 8

				display, want := tiledDisplay(t, lanes, grid, cellPixels)
				captured := p.profile.Apply(display)

				// No cell-size hint: a receiver photographing a display it has not calibrated against
				// does not have one, and the hint would paper over a geometry that only resolves
				// because it was told the answer.
				found := protocol.LocateAll(captured, protocol.LocateOptions{}, lanes)
				require.Len(t, found, lanes, "every lane should be located through the camera")

				got := map[uint32][]byte{}
				for _, g := range found {
					frame, err := encoding.DecodeAt(g, captured, protocol.LocateOptions{})
					if err != nil {
						continue
					}
					got[frame.Header.FrameNumber] = frame.Payload
				}

				require.Len(t, got, lanes, "every lane should decode through the camera")
				for _, w := range want {
					assert.Equal(t, w.Payload, got[w.Header.FrameNumber],
						"frame %d must carry its own payload through the camera", w.Header.FrameNumber)
				}
			})
		}
	}
}

// Flush lanes read from the encoder's own pixels and fail through a camera. The gap is what fixes it.
//
// This is the shape of bug that ships. Composed flush, two lanes decode perfectly from the rendered
// image, so every test that read the encoder's output directly passed and the tiling looked finished;
// through any camera at all, nothing decoded, because the receiver reads a lane by cropping around
// its fiducials and that crop lands on the neighbour when the neighbour is touching.
//
// Both halves are asserted together so the constant cannot be quietly lowered back to zero: if flush
// lanes ever start surviving the camera, the reason for the gap has changed and this should be
// re-measured rather than assumed.
func TestFlushLanesFailThroughACameraAndAGapFixesIt(t *testing.T) {
	const lanes, grid, cellPixels = 2, 80, 8

	require.Positive(t, protocol.DefaultLaneGapCells, "a tiling needs a gap between lanes")

	flush, _ := tiledDisplayGap(t, lanes, grid, cellPixels, 0)
	gapped, _ := tiledDisplay(t, lanes, grid, cellPixels)

	// Flush lanes are fine on the encoder's own pixels. That is exactly why this was missed.
	assert.Len(t, protocol.LocateAll(flush, protocol.LocateOptions{}, lanes), lanes,
		"flush lanes should still read from the rendered image, which is what hid this")

	captured := func(img image.Image) []*protocol.Geometry {
		return protocol.LocateAll(simulate.Typical.Apply(img), protocol.LocateOptions{}, lanes)
	}

	assert.Empty(t, captured(flush),
		"flush lanes are expected to fail through a camera; if they no longer do, re-measure the gap")
	assert.Len(t, captured(gapped), lanes,
		"a gap of %d cells should carry both lanes through a camera", protocol.DefaultLaneGapCells)
}

func laneName(lanes int) string {
	switch lanes {
	case 2:
		return "two lanes"
	case 4:
		return "four lanes"
	default:
		return "lanes"
	}
}
