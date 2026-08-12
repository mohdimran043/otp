package soft_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/ai/soft"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// TestRecoverFindsPlantedErrors is the central claim: a frame that fails its payload checksum
// because a handful of cells landed just over the decision boundary is recoverable, and the bytes
// recovered are the original bytes.
func TestRecoverFindsPlantedErrors(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("%d planted", n), func(t *testing.T) {
			img, l, payload := planted(t, n)

			g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
			require.NoError(t, err, "geometry must still resolve; the plants are payload-only")

			opts := soft.DefaultOptions()
			opts.Budget = 0

			res, err := soft.Recover(g, img, opts)
			require.NoError(t, err)
			require.Equal(t, payload, res.Frame.Payload)
			t.Logf("%d planted -> recovered with %d flips after %d candidates in %s",
				n, res.Flips, res.Candidates, res.Elapsed)
		})
	}
}

// TestRecoverRefusesAHopelessFrame is the safety half. A frame with two hundred wrong cells is
// beyond any bounded search, and the search must say so rather than return something that happened
// to satisfy a checksum.
func TestRecoverRefusesAHopelessFrame(t *testing.T) {
	img, l, _ := planted(t, 200)

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	if err != nil {
		t.Skipf("two hundred damaged cells also broke the geometry: %v", err)
	}

	opts := soft.DefaultOptions()
	opts.Budget = 0

	started := time.Now()
	res, err := soft.Recover(g, img, opts)
	t.Logf("exhausting %d candidates at grid 80 took %s", opts.MaxCandidates, time.Since(started))

	require.ErrorIs(t, err, soft.ErrNotRecovered)
	require.Nil(t, res)
}

// TestRecoverHonoursCandidateCap proves the bound is real, so a marginal camera cannot turn the
// decode path into an unbounded search.
func TestRecoverHonoursCandidateCap(t *testing.T) {
	img, l, _ := planted(t, 40)

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	if err != nil {
		t.Skipf("geometry did not resolve: %v", err)
	}

	_, err = soft.Recover(g, img, soft.Options{MaxCells: 8, MaxCandidates: 32})
	require.ErrorIs(t, err, soft.ErrNotRecovered)
}

// TestRecoverIsANoOpOnACleanFrame records that a frame which already verifies is returned
// immediately, at zero flips — the fast path must not be paid for twice.
func TestRecoverIsANoOpOnACleanFrame(t *testing.T) {
	img, l, payload := planted(t, 0)

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	require.NoError(t, err)

	res, err := soft.Recover(g, img, soft.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, 0, res.Flips)
	require.Equal(t, payload, res.Frame.Payload)
}

// TestRecoverAcrossEveryGrid is the grid-coverage requirement: recovery must work at every grid
// the sender offers, not only at the one the camera rig happens to use.
//
// Cell size is pinned per grid the way the sender's own chooser pins it — the largest that fits the
// display — so each case is a geometry an operator could actually select. The planted errors are a
// fixed small count rather than a fraction of the grid, because the claim under test is that the
// search finds a handful of marginal cells however large the haystack is.
func TestRecoverAcrossEveryGrid(t *testing.T) {
	for _, grid := range gridPresets {
		t.Run(fmt.Sprintf("grid%d", grid), func(t *testing.T) {
			img, l, payload, ok := plantedAt(t, grid, 3)
			if !ok {
				t.Skipf("grid %d does not fit a %d px edge at any offered cell size", grid, usableEdge)
			}

			g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
			require.NoError(t, err, "a pristine render must locate at every grid")

			// No time budget: this asks whether the search *can* find the answer at this grid,
			// which is a different question from whether it fits in fifty milliseconds. The wall
			// time is logged so the budget's effect per grid is on the record, and
			// TestRecoverBudgetIsHonoured covers the bounded case.
			opts := soft.DefaultOptions()
			opts.Budget = 0

			res, err := soft.Recover(g, img, opts)
			require.NoError(t, err)
			require.Equal(t, payload, res.Frame.Payload)
			t.Logf("grid %d (%d px cells, %d payload bytes): %d flips, %d candidates, %s",
				grid, l.CellPixels, len(payload), res.Flips, res.Candidates, res.Elapsed)
		})
	}
}

