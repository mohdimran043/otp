package readable_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/readable"
)

// The cases below are the measured ones, not invented ones. Each corresponds to a session that either
// worked or did not, so a change that breaks this table is a change that contradicts an observation.

// TestTheCaseThatCostAnAfternoon is the 128-grid portrait capture that reported "move closer" at 86% of the
// view filled — advice that could not be followed, because the ceiling was already below the requirement.
func TestTheCaseThatCostAnAfternoon(t *testing.T) {
	a := readable.Assess(128, 2, 3, 1080, 1920)

	require.False(t, a.Readable)
	require.InDelta(t, 8.18, a.ModulePixels, 0.05, "1080 across 132 cells")
	require.InDelta(t, 10.0, a.Required, 0.01)

	// And no rotation advice, because a square frame gains nothing from it: the short side bounds it either
	// way round. An earlier version claimed 14.5 pixels a cell from turning the camera, which is a number
	// that does not exist — the capture is 1920x1080 in both orientations and a square inscribed in it is
	// 1080 across.
	msg := a.Explain(128, 3)
	require.NotContains(t, msg, "sideways")
	require.Contains(t, msg, "moving closer cannot change")
}

// TestTheGeometryThatDecodedEverything is the landscape session that read 31 frames of 31 at the same grid.
// Same grid, same camera, held the other way.
func TestTheGeometryThatDecodedEverything(t *testing.T) {
	a := readable.Assess(128, 2, 3, 1920, 1080)
	require.False(t, a.Readable,
		"the short side still bounds a square frame, whichever way round the dimensions are given")

	// Which is the point of taking the short side rather than the width: a landscape *capture* does not
	// help unless the frame is turned with it. What decoded was a frame filling the 1080 height at a
	// geometry small enough for it.
	require.InDelta(t, readable.Assess(128, 2, 3, 1080, 1920).ModulePixels, a.ModulePixels, 0.001)
}

// TestTheGeometryThatWorked is 96 cells in portrait, which completed a 175-chunk transfer.
func TestTheGeometryThatWorked(t *testing.T) {
	a := readable.Assess(96, 2, 3, 1080, 1920)
	require.True(t, a.Readable)
	require.InDelta(t, 10.8, a.ModulePixels, 0.05)
	require.Contains(t, a.Explain(96, 3), "resolves to")
}

// TestColourCannotReachTheDenseGrids records the limit an operator most needs told, because the grid
// dropdown offers sizes no colour camera can read.
func TestColourCannotReachTheDenseGrids(t *testing.T) {
	for _, grid := range []int{192, 256, 384, 512, 1024} {
		a := readable.Assess(grid, 2, 3, 1080, 1920)
		require.False(t, a.Readable, "colour8 at %d cells on a 1080 capture", grid)
		t.Logf("grid %4d: %.1f px/cell (need %.0f), max colour grid here %d, binary would work: %v",
			grid, a.ModulePixels, a.Required, a.MaxGrid, a.BinaryWouldWork)
	}
}

// TestBinaryReachesFurther is the trade worth surfacing: the same capture reads a much denser grid in one
// bit a cell, because a thresholded cell needs less than half the pixels a measured one does.
func TestBinaryReachesFurther(t *testing.T) {
	colour := readable.Assess(192, 2, 3, 1080, 1920)
	binary := readable.Assess(192, 2, 1, 1080, 1920)

	require.False(t, colour.Readable)
	require.True(t, binary.Readable, "192 cells is reachable at one bit a cell")
	require.True(t, colour.BinaryWouldWork)
	require.Contains(t, colour.Explain(192, 3), "one bit a cell")
}

// TestMaxGridIsUsableAdvice checks the suggested ceiling actually passes its own test, which is the property
// that makes it safe to print. A suggestion that is itself unreadable is worse than none.
func TestMaxGridIsUsableAdvice(t *testing.T) {
	for _, capture := range [][2]int{{1080, 1920}, {1920, 1080}, {2160, 3840}, {720, 1280}} {
		for _, depth := range []uint8{1, 3} {
			a := readable.Assess(512, 2, depth, capture[0], capture[1])
			if a.MaxGrid < 48 {
				continue // below the protocol's minimum grid; nothing to suggest
			}
			again := readable.Assess(a.MaxGrid, 2, depth, capture[0], capture[1])
			require.True(t, again.Readable,
				fmt.Sprintf("suggested %d cells for %v depth %d must itself be readable",
					a.MaxGrid, capture, depth))
		}
	}
}

// TestA512GridNeedsARealSensor answers the question directly: what would it take to read the densest grid
// the sender offers?
func TestA512GridNeedsARealSensor(t *testing.T) {
	// 516 cells at four pixels each is 2064 pixels on the short side — a 12 MP phone in landscape reaches
	// it, and no 1080p capture does.
	require.False(t, readable.Assess(512, 2, 1, 1080, 1920).Readable)
	require.True(t, readable.Assess(512, 2, 1, 3024, 4032).Readable,
		"512 cells is readable in binary on a 12 MP capture")
	require.False(t, readable.Assess(512, 2, 3, 3024, 4032).Readable,
		"but not in colour, which needs 5160 pixels across")

	a := readable.Assess(512, 2, 3, 3024, 4032)
	t.Logf("512 colour on a 12MP capture: %.1f px/cell, needs %.0f — max colour grid %d",
		a.ModulePixels, a.Required, a.MaxGrid)
}

func TestDegenerateInputsDoNotPanic(t *testing.T) {
	require.False(t, readable.Assess(0, 0, 3, 0, 0).Readable)
	require.False(t, readable.Assess(-5, 2, 3, 1080, 1920).Readable)
	require.Zero(t, readable.Assess(128, 2, 3, 0, 1920).ModulePixels)
}

// TestTheMarginalBandIsNotCalledImpossible is the case that made the previous message untrustworthy: 128
// cells on a 1080 capture was reported as "cannot be read" and then acknowledged 24 chunks of 74.
//
// Measured on that session: 463 frames captured, 10 decoded on the first pass and 6 more recovered — 3.5%
// usable. That is not zero, and the wording has to reflect it.
func TestTheMarginalBandIsNotCalledImpossible(t *testing.T) {
	a := readable.Assess(128, 2, 3, 1080, 1920)

	require.False(t, a.Readable)
	require.True(t, a.Marginal, "8.2 px/cell against a requirement of 10 is marginal, not hopeless")
	require.False(t, a.Hopeless)

	msg := a.Explain(128, 3)
	require.Contains(t, msg, "marginal")
	require.NotContains(t, msg, "cannot be read",
		"a geometry that acknowledged 24 chunks must not be described as unreadable")
	require.Contains(t, msg, "few frames in a hundred")
}

// TestTheHopelessBandStillSaysSo keeps the strong wording where it is earned.
func TestTheHopelessBandStillSaysSo(t *testing.T) {
	a := readable.Assess(512, 2, 3, 1080, 1920)
	require.True(t, a.Hopeless, "2.1 px/cell is below any band that decodes")
	require.False(t, a.Marginal)
	require.Contains(t, a.Explain(512, 3), "cannot be read")
}
