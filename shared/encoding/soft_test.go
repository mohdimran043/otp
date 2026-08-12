package encoding_test

import (
	"image"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// softRender renders a real colour8 frame at grid 80x80 with 8 px cells.
//
// That geometry rather than an arbitrary one because it is the configuration that achieved a
// byte-exact camera transfer on this project, so a regression here is a regression in something
// known to work rather than in a hypothetical.
func softRender(t *testing.T, payloadLen int) (*protocol.Frame, protocol.Layout, []byte, *protocol.Geometry, *image.RGBA) {
	t.Helper()

	l, err := protocol.NewLayout(80, 80, 8)
	require.NoError(t, err)

	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	f := protocol.NewFrame(protocol.Header{TransmissionID: uuid.New()}, payload)

	img, err := encoding.Color8.Encode(f, l, 3)
	require.NoError(t, err)

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	require.NoError(t, err)

	return f, l, payload, g, img
}

// TestSoftReadRoundTrip is the baseline: on a pristine render, the symbols read back verify
// against the footer and reproduce the payload exactly.
func TestSoftReadRoundTrip(t *testing.T) {
	_, _, payload, g, img := softRender(t, 300)

	r, err := encoding.SoftRead(g, img)
	require.NoError(t, err)
	require.Equal(t, 3, r.BitsPerCell)
	require.Len(t, r.Cells, len(r.Symbols))

	got, err := r.Verify(r.Symbols)
	require.NoError(t, err)
	require.Equal(t, payload, got.Payload)
}

// TestSoftReadPristineMarginsAreWide records what a clean render looks like, so a regression in
// the photometric normalisation shows up here rather than as a mysterious drop in recovery rate.
func TestSoftReadPristineMarginsAreWide(t *testing.T) {
	_, _, _, g, img := softRender(t, 300)

	r, err := encoding.SoftRead(g, img)
	require.NoError(t, err)

	worst := r.Cells[0].Margin
	for _, c := range r.Cells {
		if c.Margin < worst {
			worst = c.Margin
		}
	}
	t.Logf("worst margin on a pristine colour8 render: %.1f (separation %.1f)",
		worst, encoding.Color8Palette.MinSeparation())
	require.Greater(t, worst, encoding.Color8Palette.MinSeparation()/2,
		"a pristine render should not have marginal cells")
}

// TestSoftReadMarginsNarrowUnderDegradation is the other half: a degraded capture must produce
// cells the reader is unsure about, or there is nothing for a candidate search to rank and the
// whole idea has no purchase.
//
// The geometry here is 4 px cells, not the 8 px the other cases use, and that is the finding
// rather than an arbitrary choice. Measured across the profiles at grids 80 and 128:
//
//	cell  Clean      Typical              Harsh
//	8     0 marginal 0 marginal           0 marginal, still decodes
//	4     0 marginal 147-893 marginal     does not locate
//	3     0 marginal does not locate      does not locate
//
// At 8 px the sampler averages a window a quarter of a cell wide about the cell's centre, blur
// never reaches it, and the photometric fit snaps every level back onto its palette entry — the
// worst margin on a whole frame is the full separation. Uncertainty needs a cell small enough
// that its neighbours bleed into the sampling window, and by 3 px the fiducials have gone too and
// there is no geometry at all. Four pixels a cell is the only rung of that ladder where the read
// is uncertain and the frame is still locatable.
//
// The consequence for testing recovery is worth stating: at no locatable geometry do these
// profiles produce a frame that *fails* its payload with only a few bad cells. They either decode
// or they lose the fiducials. Reproducible recoverable failures therefore have to be planted
// deliberately, which is what receiver/ai/soft's tests do.
func TestSoftReadMarginsNarrowUnderDegradation(t *testing.T) {
	l, err := protocol.NewLayout(80, 80, 4)
	require.NoError(t, err)

	f := protocol.NewFrame(protocol.Header{TransmissionID: uuid.New()}, make([]byte, 300))
	clean, err := encoding.Color8.Encode(f, l, 3)
	require.NoError(t, err)

	degraded := simulate.Typical.Apply(clean)
	g, err := protocol.Locate(degraded, protocol.LocateOptions{ExpectedLayout: &l})
	if err != nil {
		t.Skipf("the typical profile did not locate at 80x80 @4px: %v", err)
	}

	r, err := encoding.SoftRead(g, degraded)
	require.NoError(t, err)

	sep := encoding.Color8Palette.MinSeparation()
	var marginal int
	worst := sep
	for _, c := range r.Cells {
		if c.Margin < sep/4 {
			marginal++
		}
		if c.Margin < worst {
			worst = c.Margin
		}
	}
	t.Logf("%d of %d cells marginal under the typical profile at 4 px cells, worst margin %.1f of %.1f",
		marginal, len(r.Cells), worst, sep)
	require.Positive(t, marginal, "a degraded capture should leave some cells uncertain")
}

// TestSoftReadFailsOnUnreadableFooter records why SoftRead reads the footer up front: the footer
// is the oracle every candidate is checked against, so a frame whose footer cannot be read is
// unrecoverable by construction and must say so immediately rather than after thousands of
// pointless candidates.
func TestSoftReadFailsOnUnreadableFooter(t *testing.T) {
	l, err := protocol.NewLayout(80, 80, 8)
	require.NoError(t, err)

	f := protocol.NewFrame(protocol.Header{TransmissionID: uuid.New()}, make([]byte, 300))
	img, err := encoding.Color8.Encode(f, l, 3)
	require.NoError(t, err)

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	require.NoError(t, err)

	// Flatten the footer band, leaving the fiducials and the header intact so the geometry still
	// resolves and the failure is attributable to the footer alone.
	for y := l.GridHeight - l.FooterBandRows; y < l.GridHeight; y++ {
		for x := protocol.FinderBox; x < l.GridWidth-protocol.FinderBox; x++ {
			rect := l.CellRect(protocol.Cell{X: x, Y: y})
			for py := rect.Min.Y; py < rect.Max.Y; py++ {
				for px := rect.Min.X; px < rect.Max.X; px++ {
					img.Set(px, py, encoding.Color8Palette.Colors[3])
				}
			}
		}
	}

	_, err = encoding.SoftRead(g, img)
	require.Error(t, err)
}
