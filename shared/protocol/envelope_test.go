package protocol_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// TestOperatingEnvelope maps where decoding stops working.
//
// The result is deployment guidance rather than a pass-or-fail check: it says how
// sharp a lens and how quiet a sensor an installation needs for a given cell size,
// which is what determines how far a camera can sit from the display and what
// optics it needs. The assertions at the end only pin the conclusions the rest of
// the system relies on, so tightening the decoder widens the table without
// breaking the test.
func TestOperatingEnvelope(t *testing.T) {
	const (
		gridW = 96
		gridH = 96
	)

	type result struct {
		cellPx int
		ratio  float64
		ok     bool
	}
	var results []result

	var table strings.Builder
	table.WriteString("\nblur sigma as a fraction of cell width, decode success:\n")
	table.WriteString("  cell |")
	ratios := []float64{0.05, 0.10, 0.15, 0.20, 0.25, 0.30, 0.35}
	for _, r := range ratios {
		fmt.Fprintf(&table, " %4.2f", r)
	}
	table.WriteString("\n")

	for _, cellPx := range []int{4, 6, 8, 12, 16} {
		l, err := protocol.NewLayout(gridW, gridH, cellPx)
		require.NoError(t, err)

		want := sampleHeader()
		want.GridWidth, want.GridHeight, want.CellPixels = gridW, gridH, uint16(cellPx)
		base := renderBands(t, l, want)

		fmt.Fprintf(&table, "  %4d |", cellPx)
		for _, ratio := range ratios {
			img := simulate.Profile{
				BlurSigma:  float64(cellPx) * ratio,
				NoiseSigma: 6,
				Pad:        0.05,
				Seed:       21,
			}.Apply(base)

			g, err := protocol.Locate(img, protocol.LocateOptions{})
			ok := err == nil && g.Header == want
			results = append(results, result{cellPx, ratio, ok})
			if ok {
				table.WriteString("    +")
			} else {
				table.WriteString("    .")
			}
		}
		table.WriteString("\n")
	}

	table.WriteString("\nnoise sigma at cell width 8, decode success:\n  ")
	l, err := protocol.NewLayout(gridW, gridH, 8)
	require.NoError(t, err)
	want := sampleHeader()
	want.GridWidth, want.GridHeight, want.CellPixels = gridW, gridH, 8
	base := renderBands(t, l, want)

	noiseOK := map[float64]bool{}
	for _, noise := range []float64{0, 8, 16, 24, 32, 48, 64} {
		img := simulate.Profile{BlurSigma: 0.8, NoiseSigma: noise, Pad: 0.05, Seed: 22}.Apply(base)
		g, err := protocol.Locate(img, protocol.LocateOptions{})
		ok := err == nil && g.Header == want
		noiseOK[noise] = ok
		mark := "."
		if ok {
			mark = "+"
		}
		fmt.Fprintf(&table, "sigma %2.0f %s   ", noise, mark)
	}
	table.WriteString("\n")
	t.Log(table.String())

	// Blur up to a fifth of a cell must decode at every usable cell size. This is
	// the figure the Harsh simulation profile is calibrated against and the one
	// the deployment guide quotes.
	for _, r := range results {
		if r.ratio <= 0.20 && r.cellPx >= 6 {
			require.True(t, r.ok,
				"blur at %.0f%% of a %dpx cell should decode; if this regressed, the deployment guide is now wrong",
				r.ratio*100, r.cellPx)
		}
	}

	// The denoise fallback exists to hold this line.
	require.True(t, noiseOK[24], "sensor noise of sigma 24 should decode at an 8px cell")
}
