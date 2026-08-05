package encoding_test

import (
	"fmt"
	"image"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// channels are the simulated optical paths every encoding is measured against,
// ordered from a screen grab to a marginal capture.
var channels = []struct {
	name    string
	profile simulate.Profile
}{
	{"pristine", simulate.Pristine},
	{"clean", simulate.Clean},
	{"typical", simulate.Typical},
	{"harsh", simulate.Harsh},
}

// TestOpticalEnvelope measures which encodings survive which optical paths.
//
// The table is the deliverable: it is what tells an operator that a colour camera
// in a controlled room can run color16, while the same camera badly sited has to
// fall back to binary. The assertions afterwards pin only the conclusions the rest
// of the platform depends on, so improving the decoder widens the table rather
// than breaking the test.
func TestOpticalEnvelope(t *testing.T) {
	l := testLayout(t)

	type key struct{ variant, channel string }
	outcome := map[key]bool{}

	var table strings.Builder
	table.WriteString("\ndecode success and payload byte error rate by optical path:\n")
	table.WriteString("  encoding       ")
	for _, ch := range channels {
		fmt.Fprintf(&table, " %-14s", ch.name)
	}
	table.WriteString("\n")

	for _, v := range variants(t) {
		want := frameFor(t, v, l, 11)
		base, err := v.enc.Encode(want, l, v.depth)
		require.NoError(t, err)

		fmt.Fprintf(&table, "  %-14s ", v.name)
		for _, ch := range channels {
			img := ch.profile.Apply(base)
			got, err := encoding.Decode(img, protocol.LocateOptions{})

			ok := err == nil
			outcome[key{v.name, ch.name}] = ok
			switch {
			case ok:
				require.Equal(t, want.Payload, got.Payload,
					"%s over %s: a frame that passed CRC32 and SHA-256 must be byte-identical",
					v.name, ch.name)
				fmt.Fprintf(&table, " %-14s", "ok")
			default:
				// Where the frame failed, report how badly, so a regression that
				// merely narrows the margin is visible before it becomes a failure.
				fmt.Fprintf(&table, " %-14s", fmt.Sprintf("fail %.1f%%", errorRate(t, v, l, want, img)*100))
			}
		}
		table.WriteString("\n")
	}
	t.Log(table.String())

	// Binary and rolling are the fallbacks an operator is told to reach for when
	// the optical path is poor, so they have to hold at the edge of the envelope.
	for _, name := range []string{"binary/d1", "rolling/d1"} {
		for _, ch := range channels {
			require.True(t, outcome[key{name, ch.name}],
				"%s must decode over the %s channel: it is the documented fallback", name, ch.name)
		}
	}

	// Every encoding is offered on the strength of at least a controlled
	// installation. These are the claims the encoding-profile UI makes on their
	// behalf, and the reason the three-bit grey ramp is documented as needing one.
	for _, name := range []string{"grayscale/d2", "grayscale/d3", "color8/d3", "color16/d4"} {
		require.True(t, outcome[key{name, "clean"}],
			"%s must decode over a well-sited camera", name)
	}
	for _, name := range []string{"grayscale/d2", "color8/d3", "color16/d4"} {
		require.True(t, outcome[key{name, "typical"}],
			"%s must decode over a normal industrial installation", name)
	}

	// Colour at the cube corners keeps as much margin as binary while carrying
	// three times as much, which is why it is the default recommendation for a
	// colour camera rather than a fair-weather option.
	require.True(t, outcome[key{"color8/d3", "harsh"}],
		"color8 must hold at the edge of the envelope")
}

// errorRate reports the fraction of payload bytes a failed decode got wrong.
//
// It is measured by re-reading the frame with the payload's own length and
// comparing byte by byte, which is the only way to distinguish a frame that lost
// one cell from one the decoder never located at all.
func errorRate(t *testing.T, v variant, l protocol.Layout, want *protocol.Frame, img image.Image) float64 {
	t.Helper()

	g, err := protocol.Locate(img, protocol.LocateOptions{})
	if err != nil {
		return 1
	}
	// Reading the payload at a located geometry is what Decode does; here the
	// result is compared rather than verified, so a corrupt frame still yields a
	// number.
	got, err := encoding.DecodePayloadAt(v.enc, g, img)
	if err != nil {
		return 1
	}
	if len(got) != len(want.Payload) {
		return 1
	}
	var bad int
	for i := range got {
		if got[i] != want.Payload[i] {
			bad++
		}
	}
	return float64(bad) / float64(len(want.Payload))
}

// TestRollingBandDamageIsLocated checks the rolling encoding's reason for
// existing: a tear across part of the frame is reported as damage to specific
// bands, not as an anonymous bad frame.
func TestRollingBandDamageIsLocated(t *testing.T) {
	l := testLayout(t)
	v := variant{"rolling/d1", encoding.Rolling, 1}

	want := frameFor(t, v, l, 13)
	img, err := encoding.Rolling.Encode(want, l, 1)
	require.NoError(t, err)

	// Tear the payload rows a single band occupies. The timing columns are left
	// alone, because a real tear displaces content rather than destroying the
	// decoder's ability to find the grid, and this test is about what happens
	// after the grid is found.
	const damagedBand = 3
	r := l.PayloadRect()
	ranges := encoding.BandRangesForTest(l.PayloadRows(), l.PayloadCols())
	require.Greater(t, len(ranges), damagedBand+1,
		"the test grid must hold more bands than the one being torn")

	for row := ranges[damagedBand][0]; row < ranges[damagedBand][1]; row++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			box := l.CellRect(protocol.Cell{X: x, Y: r.Min.Y + row})
			for y := box.Min.Y; y < box.Max.Y; y++ {
				for px := box.Min.X; px < box.Max.X; px++ {
					o := img.PixOffset(px, y)
					img.Pix[o+0] ^= 0xFF
					img.Pix[o+1] ^= 0xFF
					img.Pix[o+2] ^= 0xFF
				}
			}
		}
	}

	_, err = encoding.Decode(img, protocol.LocateOptions{})
	require.Error(t, err, "a torn frame must not decode")
	require.ErrorIs(t, err, protocol.ErrPayloadCRC)

	var damage *encoding.BandDamageError
	require.ErrorAs(t, err, &damage, "the failure must say which band tore")
	require.Contains(t, damage.Bands, damagedBand)
	require.Less(t, len(damage.Bands), damage.TotalBands,
		"only the torn band should be reported")
}

// TestIntactFrameReportsNoDamage is the other half of the rule that band
// checksums cannot veto a frame: a frame the footer accepts decodes, and nothing
// about the bands is consulted.
func TestIntactFrameReportsNoDamage(t *testing.T) {
	l := testLayout(t)
	v := variant{"rolling/d1", encoding.Rolling, 1}

	want := frameFor(t, v, l, 17)
	img, err := encoding.Rolling.Encode(want, l, 1)
	require.NoError(t, err)

	got, err := encoding.Decode(simulate.Harsh.Apply(img), protocol.LocateOptions{})
	require.NoError(t, err)
	require.Equal(t, want.Payload, got.Payload)
}
