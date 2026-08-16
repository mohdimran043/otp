package encoding_test

import (
	"fmt"
	"testing"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// What the capture format costs a colour payload.
//
// The browser photographs the display into a canvas and posts it as JPEG. That is a reasonable
// default for a photograph and a poor one for this payload: JPEG subsamples chroma, storing colour at
// half resolution in each direction, and colour8 carries its symbols *entirely* in colour — every
// symbol is a corner of the RGB cube. A cell at ten pixels therefore has its luminance recorded at ten
// pixels and the thing that actually distinguishes it recorded at five.
//
// This measures that directly: the same degraded capture, encoded losslessly and as JPEG, read back
// and compared on the fraction of cells left ambiguous. It matters because the fraction is what
// decides whether anything downstream can read the frame — recovery corrects a handful of cells, so
// the difference between 3% ambiguous and 30% is the difference between a transfer and a stall.

// TestJPEGChromaSubsamplingCostsAColourPayload compares capture formats on the same picture.
func TestJPEGChromaSubsamplingCostsAColourPayload(t *testing.T) {
	const lanes, grid, cellPixels = 2, 80, 8

	display, want := tiledDisplay(t, lanes, grid, cellPixels)

	// The same optical path either way — defocus, noise, a few degrees off square. Only the format
	// the camera writes differs, which is the one variable under test.
	// Swept across the marginal band rather than measured at one point. At a comfortable geometry
	// this channel is bimodal — every cell clean or the fiducials gone — so a single operating point
	// shows nothing whichever way the answer falls. The interesting question is what the format costs
	// where the channel is already struggling, which is where every real capture in this project sits.
	type point struct {
		name    string
		profile simulate.Profile
	}
	var points []point
	for _, blur := range []float64{1.6, 2.0, 2.4, 2.8} {
		base := simulate.Profile{
			BlurSigma: blur, NoiseSigma: 10, Tilt: 0.06, Rotation: 1.5, Pad: 0.08,
			Brightness: -6, Gamma: 1.1, Vignette: 0.15, Seed: 2,
		}
		lossless := base
		lossless.JPEGQuality = 0
		jpeg92 := base
		jpeg92.JPEGQuality = 92

		points = append(points,
			point{name: fmt.Sprintf("blur %.1f lossless", blur), profile: lossless},
			point{name: fmt.Sprintf("blur %.1f jpeg 92", blur), profile: jpeg92},
		)
	}

	for _, tc := range points {
		t.Run(tc.name, func(t *testing.T) {
			captured := tc.profile.Apply(display)
			opts := protocol.LocateOptions{}

			found := protocol.LocateAll(captured, opts, lanes)
			if len(found) == 0 {
				t.Logf("%-22s no lanes located", tc.name)
				return
			}

			var read, totalCells, ambiguousCells int
			for _, g := range found {
				if _, err := encoding.DecodeAt(g, captured, opts); err == nil {
					read++
				}
				soft, err := encoding.SoftRead(g, captured)
				if err != nil {
					continue
				}
				for _, c := range soft.Cells {
					totalCells++
					// Half the palette separation. colour8's corners sit 86 apart, so 40 is a sample
					// that a small perturbation moves across the boundary.
					if c.Margin < 40 {
						ambiguousCells++
					}
				}
			}

			pct := 0.0
			if totalCells > 0 {
				pct = float64(ambiguousCells) / float64(totalCells) * 100
			}
			t.Logf("%-22s located %d/%d  read %d/%d  ambiguous %.2f%%",
				tc.name, len(found), lanes, read, lanes, pct)
			_ = want
		})
	}
}
