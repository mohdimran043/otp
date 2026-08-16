package protocol_test

import (
	"image"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

func laneOf(t *testing.T, grid, cell int) protocol.Layout {
	t.Helper()
	l, err := protocol.NewLayout(grid, grid, cell)
	require.NoError(t, err)
	return l
}

// Tiling costs capacity, and the test says by how much.
//
// Two costs, and the obvious intuition gets each of them wrong.
//
// The first is pixels per cell: tiling does not buy any. Area is area, so a tiling and a single grid
// spanning the same display resolve the same cell, less the quiet zones and gaps each lane adds.
//
// The second is larger and much less obvious. Every lane carries its own header and footer bands, and
// those bands hold repeated copies of fixed-size records so the header survives captures whose
// payload does not. That is a fixed cost per lane, so four lanes pay it four times: four 96-cell
// lanes span roughly the same display as one 192-cell grid and carry 20,304 payload cells against its
// 32,490. At smaller lanes it is far worse — at 64 cells the bands consume all but 62 cells.
//
// None of which makes tiling a mistake; it makes it a trade, paid in capacity and bought in
// independent failure. Asserted here so nobody re-derives it on a camera.
func TestTilingCostsCapacityForIndependence(t *testing.T) {
	const capture, fill = 1080, 0.85

	tiled, err := protocol.NewLaneLayout(laneOf(t, 96, 4), 4, 2)
	require.NoError(t, err)
	single, err := protocol.NewLaneLayout(laneOf(t, 192, 4), 1, 0)
	require.NoError(t, err)

	ratio := tiled.ModulePixelsAt(capture, fill) / single.ModulePixelsAt(capture, fill)
	assert.InDelta(t, 1.0, ratio, 0.1,
		"the two span roughly the same display, so cells resolve alike: got %.3f", ratio)

	assert.Less(t, tiled.CellsPerFrame(), single.CellsPerFrame(),
		"tiling must cost capacity: every lane pays for its own header and footer bands")
	assert.Greater(t, float64(tiled.CellsPerFrame())/float64(single.CellsPerFrame()), 0.5,
		"but at 96-cell lanes it should keep well over half: got %d of %d",
		tiled.CellsPerFrame(), single.CellsPerFrame())
}

// The band overhead is why small lanes are useless, as a number so it is not rediscovered.
func TestSmallLanesAreEatenByTheirOwnBands(t *testing.T) {
	tiny, err := protocol.NewLaneLayout(laneOf(t, 64, 4), 4, 2)
	require.NoError(t, err)
	workable, err := protocol.NewLaneLayout(laneOf(t, 96, 4), 4, 2)
	require.NoError(t, err)

	assert.Less(t, tiny.CellsPerFrame(), 500,
		"four 64-cell lanes carry almost nothing: the bands take the rest")
	assert.Greater(t, workable.CellsPerFrame(), 20000,
		"96 is where lanes start carrying a useful payload")
}

// What tiling actually buys: one spoiled lane costs its own cells and no others.
//
// Stated as a property of the geometry, since that is where it comes from. The lanes are disjoint, so
// a blemish falling inside one covers none of the others — where on a single grid of the same
// capacity every cell shares one frame and one checksum, and so shares its fate.
func TestOneSpoiledLaneCostsOnlyItsOwnCells(t *testing.T) {
	tiled, err := protocol.NewLaneLayout(laneOf(t, 64, 4), 4, 2)
	require.NoError(t, err)

	spoiled, err := tiled.LaneBounds(0)
	require.NoError(t, err)

	survived := 0
	for i := 1; i < tiled.Lanes(); i++ {
		b, err := tiled.LaneBounds(i)
		require.NoError(t, err)
		assert.True(t, b.Intersect(spoiled).Empty(),
			"lane %d shares area with the spoiled lane 0, so it cannot survive independently", i)
		survived++
	}
	assert.Equal(t, 3, survived, "three of four lanes should be untouched by a blemish on the fourth")
	assert.Equal(t, tiled.CellsPerFrame()*3/4, survived*tiled.Lane.PayloadCellCount(),
		"and they should carry three quarters of the frame's cells between them")
}

// Lanes must not overlap, or one lane's fiducials would sit inside another's search area.
func TestLanesDoNotOverlap(t *testing.T) {
	l, err := protocol.NewLaneLayout(laneOf(t, 64, 4), 4, 2)
	require.NoError(t, err)

	var boxes []image.Rectangle
	for i := 0; i < l.Lanes(); i++ {
		b, err := l.LaneBounds(i)
		require.NoError(t, err)
		boxes = append(boxes, b)
	}
	for i := range boxes {
		for j := i + 1; j < len(boxes); j++ {
			assert.True(t, boxes[i].Intersect(boxes[j]).Empty(),
				"lane %d and lane %d overlap: %v and %v", i, j, boxes[i], boxes[j])
		}
	}
}

// Lanes are numbered in reading order, because that is what someone looking at the screen assumes.
func TestLanesAreNumberedLeftToRightThenDown(t *testing.T) {
	l, err := protocol.NewLaneLayout(laneOf(t, 64, 4), 4, 2)
	require.NoError(t, err)
	require.Equal(t, 2, l.Columns)
	require.Equal(t, 2, l.Rows)

	tl, _ := l.LaneOrigin(0)
	tr, _ := l.LaneOrigin(1)
	bl, _ := l.LaneOrigin(2)
	br, _ := l.LaneOrigin(3)

	assert.Equal(t, tl.Y, tr.Y, "lanes 0 and 1 share a row")
	assert.Less(t, tl.X, tr.X, "lane 0 is left of lane 1")
	assert.Equal(t, bl.Y, br.Y, "lanes 2 and 3 share a row")
	assert.Less(t, tl.Y, bl.Y, "lane 0 is above lane 2")
}

// A point between lanes belongs to none, and must say so.
//
// It is how a receiver learns its geometry is wrong: a fiducial found in the gap is evidence the
// lane grid has been fitted badly, where attributing it to the nearest lane would bury that.
func TestTheGapBelongsToNoLane(t *testing.T) {
	lane := laneOf(t, 64, 4)
	l, err := protocol.NewLaneLayout(lane, 4, 2)
	require.NoError(t, err)

	b0, err := l.LaneBounds(0)
	require.NoError(t, err)

	// A point just past lane 0's right edge, inside the gap.
	inGap := image.Point{X: b0.Max.X + 1, Y: b0.Min.Y + 10}
	_, ok := l.LaneAt(inGap)
	assert.False(t, ok, "a point in the gap must belong to no lane")

	which, ok := l.LaneAt(image.Point{X: b0.Min.X + 5, Y: b0.Min.Y + 5})
	require.True(t, ok)
	assert.Equal(t, 0, which)
}

// Every lane must fit inside the image the layout claims to need.
func TestEveryLaneFitsTheStatedImage(t *testing.T) {
	l, err := protocol.NewLaneLayout(laneOf(t, 64, 4), 4, 2)
	require.NoError(t, err)

	size := l.ImageSize()
	whole := image.Rect(0, 0, size.X, size.Y)
	for i := 0; i < l.Lanes(); i++ {
		b, err := l.LaneBounds(i)
		require.NoError(t, err)
		assert.True(t, b.In(whole), "lane %d at %v falls outside the %v image", i, b, whole)
	}
}

// A single lane is the same code path as a tiled one, not a special case beside it.
func TestOneLaneIsAnOrdinaryTiling(t *testing.T) {
	lane := laneOf(t, 96, 8)
	l, err := protocol.NewLaneLayout(lane, 1, 0)
	require.NoError(t, err)

	assert.Equal(t, 1, l.Lanes())
	origin, err := l.LaneOrigin(0)
	require.NoError(t, err)
	assert.Equal(t, image.Point{}, origin, "the only lane starts at the origin")
	assert.Equal(t, lane.PayloadCellCount(), l.CellsPerFrame())
}

// Counts that cannot be tiled evenly are refused, rather than silently rounded into a ragged grid
// whose last row is half empty.
func TestUntileableLaneCountsAreRefused(t *testing.T) {
	for _, n := range []int{3, 5, 7, 11, 0, 17} {
		_, err := protocol.NewLaneLayout(laneOf(t, 64, 4), n, 2)
		assert.ErrorIs(t, err, protocol.ErrLaneCount, "%d lanes should be refused", n)
	}
	for _, n := range []int{1, 2, 4, 6, 8, 9, 12, 16} {
		_, err := protocol.NewLaneLayout(laneOf(t, 64, 4), n, 2)
		assert.NoError(t, err, "%d lanes should tile", n)
	}
}

// Four frames tiled into one picture, and all four read back out of it.
//
// This is the whole claim of the lane system in one test: the sender composes independent frames, the
// receiver finds every one of them in a single capture, and each decodes on its own. It uses the real
// renderer and the real fiducial search rather than fixtures, so a change that breaks tiling breaks
// this rather than being discovered on a camera.
func TestFourLanesAreComposedAndAllFourFound(t *testing.T) {
	lane, err := protocol.NewLayout(96, 96, 4)
	require.NoError(t, err)
	tiled, err := protocol.NewLaneLayout(lane, 4, 2)
	require.NoError(t, err)

	// Four distinguishable frames: same geometry, different frame numbers.
	var images []image.Image
	for i := 0; i < 4; i++ {
		h := sampleHeader()
		h.CellPixels = uint16(lane.CellPixels)
		h.GridWidth, h.GridHeight = uint16(lane.GridWidth), uint16(lane.GridHeight)
		h.FrameNumber = uint32(100 + i)
		images = append(images, renderBands(t, lane, h))
	}

	display, err := tiled.Compose(images)
	require.NoError(t, err)

	size := tiled.ImageSize()
	require.Equal(t, size.X, display.Bounds().Dx())
	require.Equal(t, size.Y, display.Bounds().Dy())

	// The receiver's own search, over the whole tiled picture.
	cands := protocol.FindFinderCandidates(protocol.Binarize(protocol.Grayscale(display)))
	quads := tiled.SelectLaneQuads(cands)
	assert.Len(t, quads, 4, "all four lanes should be found in one capture, got %d", len(quads))

	// Each quad must lie wholly inside the lane it was filed under, or the grouping has taken
	// fiducials from a neighbour — the failure that made the earlier greedy approach unusable.
	for lane, q := range quads {
		bounds, err := tiled.LaneBounds(lane)
		require.NoError(t, err)
		for _, c := range q {
			p := image.Point{X: int(c.Center.X), Y: int(c.Center.Y)}
			assert.True(t, p.In(bounds),
				"lane %d was given a fiducial at %v, outside its own bounds %v", lane, p, bounds)
		}
	}
}

// Three lanes of four still yields three frames.
//
// The point of tiling: a lane lost to glare, a reflection or a hand costs its own cells and nothing
// else. Composing only three images is exactly what the sender does at the end of a transmission when
// there are fewer chunks left than lanes.
func TestALaneMissingCostsOnlyItself(t *testing.T) {
	lane, err := protocol.NewLayout(96, 96, 4)
	require.NoError(t, err)
	tiled, err := protocol.NewLaneLayout(lane, 4, 2)
	require.NoError(t, err)

	var images []image.Image
	for i := 0; i < 3; i++ {
		h := sampleHeader()
		h.CellPixels = uint16(lane.CellPixels)
		h.GridWidth, h.GridHeight = uint16(lane.GridWidth), uint16(lane.GridHeight)
		h.FrameNumber = uint32(200 + i)
		images = append(images, renderBands(t, lane, h))
	}

	display, err := tiled.Compose(images)
	require.NoError(t, err, "fewer images than lanes is ordinary, not an error")

	cands := protocol.FindFinderCandidates(protocol.Binarize(protocol.Grayscale(display)))
	quads := tiled.SelectLaneQuads(cands)
	assert.Len(t, quads, 3, "three composed lanes should yield three frames, got %d", len(quads))
}

// The receiver's side of tiling: four frames read out of one capture, unrectified.
//
// This is the test that matters for a camera. It composes four real frames, hands the whole picture
// to LocateAll exactly as a receiver would, and requires all four back — each with its own header,
// decoded independently. No screen detection, no perspective removal, no knowledge of the tiling:
// the descriptor CRC does the work of deciding which fiducials belong together.
func TestReceiverReadsFourLanesFromOneCapture(t *testing.T) {
	lane, err := protocol.NewLayout(96, 96, 4)
	require.NoError(t, err)
	tiled, err := protocol.NewLaneLayout(lane, 4, 2)
	require.NoError(t, err)

	want := map[uint32]bool{}
	var images []image.Image
	for i := 0; i < 4; i++ {
		h := sampleHeader()
		h.CellPixels = uint16(lane.CellPixels)
		h.GridWidth, h.GridHeight = uint16(lane.GridWidth), uint16(lane.GridHeight)
		h.FrameNumber = uint32(500 + i)
		want[h.FrameNumber] = true
		images = append(images, renderBands(t, lane, h))
	}

	display, err := tiled.Compose(images)
	require.NoError(t, err)

	found := protocol.LocateAll(display, protocol.LocateOptions{CellPixelsHint: lane.CellPixels}, 4)
	require.Len(t, found, 4, "all four lanes should decode from one capture, got %d", len(found))

	got := map[uint32]bool{}
	for _, g := range found {
		got[g.Header.FrameNumber] = true
		assert.Equal(t, 1.0, g.FinderScore, "each lane should match its fiducials exactly")
	}
	assert.Equal(t, want, got, "each lane must be read as its own frame, not one frame four times")
}

// A lane lost costs only itself, end to end.
func TestReceiverStillReadsTheLanesThatSurvive(t *testing.T) {
	lane, err := protocol.NewLayout(96, 96, 4)
	require.NoError(t, err)
	tiled, err := protocol.NewLaneLayout(lane, 4, 2)
	require.NoError(t, err)

	var images []image.Image
	for i := 0; i < 2; i++ {
		h := sampleHeader()
		h.CellPixels = uint16(lane.CellPixels)
		h.GridWidth, h.GridHeight = uint16(lane.GridWidth), uint16(lane.GridHeight)
		h.FrameNumber = uint32(600 + i)
		images = append(images, renderBands(t, lane, h))
	}

	display, err := tiled.Compose(images)
	require.NoError(t, err)

	found := protocol.LocateAll(display, protocol.LocateOptions{CellPixelsHint: lane.CellPixels}, 4)
	assert.Len(t, found, 2, "two composed lanes should yield two frames, got %d", len(found))
}
