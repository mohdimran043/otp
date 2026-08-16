package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The production rig as specified, checked against what it can actually resolve.
//
// The specification is a 4K panel carrying four colour frames, photographed by a 5 MP global-shutter
// camera. It reads as comfortable and is not: a 16:9 panel inside a 4:3-ish sensor is limited by width, the
// framing leaves margin so a nudge does not lose a fiducial, and a two-by-two tiling halves what is left in
// each direction. The three factors compound to about a third, and a cell rendered at eight display pixels
// arrives at the decoder as five.
//
// These pin the arithmetic because it is the arithmetic a hardware order is placed on.

func rigCamera() RGBCamera {
	return RGBCamera{
		Enabled:      true,
		Model:        "Basler ace 2 a2A2448-120cc",
		SensorWidth:  2448,
		SensorHeight: 2048,
		PanelWidth:   3840,
		PanelHeight:  2160,
		// The whole frame, which no real installation achieves — used here so the numbers are the
		// specification's own best case rather than a pessimistic reading of it.
		PanelFill: 1,
	}
}

// A 5 MP sensor sees well under half a sensor pixel per display pixel on a 4K panel.
func TestAFiveMegapixelSensorDoesNotSeeFourKAtFullResolution(t *testing.T) {
	f, ok := rigCamera().Feasibility(80, 2, 3, 4)
	require.True(t, ok)

	// 2448/3840. The height is not the limit: 2160 scaled by this is 1377, well inside 2048.
	assert.InDelta(t, 0.6375, f.CameraPixelsPerDisplayPixel, 0.001,
		"the panel is fitted by width, so this is 2448/3840 and not the sensor's megapixels")
	assert.Equal(t, 2448, f.PanelWidthInCamera)
	assert.Equal(t, 1377, f.PanelHeightInCamera,
		"a 16:9 panel leaves a third of a 1.2:1 sensor looking at the room")
}

// Four colour lanes on this rig fit only at the smallest grid the sender offers.
func TestFourColourLanesFitOnlyTheSmallestGrid(t *testing.T) {
	camera := rigCamera()

	at64, ok := camera.Feasibility(64, 2, 3, 4)
	require.True(t, ok)
	assert.True(t, at64.Readable, "grid 64 should carry: %s", at64.Explanation)
	assert.False(t, at64.Marginal, "and should not be marginal: %s", at64.Explanation)

	at80, ok := camera.Feasibility(80, 2, 3, 4)
	require.True(t, ok)
	assert.True(t, at80.Marginal,
		"grid 80 is past what four colour lanes can carry here: %s", at80.Explanation)

	at96, ok := camera.Feasibility(96, 2, 3, 4)
	require.True(t, ok)
	assert.False(t, at96.Readable,
		"grid 96 cannot be read at four colour lanes on this camera: %s", at96.Explanation)

	assert.Equal(t, 64, at64.MaxGrid,
		"which makes 64 both the working grid and the ceiling, with no margin above it")
}

// Halving the lanes is what buys the headroom, not a bigger grid.
//
// Worth stating because the instinct on being told four lanes cannot carry grid 96 is to raise something.
// Tiling never bought pixels per cell — area is area — so the lane count is the thing to spend.
func TestHalvingTheLanesBuysTheHeadroom(t *testing.T) {
	camera := rigCamera()

	four, ok := camera.Feasibility(96, 2, 3, 4)
	require.True(t, ok)
	two, ok := camera.Feasibility(96, 2, 3, 2)
	require.True(t, ok)

	assert.False(t, four.Readable, "four lanes at grid 96: %s", four.Explanation)
	assert.True(t, two.Readable, "two lanes at the same grid: %s", two.Explanation)
	assert.Greater(t, two.ModulePixels, four.ModulePixels,
		"a lane gets a larger share of the sensor when there are fewer of them")
}

// A binary payload carries far more on the same hardware, because it needs fewer pixels a cell.
func TestBinaryCarriesMoreOnTheSameCamera(t *testing.T) {
	camera := rigCamera()

	colour, ok := camera.Feasibility(96, 2, 3, 4)
	require.True(t, ok)
	binary, ok := camera.Feasibility(96, 2, 1, 4)
	require.True(t, ok)

	assert.False(t, colour.Readable, "colour at grid 96, four lanes: %s", colour.Explanation)
	assert.True(t, binary.Readable, "binary at the same geometry: %s", binary.Explanation)
	assert.Greater(t, binary.MaxGrid, colour.MaxGrid,
		"a binary cell is thresholded rather than matched against eight shades, so it needs fewer pixels")
}

// Framing margin is not free, and the planner has to charge for it.
func TestFramingMarginIsCharged(t *testing.T) {
	full := rigCamera()
	full.PanelFill = 1

	realistic := rigCamera()
	realistic.PanelFill = 0.9

	a, ok := full.Feasibility(64, 2, 3, 4)
	require.True(t, ok)
	b, ok := realistic.Feasibility(64, 2, 3, 4)
	require.True(t, ok)

	assert.Greater(t, a.ModulePixels, b.ModulePixels,
		"a panel framed with margin occupies fewer sensor pixels, and the planner must say so")
	assert.False(t, b.Readable && !b.Marginal,
		"which is enough to take the specification's own best case out of comfortable: %s", b.Explanation)
}

// Nothing is reported until there is something to report on.
func TestAnUndescribedCameraAnswersNothing(t *testing.T) {
	_, ok := RGBCamera{}.Feasibility(80, 2, 3, 4)
	assert.False(t, ok, "\"not configured\" and \"resolves nothing\" are different facts")
}
