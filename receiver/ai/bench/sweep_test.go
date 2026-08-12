package bench_test

import (
	"fmt"
	"image"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/receiver/ai/soft"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// What recovery buys, per grid and per simulated optical path.
//
// The table is the deliverable, not the assertion. The only thing asserted is monotonicity —
// recovery runs after a failure and returns a frame only when the footer confirms it, so lowering
// the decode count would be a bug rather than a tuning question. A threshold on the rate itself
// would be either too loose to prove anything or too tight to survive a different machine.
//
// Read the zeroes. Grids whose cells are one or two pixels decode nothing either way, and that is
// the px-per-cell ceiling made visible rather than folklore.

// gridPresets is the sender's dropdown. See sender/web/src/pages/NewTransfer.tsx.
var gridPresets = []int{80, 96, 128, 192, 256, 384, 512, 1024}

// cellPresets is the sender's other dropdown, and usableEdge stands for the short side of a 1080p
// panel — what the browser's own chooser measures at run time.
var cellPresets = []int{1, 2, 3, 4, 6, 8}

const usableEdge = 1080

// framesPerPoint is how many frames each grid-and-profile pair is measured over.
//
// Twenty rather than hundreds because the cost is dominated by grid 1024, where one frame is a
// 386 KB payload over a million cells. The figure this produces is a decode *rate* to the nearest
// five percent, which is the resolution the question needs: whether recovery moves the ceiling, not
// where the ceiling is to two decimal places.
const framesPerPoint = 20

// cellFor is the largest offered cell size whose frame fits the display, mirroring the sender's own
// fitGridAndCell: screen area is free once the grid is chosen, and a bigger cell is what a camera
// needs to resolve. Zero when nothing fits.
func cellFor(grid int) int {
	best := 0
	for _, c := range cellPresets {
		if (grid+2*protocol.DefaultQuietZone)*c <= usableEdge {
			best = c
		}
	}
	return best
}

type namedProfile struct {
	name    string
	profile simulate.Profile
}

func profiles() []namedProfile {
	return []namedProfile{
		{"pristine", simulate.Pristine},
		{"clean", simulate.Clean},
		{"typical", simulate.Typical},
		{"harsh", simulate.Harsh},
	}
}

// renderFrame builds one colour8 frame whose payload fills the geometry, varying by index so the
// frames are not identical.
func renderFrame(t *testing.T, l protocol.Layout, index int) *image.RGBA {
	t.Helper()
	capacity, err := encoding.Color8.EstimateCapacity(l, 3)
	require.NoError(t, err)

	payload := make([]byte, capacity.PayloadBytes)
	for i := range payload {
		payload[i] = byte((i + index*7) * 11)
	}
	f := protocol.NewFrame(protocol.Header{TransmissionID: uuid.New(), FrameNumber: uint32(index)}, payload)
	img, err := encoding.Color8.Encode(f, l, 3)
	require.NoError(t, err)
	return img
}

func TestSweepDecodeRate(t *testing.T) {
	if testing.Short() {
		t.Skip("the sweep renders and decodes hundreds of frames up to grid 1024; -short skips it")
	}

	fmt.Printf("\n%-6s %-6s %-9s %8s %5s %5s %6s %8s %9s %-18s\n",
		"grid", "px/cel", "profile", "bytes", "off", "on", "delta", "badcells", "ms/recov", "failed at")

	for _, grid := range gridPresets {
		cell := cellFor(grid)
		if cell == 0 {
			t.Logf("grid %d: no offered cell size fits a %d px edge", grid, usableEdge)
			continue
		}
		l, err := protocol.NewLayout(grid, grid, cell)
		if err != nil {
			t.Logf("grid %d at %d px: %v", grid, cell, err)
			continue
		}
		capacity, err := encoding.Color8.EstimateCapacity(l, 3)
		require.NoError(t, err)

		for _, p := range profiles() {
			off, on, recovered := 0, 0, 0
			var recoverTime time.Duration
			// Why a frame failed decides whether this layer could ever have helped it, so the
			// table says so rather than leaving a "+0" to be interpreted. A point that failed at
			// no_quad had no geometry to search; one that failed at payload_crc did, and a zero
			// delta there would mean the search itself came up short.
			buckets := map[classify.Bucket]int{}
			// Symbol errors on the frames that located and failed. This is the number that decides
			// whether a bounded search over the least confident cells is the right tool at all: it
			// can correct twelve, so a regime averaging hundreds is telling you the cells are not
			// marginal, they are wrong.
			badCells, badFrames := 0, 0

			for i := 0; i < framesPerPoint; i++ {
				degraded := p.profile.Apply(renderFrame(t, l, i))
				opts := protocol.LocateOptions{ExpectedLayout: &l}

				_, decodeErr := encoding.Decode(degraded, opts)
				buckets[classify.Of(decodeErr)]++
				if decodeErr == nil {
					off++
					on++
					continue
				}

				// Recovery needs a geometry. A frame that never located is counted as a failure
				// both ways, which is the honest accounting: this layer cannot help it.
				g, lerr := protocol.Locate(degraded, opts)
				if lerr != nil {
					continue
				}
				if n, ok := symbolErrors(t, l, i, degraded, g); ok {
					badCells += n
					badFrames++
				}

				res, rerr := soft.Recover(g, degraded, soft.DefaultOptions())
				if rerr != nil {
					continue
				}
				on++
				recovered++
				recoverTime += res.Elapsed
			}

			require.GreaterOrEqual(t, on, off,
				"grid %d %s: recovery lowered the decode count from %d to %d", grid, p.name, off, on)

			var msPer float64
			if recovered > 0 {
				msPer = float64(recoverTime.Microseconds()) / float64(recovered) / 1000
			}
			meanBad := "-"
			if badFrames > 0 {
				meanBad = fmt.Sprintf("%d", badCells/badFrames)
			}
			fmt.Printf("%-6d %-6d %-9s %8d %5d %5d %+6d %8s %9.1f %-18s\n",
				grid, cell, p.name, capacity.PayloadBytes, off, on, on-off, meanBad, msPer,
				dominantFailure(buckets))
		}
	}
	fmt.Println()
}

// symbolErrors counts how many payload cells the degraded capture read differently from the truth.
//
// The truth is taken by reading the *pristine* render of the same frame rather than by re-packing the
// payload bytes, because both readings then come through the identical plan and photometric path and
// the comparison is index-aligned by construction. Reports false when the pristine render cannot be
// read, which would mean the geometry itself is broken and the comparison meaningless.
func symbolErrors(t *testing.T, l protocol.Layout, index int, degraded image.Image, g *protocol.Geometry) (int, bool) {
	t.Helper()

	clean := renderFrame(t, l, index)
	truthGeom, err := protocol.Locate(clean, protocol.LocateOptions{ExpectedLayout: &l})
	if err != nil {
		return 0, false
	}
	truth, err := encoding.SoftRead(truthGeom, clean)
	if err != nil {
		return 0, false
	}
	got, err := encoding.SoftRead(g, degraded)
	if err != nil {
		return 0, false
	}
	if len(truth.Symbols) != len(got.Symbols) {
		return 0, false
	}

	wrong := 0
	for i := range truth.Symbols {
		if truth.Symbols[i] != got.Symbols[i] {
			wrong++
		}
	}
	return wrong, true
}

// dominantFailure names the stage most frames failed at, so a zero delta can be attributed.
func dominantFailure(buckets map[classify.Bucket]int) string {
	best, bestCount := classify.Bucket(""), 0
	for b, n := range buckets {
		if b == classify.BucketDecoded || n <= bestCount {
			continue
		}
		best, bestCount = b, n
	}
	if bestCount == 0 {
		return "-"
	}
	return fmt.Sprintf("%s x%d", best, bestCount)
}
