package encoding_test

import (
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// TestCellSizeRoundTrip is task 7's load-bearing check. The sender now picks cell
// size per transfer instead of always using the deployment's configured default,
// specifically so a grid can be shrunk to fit a real panel instead of being
// cropped: a 512 grid at the default 8 px/cell renders 4128 px, four times too
// big for a 1080p panel, but at 2 px/cell it renders 1032 px. This proves the
// encoder and the decoder actually agree at the resulting cell sizes — that
// combination, and the more aggressive 1024 grid at 1 px/cell (1028 px).
//
// The round trip goes through an actual file on disk, not just an in-memory
// image.Image, because that is the path a rendered frame really takes: the
// pipeline writes a PNG to the object store, and a receiver reads a PNG back.
//
// What this test does NOT prove is that a camera can read the result. Applying
// shared/simulate's Clean and Typical profiles to these same renders — the same
// profiles TestPipelineSurvivesTheOpticalChannel and TestDecodeCost use to judge
// real capture — fails to decode at both 1 and 2 px/cell, and even at 4 px/cell
// under Typical, matching TestDecodeCost's documented finding that a typical
// optical path's blur exceeds a quarter of a cell width at 4 px/cell. The
// codebase's only camera-validated cell size remains 8 px (config.Default's
// CellPixels). So "round-trips" here means "the frame is correctly formed and
// self-consistent," not "a real panel and camera can carry it" — see the task-7
// report for the full finding, since task 9's UI floor has to account for it.
func TestCellSizeRoundTrip(t *testing.T) {
	enc := encoding.Color8
	depth := enc.DefaultBitDepth()

	cases := []struct {
		name             string
		grid, cellPixels int
	}{
		{"512 grid at 2 px per cell", 512, 2},
		{"1024 grid at 1 px per cell", 1024, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layout, err := protocol.NewLayoutQuiet(tc.grid, tc.grid, tc.cellPixels, protocol.DefaultQuietZone)
			require.NoError(t, err)

			capacity, err := enc.EstimateCapacity(layout, depth)
			require.NoError(t, err)

			// Run it a few times with different payloads: one lucky bit pattern
			// decoding would not be evidence that the geometry itself is sound at
			// this cell size, only that this particular payload survived.
			for seed := int64(1); seed <= 3; seed++ {
				payload := make([]byte, capacity.PayloadBytes)
				rand.New(rand.NewSource(seed)).Read(payload)

				want := protocol.NewFrame(protocol.Header{
					Flags:          protocol.FlagLastChunk,
					CompressionID:  2,
					FECID:          1,
					TransmissionID: uuid.New(),
					SessionID:      uuid.New(),
					FrameNumber:    3,
					TotalFrames:    9,
					ChunkNumber:    3,
					TotalChunks:    9,
					TimestampMS:    1754300000000,
				}, payload)

				img, err := enc.Encode(want, layout, depth)
				require.NoError(t, err)
				require.Equal(t, layout.ImageWidth(), img.Bounds().Dx())
				require.Equal(t, layout.ImageHeight(), img.Bounds().Dy())

				// File to file: an actual PNG on disk, not an image.Image kept in
				// memory, matching how a rendered frame is really handed from the
				// pipeline to a decoder.
				path := filepath.Join(t.TempDir(), "frame.png")
				out, err := os.Create(path)
				require.NoError(t, err)
				require.NoError(t, png.Encode(out, img))
				require.NoError(t, out.Close())

				stored, err := os.Open(path)
				require.NoError(t, err)
				loaded, decodeErr := png.Decode(stored)
				require.NoError(t, stored.Close())
				require.NoError(t, decodeErr)

				got, err := encoding.Decode(loaded, protocol.LocateOptions{})
				require.NoError(t, err, "seed %d must decode", seed)
				require.Equal(t, want.Payload, got.Payload, "seed %d payload must round-trip byte-for-byte", seed)
				require.Equal(t, want.Header, got.Header, "seed %d header must round-trip", seed)
			}
		})
	}
}
