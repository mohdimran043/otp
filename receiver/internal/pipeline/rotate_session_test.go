package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Starting the camera should begin a clean slate.
//
// A capture session lasted as long as the process, so every attempt piled onto the last: Live capture still
// showed frames from an hour earlier, the counters mixed one attempt's failures with another's, and an operator
// adjusting a camera could not tell what the change had done. It also made the diagnostics untrustworthy —
// "0 decoded of 478" spanned six unrelated attempts.
//
// A session should mean one capture run, so switching source starts a new one. Nothing is deleted: the old
// session keeps its rows and closes cleanly, and every view is already scoped by session, so a new one reads
// empty without anything being destroyed.

func TestRotateSessionStartsAFreshOne(t *testing.T) {
	h := newIngestHarness(t)
	ctx := context.Background()

	first := h.r.Session()
	require.NotEqual(t, first.String(), "00000000-0000-0000-0000-000000000000")

	second, err := h.r.RotateSession(ctx)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "rotating must produce a different session")
	require.Equal(t, second, h.r.Session(), "and the receiver must now record frames against it")
}

// The old session is closed rather than abandoned, so it does not sit "capturing" for ever in the sessions list.
func TestRotateSessionClosesTheOldOne(t *testing.T) {
	h := newIngestHarness(t)
	ctx := context.Background()

	first := h.r.Session()
	_, err := h.r.RotateSession(ctx)
	require.NoError(t, err)

	old, err := h.st.Sessions.Get(ctx, first)
	require.NoError(t, err)
	require.NotEqual(t, "capturing", old.Status, "the session we left should not still claim to be capturing")
	require.NotNil(t, old.EndedAt)
}

// The point of the whole thing: what an operator sees resets, without anything being deleted.
func TestRotateSessionLeavesTheNewSessionEmptyAndKeepsTheOldRows(t *testing.T) {
	h := newIngestHarness(t)
	ctx := context.Background()
	tx := buildOneChunkTransmission(t, false, nil)

	_, err := h.r.Ingest(ctx, tx.manifestImage, nil)
	require.NoError(t, err)
	_, err = h.r.Ingest(ctx, tx.dataImage, nil)
	require.NoError(t, err)

	first := h.r.Session()
	require.Eventually(t, func() bool {
		got, err := h.st.Frames.Recent(ctx, first, 10)
		return err == nil && len(got) > 0
	}, 5*time.Second, 20*time.Millisecond, "the first session should have recorded something")

	before, err := h.st.Frames.Recent(ctx, first, 10)
	require.NoError(t, err)

	second, err := h.r.RotateSession(ctx)
	require.NoError(t, err)

	fresh, err := h.st.Frames.Recent(ctx, second, 10)
	require.NoError(t, err)
	require.Empty(t, fresh, "the new session starts empty, which is what clears Live capture")

	kept, err := h.st.Frames.Recent(ctx, first, 10)
	require.NoError(t, err)
	require.Len(t, kept, len(before), "and the old session's frames are still there — nothing is destroyed")
}
