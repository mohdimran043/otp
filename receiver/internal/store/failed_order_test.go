package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/internal/store"
	"github.com/opticaltransport/otp/receiver/internal/testdb"
)

// TestFailedFramesReturnsTheMostRecentFirst.
//
// The Decode failures page exists to answer "why is my camera not reading this", which is a question about the
// last few seconds. It was ordered by sequence ascending and capped by a limit, so once a session had produced
// more failures than the limit the page was frozen on the first ones ever recorded — in one real session, 3,861
// failures deep, it showed frames 1 to 24 from twenty minutes earlier while the operator moved the camera
// around and watched nothing change.
//
// Newest first is the only ordering that answers the question the page is for.
func TestFailedFramesReturnsTheMostRecentFirst(t *testing.T) {
	st := store.New(testdb.New(t))
	ctx := context.Background()

	session, err := st.Sessions.Create(ctx, "browser")
	require.NoError(t, err)

	// Twelve failures, oldest to newest.
	for seq := 1; seq <= 12; seq++ {
		err := st.Frames.Record(ctx, store.CapturedFrame{
			SessionID:   session.ID,
			Sequence:    int64(seq),
			StoredPath:  "captures/x/" + uuid.NewString() + ".png",
			SHA256:      make([]byte, 32),
			Decoded:     false,
			DecodeError: "protocol: could not locate four finder patterns",
		})
		require.NoError(t, err)
	}

	got, err := st.Frames.Failed(ctx, session.ID, 3)
	require.NoError(t, err)
	require.Len(t, got, 3)

	require.Equal(t, int64(12), got[0].Sequence, "the newest failure has to come first")
	require.Equal(t, int64(11), got[1].Sequence)
	require.Equal(t, int64(10), got[2].Sequence)
}
