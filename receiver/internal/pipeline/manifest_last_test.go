package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/internal/objectstore"
)

// TestChunksBeforeTheManifestStillMerges covers the ordering the receiver was never able to finish.
//
// Chunks routinely arrive before the manifest — the manifest is one frame in a repeating cycle, so a receiver
// that starts watching mid-stream reads data frames first, and delete.go's own comment says as much. The
// completion check was only ever reached from handleChunk, and only for a newly inserted non-parity chunk;
// handleManifest stored the manifest and returned. So a transmission whose last data chunk landed before its
// manifest sat with every chunk present, nothing missing, and no merged file, for ever.
//
// It could not recover, either. The sender keeps redisplaying, but a chunk already held is not inserted, and
// the check ran only on insertion — so every later frame was ignored too.
//
// Ingesting the data frame first and the manifest second is exactly that ordering, and the assertion is simply
// that the transfer finishes.
func TestChunksBeforeTheManifestStillMerges(t *testing.T) {
	h := newIngestHarness(t)
	tx := buildOneChunkTransmission(t, false, nil)
	ctx := context.Background()

	// The data frame first. Nothing can merge yet: the receiver has the bytes but does not know the filename,
	// the size, or the hash to check them against.
	dataResult, err := h.r.Ingest(ctx, tx.dataImage, nil)
	require.NoError(t, err)
	require.True(t, dataResult.Decoded)
	require.False(t, dataResult.IsManifest)

	time.Sleep(50 * time.Millisecond)
	_, err = h.st.Merged.Get(ctx, tx.transmissionID)
	require.Error(t, err, "nothing should merge before the manifest describes it")

	// Now the manifest, which completes the picture. Nothing else will arrive: this is the last frame either
	// side has to offer, so if the manifest does not trigger the merge, nothing ever will.
	manifestResult, err := h.r.Ingest(ctx, tx.manifestImage, nil)
	require.NoError(t, err)
	require.True(t, manifestResult.Decoded)
	require.True(t, manifestResult.IsManifest)

	require.Eventually(t, func() bool {
		merged, err := h.st.Merged.Get(ctx, tx.transmissionID)
		return err == nil && merged.Verified
	}, 5*time.Second, 10*time.Millisecond, "the manifest arriving last has to complete the transmission")

	merged, err := h.st.Merged.Get(ctx, tx.transmissionID)
	require.NoError(t, err)
	require.True(t, merged.Verified)

	body, err := objectstore.GetBytes(ctx, h.objects, merged.StoredPath, merged.SizeBytes+1)
	require.NoError(t, err)
	require.Equal(t, tx.payload, body)
}

// TestASecondManifestDoesNotMergeTwice guards the fix rather than the bug.
//
// The manifest is re-emitted every so many frames, so a receiver watching a finished transmission will see it
// again — and now that the manifest triggers the completion check, that check runs again on an already merged
// transmission. It must be a no-op rather than a second merge, a second delivery, or a second report to the
// sender.
func TestASecondManifestDoesNotMergeTwice(t *testing.T) {
	h := newIngestHarness(t)
	tx := buildOneChunkTransmission(t, false, nil)
	ctx := context.Background()

	_, err := h.r.Ingest(ctx, tx.dataImage, nil)
	require.NoError(t, err)
	_, err = h.r.Ingest(ctx, tx.manifestImage, nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		merged, err := h.st.Merged.Get(ctx, tx.transmissionID)
		return err == nil && merged.Verified
	}, 5*time.Second, 10*time.Millisecond)

	first, err := h.st.Merged.Get(ctx, tx.transmissionID)
	require.NoError(t, err)

	// The same manifest again, as a real display would show it.
	_, err = h.r.Ingest(ctx, tx.manifestImage, nil)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	again, err := h.st.Merged.Get(ctx, tx.transmissionID)
	require.NoError(t, err)
	require.Equal(t, first.VerifiedAt, again.VerifiedAt, "a repeated manifest must not merge the file again")
}
