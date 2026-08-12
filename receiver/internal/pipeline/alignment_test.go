package pipeline

import (
	"image"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

// frameAt builds a geometry whose fiducials sit on a square of the given side, centred in a
// 1000x1000 capture. Everything the aiming advice keys on is derived from those four points, so
// this is enough to drive every branch.
func frameAt(side float64) *protocol.Geometry {
	const centre = 500.0
	half := side / 2
	return &protocol.Geometry{
		Corners: [4]protocol.Point{
			{X: centre - half, Y: centre - half},
			{X: centre + half, Y: centre - half},
			{X: centre - half, Y: centre + half},
			{X: centre + half, Y: centre + half},
		},
		ModuleSize:  8,
		FinderScore: 1,
		TimingScore: 1,
	}
}

func capture() image.Image { return image.NewRGBA(image.Rect(0, 0, 1000, 1000)) }

// Nothing found is "searching", not an error. A camera pointed at a wall reports this, and so does
// every camera before it is aimed; presenting it as a failure would train an operator to ignore it.
func TestAlignmentReportsSearchingWithoutGeometry(t *testing.T) {
	a := measureAlignment(capture(), nil, false)
	assert.Equal(t, StatusSearching, a.Status)
	assert.False(t, a.Locked)
	assert.NotEmpty(t, a.Advice)
}

func TestAlignmentDistanceAdvice(t *testing.T) {
	tests := []struct {
		name string
		side float64
		want AlignmentStatus
	}{
		{"a grid lost in the view is too far", 200, StatusTooFar},
		{"a grid about to lose a corner is too close", 960, StatusTooClose},
		{"a grid filling two thirds is framed well", 650, StatusGood},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := measureAlignment(capture(), frameAt(tc.side), true)
			assert.Equal(t, tc.want, a.Status, "fill was %.2f", a.Fill)
		})
	}
}

// A cell too small to sample is "too far" whatever the fill says. The two normally agree, and come
// apart exactly when the sender packs more cells into the same area — which is the case where fill
// alone would call a hopeless frame well aimed.
func TestAlignmentTreatsTinyCellsAsTooFar(t *testing.T) {
	g := frameAt(650)
	g.ModuleSize = 2
	a := measureAlignment(capture(), g, false)
	assert.Equal(t, StatusTooFar, a.Status)
}

// Off-axis is reported even when the framing is right, because squaring up is usually the easiest
// correction available and it buys margin against everything else.
func TestAlignmentReportsOffAxis(t *testing.T) {
	g := frameAt(650)
	// Shorten the bottom edge: a trapezoid, which is what a camera looking up at a monitor sees.
	g.Corners[2].X += 150
	g.Corners[3].X -= 150
	require.Greater(t, g.Perspective(), maxPerspective, "the fixture must actually be off-square")

	a := measureAlignment(capture(), g, false)
	assert.Equal(t, StatusOffAxis, a.Status)
}

// Located but not decoded is its own verdict. It looks identical to total failure from outside and
// is one small movement from working, so it must not be reported as either "good" or "searching".
func TestAlignmentDistinguishesMarginalFromGood(t *testing.T) {
	marginal := measureAlignment(capture(), frameAt(650), false)
	assert.Equal(t, StatusMarginal, marginal.Status)
	assert.True(t, marginal.Locked)

	good := measureAlignment(capture(), frameAt(650), true)
	assert.Equal(t, StatusGood, good.Status)
}

// The corners are what the page draws its outline from, so they must be normalised to the frame
// rather than left in capture pixels: the preview element's size has nothing to do with the
// capture's resolution.
func TestAlignmentNormalisesCorners(t *testing.T) {
	a := measureAlignment(capture(), frameAt(500), true)
	assert.Equal(t, [2]float64{0.25, 0.25}, a.Corners[0])
	assert.Equal(t, [2]float64{0.75, 0.75}, a.Corners[3])
	for _, c := range a.Corners {
		assert.GreaterOrEqual(t, c[0], 0.0)
		assert.LessOrEqual(t, c[0], 1.0)
	}
}

// A colour payload needs a far larger cell than a binary one, and the advice must say so while
// there is still a chance to act on it.
//
// This is the case that produced a hundred consecutive frames with perfect fiducials and a failed
// payload CRC: the framing was inside every limit the aiming display knew about, so it reported
// "almost" and the operator had nothing to act on. The cell was simply too small to measure a
// colour in, and nothing said that.
func TestAlignmentDemandsLargerCellsForColour(t *testing.T) {
	g := frameAt(650)
	g.ModuleSize = 6 // comfortable for binary, far too small for eight colours

	binary := measureAlignment(capture(), g, false)
	assert.Equal(t, StatusMarginal, binary.Status, "a binary frame at six pixels a cell is framed fine")

	g.Header.BitDepth = 3
	colour := measureAlignment(capture(), g, false)
	assert.Equal(t, StatusTooFar, colour.Status, "the same framing is too far for a colour payload")
	assert.Equal(t, colourModulePixels, colour.RequiredModulePixels)
	assert.Contains(t, colour.Advice, "Colour needs")
}

// Closer is not monotonically better for a colour payload, and the advice has to say so.
//
// Past roughly thirteen pixels a cell the camera is resolving the panel's own pixel structure
// rather than the frame on it. Measured back to back: 11.2 px per cell decoded six of six, 13.6
// decoded none of six. A floor alone reported "good" at 13.6 while nothing decoded, which sent the
// operator closer — the exact wrong direction.
func TestAlignmentDemandsBackingOffWhenTooClose(t *testing.T) {
	g := frameAt(650)
	g.Header.BitDepth = 3

	g.ModuleSize = 11.2
	inBand := measureAlignment(capture(), g, true)
	assert.Equal(t, StatusGood, inBand.Status, "11.2 px a cell is inside the colour band")
	assert.Equal(t, maxColourModulePixels, inBand.MaxModulePixels)

	g.ModuleSize = 13.6
	tooClose := measureAlignment(capture(), g, false)
	assert.Equal(t, StatusTooClose, tooClose.Status, "13.6 is past the band, however good the framing")
	assert.Contains(t, tooClose.Advice, "Closer is not better")
}

// A binary payload has no upper bound worth reporting: it thresholds rather than measures, so the
// panel's subpixels do not confuse it and more pixels are simply more.
func TestAlignmentHasNoUpperBoundForBinary(t *testing.T) {
	g := frameAt(650)
	g.Header.BitDepth = 1
	g.ModuleSize = 20

	a := measureAlignment(capture(), g, true)
	assert.Equal(t, StatusGood, a.Status)
	assert.Zero(t, a.MaxModulePixels)
}
