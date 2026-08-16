package scheduler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/store"
)

// The tail of a transmission, where there are fewer chunks left than lanes to show them in.
//
// Two things have to hold at once, and they pull in opposite directions. The display should be full,
// because a dark lane is a quarter of the screen carrying nothing while the camera photographs it
// anyway. The accounting should not see the repeats, because a chunk shown four times in one frame
// went out *once* as far as the receiver is concerned — counting it four times inflates its attempt
// count and, with a retry ceiling in force, fails the transfer for the crime of nearly being finished.

// frames builds n distinct frames, which is all these tests need of one.
func frames(n int) []store.Frame {
	out := make([]store.Frame, n)
	for i := range out {
		out[i] = store.Frame{ID: uuid.New(), FrameNumber: i}
	}
	return out
}

func ids(fs []store.Frame) []uuid.UUID {
	out := make([]uuid.UUID, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}

// distinct counts how many different frames a display carries, which is what the accounting counts.
func distinct(fs []store.Frame) int {
	seen := map[uuid.UUID]bool{}
	for _, f := range fs {
		seen[f.ID] = true
	}
	return len(seen)
}

// TestOneChunkLeftFillsEveryLaneWithIt — the case the feature exists for.
func TestOneChunkLeftFillsEveryLaneWithIt(t *testing.T) {
	chosen := frames(1)

	display := fillLanes(chosen, 4)

	require.Len(t, display, 4, "a lane left dark is screen area the camera photographs for nothing")
	for _, f := range display {
		require.Equal(t, chosen[0].ID, f.ID)
	}
	require.Equal(t, 1, distinct(display),
		"four lanes, one chunk: the receiver is being offered four reads of the same symbol")
}

// TestPartialRoundsRepeatInOrder: three chunks into four lanes puts the first one back on the end,
// rather than filling with whichever frame is handiest.
//
// The order matters because lane 0 is the round's leader — it names the frame in the display record
// and in the logs — and because repeating in order spreads the extra reads across the chunks that
// have them rather than piling them onto one.
func TestPartialRoundsRepeatInOrder(t *testing.T) {
	chosen := frames(3)

	display := fillLanes(chosen, 4)

	require.Equal(t,
		[]uuid.UUID{chosen[0].ID, chosen[1].ID, chosen[2].ID, chosen[0].ID},
		ids(display))
	require.Equal(t, 3, distinct(display), "the repeat must not look like a fourth chunk to the accounting")
}

// TestTwoChunksAcrossFourLanesRepeatBoth, rather than doubling only the first.
func TestTwoChunksAcrossFourLanesRepeatBoth(t *testing.T) {
	chosen := frames(2)

	display := fillLanes(chosen, 4)

	require.Equal(t,
		[]uuid.UUID{chosen[0].ID, chosen[1].ID, chosen[0].ID, chosen[1].ID},
		ids(display))
}

// TestAFullRoundIsUntouched: the ordinary case, which runs for every frame of every transfer and must
// not allocate or reorder anything.
func TestAFullRoundIsUntouched(t *testing.T) {
	chosen := frames(4)

	display := fillLanes(chosen, 4)

	require.Equal(t, ids(chosen), ids(display))
	require.Equal(t, &chosen[0], &display[0], "a full round should be passed through, not copied")
}

// TestFillingNeverWritesIntoTheCallersSlice is the regression test for the trap in this function.
//
// The scheduler reads `chosen` *after* handing the filled slice to the display: it walks it to record
// which chunks went out and when. If the fill appended into the caller's own backing array — which
// `append(chosen, ...)` does whenever the slice has spare capacity — the repeats would overwrite
// whatever followed, and the accounting would then charge those sends to the wrong chunks.
//
// The bug needs spare capacity to appear at all, so a slice built by `frames` above would not show it.
// This one is made with room to grow, which is what a slice that has been filtered down usually has.
func TestFillingNeverWritesIntoTheCallersSlice(t *testing.T) {
	backing := make([]store.Frame, 0, 8)
	backing = append(backing, frames(2)...)
	chosen := backing[:2]
	before := ids(chosen)

	display := fillLanes(chosen, 4)

	require.Equal(t, before, ids(chosen), "the caller's slice is read again after this and must be intact")
	require.Len(t, display, 4)
	require.Equal(t, 2, distinct(display))
}

// TestMoreChunksThanLanesIsLeftAlone. The chooser caps a round at the lane count, so this is a
// defensive case rather than a reachable one — but truncating here would silently drop a chunk that
// the accounting was about to mark as sent, which is the worst available failure.
func TestMoreChunksThanLanesIsLeftAlone(t *testing.T) {
	chosen := frames(5)

	display := fillLanes(chosen, 4)

	require.Equal(t, ids(chosen), ids(display))
}

// TestNoChunksStaysEmpty, rather than dividing by zero on the modulo.
func TestNoChunksStaysEmpty(t *testing.T) {
	require.Empty(t, fillLanes(nil, 4))
	require.Empty(t, fillLanes([]store.Frame{}, 4))
}

// TestASingleLaneDisplayNeverRepeats — with one lane there is no spare to fill, and a repeat would
// mean showing the same chunk twice in a row for no reason.
func TestASingleLaneDisplayNeverRepeats(t *testing.T) {
	chosen := frames(1)

	require.Len(t, fillLanes(chosen, 1), 1)
	require.Len(t, fillLanes(chosen, 0), 1, "a lane count of zero must not loop forever")
}