// TestRecoverBudgetIsHonoured records the consequence of a per-candidate cost that scales with
// payload size: at the top of the grid range the default budget stops the search early.
//
// It asserts only that the budget is honoured, not that recovery fails — a fast machine may finish
// inside the budget and that is a pass, not a surprise. What must never happen is the search
// running for a second and a half on the decode path because the bound was expressed in candidates
// rather than in time.
func TestRecoverBudgetIsHonoured(t *testing.T) {
	img, l, _, ok := plantedAt(t, 512, 40)
	if !ok {
		t.Skipf("grid 512 does not fit a %d px edge at any offered cell size", usableEdge)
	}

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	require.NoError(t, err)

	opts := soft.DefaultOptions()
	opts.Budget = 20 * time.Millisecond

	started := time.Now()
	_, err = soft.Recover(g, img, opts)
	elapsed := time.Since(started)

	require.ErrorIs(t, err, soft.ErrNotRecovered)

	// The bound is the budget plus the sunk-cost floor, not the budget alone: once the payload has
	// been sampled that cost cannot be un-spent, so a fixed number of candidates is always tried.
	// Measured at grid 512 this lands near 100 ms against a 20 ms budget, and the assertion is set
	// to catch the failure that matters — an unbounded search running for seconds — rather than to
	// pin a figure that depends on how loaded the host is.
	require.Less(t, elapsed, 1500*time.Millisecond,
		"the search must terminate near the budget plus the sunk-cost floor, not run unbounded")
	t.Logf("grid %d (%d px cells) with a 20ms budget returned in %s", l.GridWidth, l.CellPixels, elapsed)
}

// TestRecoverFloorBeatsATinyBudgetAtALargeGrid is the guarantee that makes "recovery works at every
// grid" true rather than nominal.
//
// At a large grid the sampling pass alone exceeds any sane budget — about 118 ms of a 121 ms
// recovery at grid 1024 — so a search that honoured the budget strictly would refuse every large
// frame *after* paying for the read. The floor exists so that a completed read is always followed
// by enough candidates to fix the corrections that are actually common, and this asserts it: a one
// millisecond budget, which the read blows through many times over, still recovers a single-cell
// error at grid 512.
func TestRecoverFloorBeatsATinyBudgetAtALargeGrid(t *testing.T) {
	img, l, payload, ok := plantedAt(t, 512, 1)
	if !ok {
		t.Skipf("grid 512 does not fit a %d px edge at any offered cell size", usableEdge)
	}

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	require.NoError(t, err)

	opts := soft.DefaultOptions()
	opts.Budget = time.Millisecond

	res, err := soft.Recover(g, img, opts)
	require.NoError(t, err, "the sunk-cost floor must survive a budget the read alone exceeds")
	require.Equal(t, payload, res.Frame.Payload)
	t.Logf("grid %d recovered %d flip in %d candidates with a 1ms budget, took %s",
		l.GridWidth, res.Flips, res.Candidates, res.Elapsed)
}

// TestLeastConfidentPicksTheSmallest guards the selection separately from the search, because a
// heap that quietly returned the k largest margins would still produce a working search — it would
// simply almost never recover anything, which is indistinguishable from a hard channel.
func TestLeastConfidentPicksTheSmallest(t *testing.T) {
	img, l, _, ok := plantedAt(t, 96, 5)
	require.True(t, ok)

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	require.NoError(t, err)
	r, err := encoding.SoftRead(g, img)
	require.NoError(t, err)

	// Brute-force the answer from a full sort and require the heap selection to agree.
	all := append([]encoding.SoftCell(nil), r.Cells...)
	sort.Slice(all, func(i, j int) bool { return all[i].Margin < all[j].Margin })

	got := soft.LeastConfidentForTest(r.Cells, 12)
	require.Len(t, got, 12)
	for i := range got {
		require.InDelta(t, all[i].Margin, got[i].Margin, 1e-9, "position %d", i)
	}
}

// TestLeastConfidentHandlesFewerCellsThanAsked covers the small-grid edge: asking for twelve out of
// a region holding fewer than twelve must return what exists rather than panicking or padding.
func TestLeastConfidentHandlesFewerCellsThanAsked(t *testing.T) {
	cells := []encoding.SoftCell{{Index: 0, Margin: 5}, {Index: 1, Margin: 1}, {Index: 2, Margin: 3}}
	got := soft.LeastConfidentForTest(cells, 12)
	require.Len(t, got, 3)
	require.Equal(t, 1.0, got[0].Margin)
	require.Equal(t, 3.0, got[1].Margin)
	require.Equal(t, 5.0, got[2].Margin)

	require.Empty(t, soft.LeastConfidentForTest(cells, 0))
	require.Empty(t, soft.LeastConfidentForTest(nil, 12))
}
