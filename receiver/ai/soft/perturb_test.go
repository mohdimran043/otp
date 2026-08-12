package soft_test

import (
	"image"
	"image/color"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// cellPresets and gridPresets mirror the sender's own dropdown, so these tests exercise the
// geometries an operator can actually select rather than arbitrary ones.
// See sender/web/src/pages/NewTransfer.tsx.
var (
	gridPresets = []int{80, 96, 128, 192, 256, 384, 512, 1024}
	cellPresets = []int{1, 2, 3, 4, 6, 8}
)

// usableEdge is the display edge the sender fits a frame into. 1080 px stands for the short side
// of a 1080p panel, which is what the browser's own chooser measures at run time.
const usableEdge = 1080

// cellFor is the largest offered cell size whose frame fits the display, mirroring the rule in the
// sender's fitGridAndCell: screen area is free once the grid is chosen, and a bigger cell is what a
// camera needs to resolve. Zero when no offered size fits.
func cellFor(grid int) int {
	best := 0
	for _, c := range cellPresets {
		if (grid+2*protocol.DefaultQuietZone)*c <= usableEdge {
			best = c
		}
	}
	return best
}

// cubeNeighbours lists, for each colour8 symbol, the symbols one channel away from it — the other
// end of a cube edge.
//
// The plant has to move a cell along an *edge* of the RGB cube, and getting this wrong is what
// makes the difference between a realistic marginal cell and a meaningless one. Nudging white 55%
// of the way toward blue lands at (115,115,255), whose two nearest entries by the palette's
// weighted metric are blue at 108 and magenta at 117 — with the original white only third, at 132.
// The line crosses the cube's interior and passes nearer magenta than white, so a search that
// corrects toward the second-nearest entry produces magenta and is right to: white was never a
// plausible reading of that sample.
//
// Along a cube edge no third corner intrudes. Black to red passes through (127,0,0), whose nearest
// two are exactly black and red at 69 each, with the closest other corner at 207. That is the
// geometry a real marginal cell has, and it is also why correcting toward the second-nearest entry
// is sound rather than merely cheap: a small margin means the sample sits near a Voronoi boundary,
// and the two cells sharing a boundary are by definition the two nearest.
var cubeNeighbours = map[uint32][]uint32{
	0: {1, 2, 3}, // black  -> red, green, blue
	1: {0, 5, 6}, // red    -> black, magenta, yellow
	2: {0, 4, 6}, // green  -> black, cyan, yellow
	3: {0, 4, 5}, // blue   -> black, cyan, magenta
	4: {2, 3, 7}, // cyan   -> green, blue, white
	5: {1, 3, 7}, // magenta-> red, blue, white
	6: {1, 2, 7}, // yellow -> red, green, white
	7: {4, 5, 6}, // white  -> cyan, magenta, yellow
}

// nudge moves a cell's pixels a fraction of the way from their own palette colour toward another,
// so the nearest-neighbour match flips while the margin left behind stays small.
//
// This is what a marginal capture actually does to a cell, and it is reproducible in a way a
// blur-and-noise profile is not. That matters more than it sounds: measured across the simulate
// profiles, at every geometry that still locates its fiducials a colour8 frame either decodes
// cleanly or loses the fiducials entirely — there is no profile that produces a frame failing its
// payload by a handful of cells. So a test that wants to claim "recovered a frame with three bad
// cells" has to plant those three cells itself, or it is making a claim about the simulator.
func nudge(t *testing.T, img *image.RGBA, l protocol.Layout, cell protocol.Cell, toward color.RGBA, fraction float64) {
	t.Helper()
	rect := l.CellRect(cell)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			from := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
			img.Set(x, y, color.RGBA{
				R: mix(from.R, toward.R, fraction),
				G: mix(from.G, toward.G, fraction),
				B: mix(from.B, toward.B, fraction),
				A: 255,
			})
		}
	}
}

func mix(a, b uint8, f float64) uint8 {
	v := float64(a) + (float64(b)-float64(a))*f
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	return uint8(v + 0.5)
}

// plantedAt renders a colour8 frame at the given grid and pushes n payload cells just past the
// decision boundary toward a neighbouring palette entry.
//
// Reports false when no offered cell size fits the display at that grid, so a caller can skip with
// a reason rather than testing a geometry the sender would never produce.
func plantedAt(t *testing.T, grid, n int) (*image.RGBA, protocol.Layout, []byte, bool) {
	t.Helper()

	cell := cellFor(grid)
	if cell == 0 {
		return nil, protocol.Layout{}, nil, false
	}
	l, err := protocol.NewLayout(grid, grid, cell)
	require.NoError(t, err)

	capacity, err := encoding.Color8.EstimateCapacity(l, 3)
	require.NoError(t, err)

	// Sized from the encoder's own capacity rather than fixed: grid 1024 carries hundreds of
	// kilobytes, and a fixed 300 bytes would leave almost the whole payload region as padding —
	// which decodes but says nothing about a real frame.
	payload := make([]byte, capacity.PayloadBytes)
	for i := range payload {
		payload[i] = byte(i * 11)
	}

	f := protocol.NewFrame(protocol.Header{}, payload)
	img, err := encoding.Color8.Encode(f, l, 3)
	require.NoError(t, err)

	// 0.55 puts the sample just over halfway along a cube edge toward a neighbouring symbol: the
	// match flips, and the margin left behind is a tenth of the edge length, which is exactly the
	// cell a search should rank first.
	cells := l.PayloadCells()
	for i := 0; i < n && i < len(cells); i++ {
		// Spread the plants across the region rather than clustering them, so the photometric
		// surface fit is not itself distorted by them.
		idx := (i*len(cells)/maxInt(n, 1) + 37) % len(cells)
		cell := cells[idx]

		// The target depends on what the cell already holds, so read it back rather than assuming.
		// A cell nudged toward the colour it already is would not flip at all, and the test would
		// then be asserting recovery of a frame that never broke.
		rect := l.CellRect(cell)
		current := color.RGBAModel.Convert(img.At(rect.Min.X+rect.Dx()/2, rect.Min.Y+rect.Dy()/2)).(color.RGBA)
		symbol := encoding.Color8Palette.Value(current)
		neighbours := cubeNeighbours[symbol]
		toward := encoding.Color8Palette.Colors[neighbours[i%len(neighbours)]]

		nudge(t, img, l, cell, toward, 0.55)
	}
	return img, l, payload, true
}

// planted is plantedAt at grid 80, the geometry that achieved a byte-exact camera transfer here.
func planted(t *testing.T, n int) (*image.RGBA, protocol.Layout, []byte) {
	t.Helper()
	img, l, payload, ok := plantedAt(t, 80, n)
	require.True(t, ok, "grid 80 must fit the display")
	return img, l, payload
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
