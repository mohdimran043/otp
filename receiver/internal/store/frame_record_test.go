package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/internal/store"
	"github.com/opticaltransport/otp/receiver/internal/testdb"
)

// Two defects, found together while debugging a camera that would not decode.
//
// captured_frames has a UNIQUE (session_id, sequence) constraint and Record inserted with ON CONFLICT DO
// NOTHING, returning nil either way. Meanwhile a capture source numbers frames from zero, and pressing Stop then
// Start on the camera page builds a *new* source — so numbering restarts inside the same session. Every frame
// after the first restart collided, was silently discarded, and Record reported success. The session counters
// kept climbing (478 captured) while the rows stayed frozen (138), so Decode failures went permanently blind and
// every image pulled for diagnosis was from before the restart.
//
// And Failed ordered by sequence, which for exactly the same reason is not monotonic within a session: after a
// restart the newest frames have the *lowest* numbers. Ordering by time is what "most recent" means.

func mustRecord(t *testing.T, st *store.Store, f store.CapturedFrame) {
	t.Helper()
	require.NoError(t, st.Frames.Record(context.Background(), f))
}

// TestRecordReportsAFrameItDidNotStore is the important one: a store method must not report success for a write
// it threw away. Silent loss is what made the original bug invisible for an hour.
func TestRecordReportsAFrameItDidNotStore(t *testing.T) {
	st := store.New(testdb.New(t))
	ctx := context.Background()

	session, err := st.Sessions.Create(ctx, "browser")
	require.NoError(t, err)

	first := store.CapturedFrame{
		SessionID:  session.ID,
		Sequence:   7,
		StoredPath: "captures/x/" + uuid.NewString() + ".png",
		SHA256:     make([]byte, 32),
		Decoded:    false,
	}
	require.NoError(t, st.Frames.Record(ctx, first))

	// The same session and sequence again, as a restarted source produces.
	second := first
	second.ID = uuid.Nil
	second.StoredPath = "captures/x/" + uuid.NewString() + ".png"

	err = st.Frames.Record(ctx, second)
	require.Error(t, err, "a discarded write must not look like a successful one")
	require.True(t, errors.Is(err, store.ErrFrameNotRecorded),
		"and it must be distinguishable, so a caller can treat a duplicate differently from a failure")
}

// TestFailedReturnsTheMostRecentByTime — the case sequence ordering gets wrong. A restarted source produces low
// sequence numbers with recent timestamps, so ordering by sequence returns frames from before the restart and
// the page an operator is watching never updates.
func TestFailedReturnsTheMostRecentByTime(t *testing.T) {
	st := store.New(testdb.New(t))
	ctx := context.Background()

	session, err := st.Sessions.Create(ctx, "browser")
	require.NoError(t, err)

	// Before the restart: high sequence numbers, older timestamps.
	for _, seq := range []int64{500, 501, 502} {
		mustRecord(t, st, store.CapturedFrame{
			SessionID: session.ID, Sequence: seq, SHA256: make([]byte, 32),
			StoredPath: "captures/x/" + uuid.NewString() + ".png",
			CapturedAt: time.Now().Add(-10 * time.Minute),
		})
	}
	// After the restart: sequence begins again, timestamps are newer.
	for _, seq := range []int64{1, 2} {
		mustRecord(t, st, store.CapturedFrame{
			SessionID: session.ID, Sequence: seq, SHA256: make([]byte, 32),
			StoredPath: "captures/x/" + uuid.NewString() + ".png",
			CapturedAt: time.Now(),
		})
	}

	got, err := st.Frames.Failed(ctx, session.ID, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.ElementsMatch(t, []int64{1, 2}, []int64{got[0].Sequence, got[1].Sequence},
		"the newest frames are the ones captured last, not the ones numbered highest")
}
