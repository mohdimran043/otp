package pipeline_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/pipeline"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// seedAged is seedTransfer's cousin: it writes the same shape of transfer (a file and one
// chunk, with the objects those rows name) but backdates created_at, because the retention
// job decides by age and there is no other way to get an old row into a fresh test database.
func (h *harness) seedAged(t *testing.T, status store.TransmissionStatus, age time.Duration) store.Transmission {
	t.Helper()
	ctx := context.Background()

	fileKey, err := objectstore.Key("files", fmt.Sprintf("retention-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, fileKey, []byte("aged file")))

	sum := sha256.Sum256([]byte("aged file"))
	file, err := h.store.Files.Create(ctx, store.File{
		Filename: "aged.bin", StoredPath: fileKey, SizeBytes: 9, SHA256: sum[:],
	})
	require.NoError(t, err)

	tx, err := h.store.Transmissions.Create(ctx, store.Transmission{FileID: file.ID})
	require.NoError(t, err)

	require.NoError(t, h.store.Transmissions.SetStatus(ctx, tx.ID, status, ""))
	_, err = h.store.Pool().Exec(ctx,
		`UPDATE transmissions SET created_at = $2, updated_at = $2 WHERE id = $1`,
		tx.ID, time.Now().Add(-age))
	require.NoError(t, err)

	tx, err = h.store.Transmissions.Get(ctx, tx.ID)
	require.NoError(t, err)
	return tx
}

// waitForJob polls a job until it leaves pending/running, or fails the test.
func waitForJob(t *testing.T, h *harness, id uuid.UUID) jobs.Job {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := h.jobs.Get(ctx, id)
		require.NoError(t, err)
		if job.Status.Terminal() {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the retention job did not finish in time")
	return jobs.Job{}
}

// TestRetentionSweepsNeverCompletedTransfers is the whole point of the job: a transfer that
// never reached "completed" within the configured window is gone — rows and objects both —
// while one that finished, or one that simply has not aged out yet, is left alone.
func TestRetentionSweepsNeverCompletedTransfers(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Retention.MaxAge = 24 * time.Hour
		c.Retention.Interval = time.Hour
	})
	ctx := context.Background()

	oldPending := h.seedAged(t, store.TxPending, 25*time.Hour)
	oldCompleted := h.seedAged(t, store.TxCompleted, 25*time.Hour)
	recentFailed := h.seedAged(t, store.TxFailed, time.Hour)
	oldFailed := h.seedAged(t, store.TxFailed, 25*time.Hour)
	oldTransmitting := h.seedAged(t, store.TxTransmitting, 48*time.Hour)

	before := time.Now()
	job, err := h.jobs.Enqueue(ctx, jobs.Spec{Type: pipeline.TypeRetention}, 1)
	require.NoError(t, err)

	finished := waitForJob(t, h, job.ID)
	require.Equal(t, jobs.StatusCompleted, finished.Status, "the sweep itself must succeed: %s", finished.Error)

	assertGone(t, h, oldPending)
	assertGone(t, h, oldFailed)
	assertGone(t, h, oldTransmitting)
	assertPresent(t, h, oldCompleted)
	assertPresent(t, h, recentFailed)

	// And it must have re-enqueued itself exactly once, or the sweep either stops or starts
	// running twice as often as configured.
	all, err := h.jobs.List(ctx, jobs.Filter{
		Types:  []string{pipeline.TypeRetention},
		Status: []jobs.Status{jobs.StatusPending},
	})
	require.NoError(t, err)
	require.Len(t, all, 1, "the retention job must re-enqueue itself exactly once")
	require.True(t, all[0].RunAfter.After(before.Add(h.cfg.Retention.Interval/2)),
		"the next sweep should be scheduled about an interval out, not immediately")
}

// TestRetentionReschedulesAfterAPerRowFailure covers the guarantee retention.go documents at
// length: a failure partway through a sweep must never cost the next sweep. Two transmissions
// are seeded against the *same* file, both old enough to be swept. Reaping the one the sweep
// processes first deletes their shared file row, which cascades away the second transmission's
// row too — so by the time the sweep reaches that second id, its own Get returns ErrNotFound
// and reap.Transfer surfaces that as an error the handler must log and skip rather than abort
// on. This is deterministic regardless of which of the two ids ListForRetention happens to
// return first: whichever runs second always finds its row already gone.
//
// The genuinely sweep-level failure — ListForRetention itself erroring, before any row is even
// looked at — is not exercised here or anywhere else in this package. Doing so would need
// either a database fault injected mid-query (timing-dependent, and the flakiest kind of test
// to maintain) or a production-only seam: jobs.Context's store field is private to the jobs
// package, so no test outside it can build a working *jobs.Context by hand, and Pipeline's
// store and object-store dependencies are concrete types rather than interfaces, by design —
// nothing else needs them to be swappable. Adding either seam only to serve this one test was
// judged not worth it. What guarantees the sweep-level case instead is Go's own defer
// semantics: the successor-scheduling defer in retention.go is registered before
// ListForRetention is ever called, so it runs on every return from that function, including a
// return taken because that call failed — which is what this test's sibling above already
// confirms happens for the ordinary, error-free path, and what a reader of retention.go's
// comment can verify holds for the erroring one without needing a flaky test to say so.
func TestRetentionReschedulesAfterAPerRowFailure(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Retention.MaxAge = 24 * time.Hour
		c.Retention.Interval = time.Hour
	})
	ctx := context.Background()

	fileKey, err := objectstore.Key("files", fmt.Sprintf("retention-shared-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, fileKey, []byte("shared file")))
	sum := sha256.Sum256([]byte("shared file"))
	file, err := h.store.Files.Create(ctx, store.File{
		Filename: "shared.bin", StoredPath: fileKey, SizeBytes: 11, SHA256: sum[:],
	})
	require.NoError(t, err)

	age := 25 * time.Hour
	var ids []uuid.UUID
	for i := 0; i < 2; i++ {
		tx, err := h.store.Transmissions.Create(ctx, store.Transmission{FileID: file.ID})
		require.NoError(t, err)
		require.NoError(t, h.store.Transmissions.SetStatus(ctx, tx.ID, store.TxPending, ""))
		_, err = h.store.Pool().Exec(ctx,
			`UPDATE transmissions SET created_at = $2, updated_at = $2 WHERE id = $1`,
			tx.ID, time.Now().Add(-age))
		require.NoError(t, err)
		ids = append(ids, tx.ID)
	}

	job, err := h.jobs.Enqueue(ctx, jobs.Spec{Type: pipeline.TypeRetention}, 1)
	require.NoError(t, err)

	finished := waitForJob(t, h, job.ID)
	require.Equal(t, jobs.StatusCompleted, finished.Status,
		"a row that is already gone by the time it is reaped must not fail the sweep: %s", finished.Error)

	for _, id := range ids {
		_, err := h.store.Transmissions.Get(ctx, id)
		require.ErrorIs(t, err, store.ErrNotFound, "transmission %s should be gone, directly or by cascade", id)
	}
	_, err = h.store.Files.Get(ctx, file.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	all, err := h.jobs.List(ctx, jobs.Filter{
		Types:  []string{pipeline.TypeRetention},
		Status: []jobs.Status{jobs.StatusPending},
	})
	require.NoError(t, err)
	require.Len(t, all, 1, "exactly one successor must be scheduled even after a row-level failure")
}

func assertGone(t *testing.T, h *harness, tx store.Transmission) {
	t.Helper()
	ctx := context.Background()
	_, err := h.store.Transmissions.Get(ctx, tx.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "transmission %s should have been reaped", tx.ID)
	_, err = h.store.Files.Get(ctx, tx.FileID)
	require.ErrorIs(t, err, store.ErrNotFound, "file for %s should have been reaped", tx.ID)
}

func assertPresent(t *testing.T, h *harness, tx store.Transmission) {
	t.Helper()
	ctx := context.Background()
	_, err := h.store.Transmissions.Get(ctx, tx.ID)
	require.NoError(t, err, "transmission %s should not have been touched", tx.ID)
}
