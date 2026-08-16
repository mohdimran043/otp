package pipeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

// Outlining every lane, not just the one the decoder happened to lead with.
//
// The overlay draws what these report, and it drew a single box on a tiled display — so three
// quarters of a four-lane screen sat unmarked while the receiver was reading all of it. From the
// operator's side an unmarked lane and a lane that is not being seen look identical, which makes the
// overlay worse than nothing on exactly the display it was added for.

// laneAt builds a geometry whose fiducials sit on a square of the given side, centred where asked in
// a 1000x1000 capture. Position matters here in a way it does not for the aiming thresholds: these
// tests are about each lane being outlined where it actually is.
func laneAt(cx, cy, side float64, frameNumber uint32) *protocol.Geometry {
	half := side / 2
	return &protocol.Geometry{
		Corners: [4]protocol.Point{
			{X: cx - half, Y: cy - half},
			{X: cx + half, Y: cy - half},
			{X: cx - half, Y: cy + half},
			{X: cx + half, Y: cy + half},
		},
		ModuleSize:  8,
		FinderScore: 1,
		TimingScore: 1,
		Header:      protocol.Header{FrameNumber: frameNumber},
	}
}

// Two lanes side by side produce two outlines, each over its own half of the picture.
func TestEveryLaneGetsItsOwnOutline(t *testing.T) {
	left := laneAt(250, 500, 400, 11)
	right := laneAt(750, 500, 400, 12)

	a := measureReadings(capture(), []laneReading{
		{geometry: left, decoded: true},
		{geometry: right, decoded: true},
	}, 2)

	require.Len(t, a.Lanes, 2, "both lanes should be outlined")
	assert.Equal(t, uint32(11), a.Lanes[0].FrameNumber)
	assert.Equal(t, uint32(12), a.Lanes[1].FrameNumber)

	// Normalised into the picture, and on opposite sides of it. The failure this rules out is both
	// outlines being the lead lane's corners, which would draw two boxes on top of each other and
	// look convincing.
	assert.InDelta(t, 0.05, a.Lanes[0].Corners[0][0], 0.001, "left lane starts near the left edge")
	assert.InDelta(t, 0.55, a.Lanes[1].Corners[0][0], 0.001, "right lane starts past the middle")
	assert.NotEqual(t, a.Lanes[0].Corners, a.Lanes[1].Corners,
		"two lanes must not be outlined at the same place")
}

// Four lanes produce four outlines. The count is whatever was found, not a number baked in anywhere.
func TestFourLanesGetFourOutlines(t *testing.T) {
	readings := []laneReading{
		{geometry: laneAt(250, 250, 300, 1), decoded: true},
		{geometry: laneAt(750, 250, 300, 2), decoded: true},
		{geometry: laneAt(250, 750, 300, 3), decoded: true},
		{geometry: laneAt(750, 750, 300, 4), decoded: true},
	}

	a := measureReadings(capture(), readings, 4)

	require.Len(t, a.Lanes, 4)
	seen := map[[4][2]float64]bool{}
	for _, lane := range a.Lanes {
		assert.False(t, seen[lane.Corners], "each lane must be outlined in its own place")
		seen[lane.Corners] = true
	}
}

// A lane that is found but not reading is outlined as itself.
//
// This is the state the per-lane colouring exists for: glare on one corner of the panel costs that
// lane and nothing else, and a single verdict across the whole capture cannot say which one went.
func TestALaneThatDoesNotReadIsStillOutlinedAndMarkedUndecoded(t *testing.T) {
	a := measureReadings(capture(), []laneReading{
		{geometry: laneAt(250, 500, 400, 11), decoded: true},
		{geometry: laneAt(750, 500, 400, 12), decoded: false},
	}, 2)

	require.Len(t, a.Lanes, 2, "a lane that failed to decode is still in the picture")
	assert.True(t, a.Lanes[0].Decoded)
	assert.False(t, a.Lanes[1].Decoded)

	// The headline stays honest: frames are arriving, but not all of them, and the advice says which.
	assert.True(t, a.Decoded, "one lane reading means frames are arriving")
	assert.Equal(t, StatusMarginal, a.Status,
		"every lane in shot with only some reading is not the same as holding well: %s", a.Advice)
	assert.Contains(t, a.Advice, "1 of 2 frames in view are reading")
}

// All lanes reading is still plain good, with no extra hedging.
func TestEveryLaneReadingIsGood(t *testing.T) {
	a := measureReadings(capture(), []laneReading{
		{geometry: laneAt(250, 500, 400, 11), decoded: true},
		{geometry: laneAt(750, 500, 400, 12), decoded: true},
	}, 2)

	assert.Equal(t, StatusGood, a.Status, "two sent, two read: %s", a.Advice)
}

// A single-frame display still reports exactly one outline, so the overlay's ordinary case is
// unchanged rather than being a tiling of one that happens to look right.
func TestASingleFrameReportsOneOutline(t *testing.T) {
	a := measureAlignment(capture(), frameAt(650), true)

	require.Len(t, a.Lanes, 1)
	assert.True(t, a.Lanes[0].Decoded)
	assert.Equal(t, a.Corners, a.Lanes[0].Corners,
		"the lead corners and the only lane's corners describe the same frame")
}

// Nothing found means nothing to outline, rather than an empty box drawn at the origin.
func TestSearchingHasNoOutlines(t *testing.T) {
	a := measureAlignment(capture(), nil, false)

	assert.Equal(t, StatusSearching, a.Status)
	assert.Empty(t, a.Lanes)
}
