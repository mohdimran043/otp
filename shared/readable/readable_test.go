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

// TestBinaryReachesFurther is the trade an operator wanting a dense grid has to make, and its limits.
//
// Binary needs 6 pixels a cell against colour8's 10, so it reaches further on the same camera — but not as
// much further as it looked when the floor was 4. On a 1080 capture that is 172 cells against 104, and 192 is
// beyond both, which the observed run confirmed: 192 in binary located every frame and read no payloads.
func TestBinaryReachesFurther(t *testing.T) {
	colour := readable.Assess(160, 2, 3, 1080, 1920)
	binary := readable.Assess(160, 2, 1, 1080, 1920)

	require.False(t, colour.Readable, "160 cells needs 10 px/cell in colour and gets 6.6")
	require.True(t, binary.Readable, "the same grid clears the 6 px/cell binary floor")
	require.True(t, colour.BinaryWouldWork)
	require.Contains(t, colour.Explain(160, 3), "one bit a cell")

	// And the ceiling each encoding reaches on this camera, which is the number worth quoting.
	t.Logf("on a 1080 capture: colour tops out at %d cells, binary at %d",
		colour.MaxGrid, binary.MaxGrid)
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
//
// The answer moved when the binary floor was corrected from 4 to 6, and that is worth keeping visible. On the
// old figure a 12 MP capture read 512 cells in binary; on the corrected one it falls just short — 3024 across
// 516 cells is 5.86 pixels a cell against the 6 now required. So 512 needs a sensor with roughly 3,100 pixels
// on its short side, which is above 12 MP in the 3:4 shape phones use.
func TestA512GridNeedsARealSensor(t *testing.T) {
	require.False(t, readable.Assess(512, 2, 1, 1080, 1920).Readable,
		"512 cells is far out of reach at 1080p in any encoding")

	twelveMP := readable.Assess(512, 2, 1, 3024, 4032)
	require.False(t, twelveMP.Readable, "12 MP falls just short of 512 cells in binary")
	t.Logf("512 binary on a 12MP capture: %.2f px/cell against %.0f needed", twelveMP.ModulePixels, twelveMP.Required)

	// A larger sensor does reach it, which is the useful half of the answer.
	require.True(t, readable.Assess(512, 2, 1, 3472, 4624).Readable,
		"512 cells in binary needs roughly 3,100 pixels on the short side")

	// And never in colour, on any sensor a phone has: 516 cells at ten pixels is 5,160 across.
	require.False(t, readable.Assess(512, 2, 3, 3024, 4032).Readable)
	colour := readable.Assess(512, 2, 3, 3024, 4032)
	t.Logf("512 colour on a 12MP capture: %.1f px/cell, needs %.0f — max colour grid there %d",
		colour.ModulePixels, colour.Required, colour.MaxGrid)
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

// TestTheBinaryFloorMatchesWhatWasObserved pins the one binary observation there is, so the figure cannot
// drift back to a value that contradicts it.
//
// A 192-cell binary frame at 5.5 captured pixels a cell located its geometry on every frame and failed its
// payload on 41 of 41, with recovery offered all of them and rescuing none. Whatever the true floor is, it
// is above that.
func TestTheBinaryFloorMatchesWhatWasObserved(t *testing.T) {
	observed := readable.Assess(192, 2, 1, 1080, 1920)
	require.InDelta(t, 5.5, observed.ModulePixels, 0.1)
	require.False(t, observed.Readable,
		"192 cells in binary on a 1080 capture read no payloads at all and must not be called readable")

	// And the floor itself has to sit above the failure, or the assessment above is accidental.
	require.Greater(t, readable.BinaryPixels, 5.7)
}
