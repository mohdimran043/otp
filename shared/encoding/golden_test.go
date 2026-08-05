package encoding_test

import (
	"bytes"
	"flag"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// update regenerates the golden frames instead of checking against them. Doing so
// is a wire-format change: a stored frame that no longer renders identically is a
// frame an already-deployed receiver can no longer read.
var update = flag.Bool("update", false, "rewrite the golden frames in testdata")

// goldenLayout is a realistic grid at the smallest cell size, so the stored frames
// stay small enough to be worth keeping in the repository while still exercising
// every region of the layout.
//
// It is not the minimum grid the layout permits: at 48 cells wide, three copies of
// a 92-byte header record need more rows than the whole grid has, so the smallest
// square grid is not a usable one. That is the layout's business rather than the
// encoders', and TestLayoutRejectsBadGeometry covers it.
func goldenLayout(t *testing.T) protocol.Layout {
	t.Helper()
	l, err := protocol.NewLayout(96, 96, 4)
	require.NoError(t, err)
	return l
}

// goldenFrame is a fixed frame: no timestamps, no random payload, nothing that
// varies between runs or machines.
func goldenFrame(t *testing.T, v variant, l protocol.Layout) *protocol.Frame {
	t.Helper()
	capacity, err := v.enc.EstimateCapacity(l, v.depth)
	require.NoError(t, err)

	payload := make([]byte, capacity.PayloadBytes)
	for i := range payload {
		payload[i] = byte(i*31 + i/7 + 11)
	}

	return protocol.NewFrame(protocol.Header{
		Flags:          protocol.FlagLastChunk | protocol.FlagEndOfStream,
		CompressionID:  3,
		FECID:          1,
		TransmissionID: uuid.MustParse("6f9619ff-8b86-d011-b42d-00cf4fc964ff"),
		SessionID:      uuid.MustParse("9c1e5f42-0d3a-4b21-8f77-2b1d5a6c9e01"),
		FrameNumber:    3,
		TotalFrames:    9,
		ChunkNumber:    3,
		TotalChunks:    9,
		TimestampMS:    1754300000000,
	}, payload)
}

// TestGoldenFrames pins the rendered output of every encoding.
//
// The wire-record vectors in the protocol package cover what a frame *says*; this
// covers what it *looks like*, which is the part a camera actually sees. Between
// them they close the gap a round-trip test leaves: an encoder and decoder changed
// together keep agreeing with each other while silently losing the ability to read
// anything rendered by the version already deployed at the other end of the room.
//
// The comparison is on pixels rather than on the PNG bytes, so it survives a change
// to Go's PNG encoder — the stored files exist to be looked at as well as compared,
// and a golden test nobody can inspect is one nobody trusts.
func TestGoldenFrames(t *testing.T) {
	l := goldenLayout(t)

	for _, v := range variants(t) {
		t.Run(v.name, func(t *testing.T) {
			got, err := v.enc.Encode(goldenFrame(t, v, l), l, v.depth)
			require.NoError(t, err)

			path := filepath.Join("testdata", "golden",
				v.enc.Name()+"-d"+string(rune('0'+v.depth))+".png")

			if *update {
				var buf bytes.Buffer
				require.NoError(t, png.Encode(&buf, got))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
				t.Logf("wrote %s", path)
				return
			}

			f, err := os.Open(path)
			require.NoError(t, err, "missing golden frame; regenerate with -update if the change is intended")
			defer f.Close()

			stored, err := png.Decode(f)
			require.NoError(t, err)
			require.Equal(t, got.Bounds(), stored.Bounds(), "the frame changed size")

			requireSamePixels(t, got, stored)

			// The stored frame must also still decode, which is the property that
			// actually matters: it proves the pinned bytes are a frame this build can
			// read, not merely bytes this build happens to produce.
			decoded, err := encoding.Decode(stored, protocol.LocateOptions{})
			require.NoError(t, err)
			require.Equal(t, goldenFrame(t, v, l).Payload, decoded.Payload)
		})
	}
}

// requireSamePixels compares two images and reports the first difference with its
// position, since a diff of a quarter of a million pixels is unreadable.
func requireSamePixels(t *testing.T, want *image.RGBA, got image.Image) {
	t.Helper()
	b := want.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			wr, wg, wb, _ := want.At(x, y).RGBA()
			gr, gg, gb, _ := got.At(x, y).RGBA()
			if wr != gr || wg != gg || wb != gb {
				t.Fatalf("pixel (%d,%d) differs: rendered %v, stored %v — the rendered frame changed",
					x, y, want.At(x, y), got.At(x, y))
			}
		}
	}
}
