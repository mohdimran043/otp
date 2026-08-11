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

	// And it must have re-enqueued itself, or the sweep only ever runs once.
	all, err := h.jobs.List(ctx, jobs.Filter{
		Types:  []string{pipeline.TypeRetention},
		Status: []jobs.Status{jobs.StatusPending},
	})
	require.NoError(t, err)
	require.NotEmpty(t, all, "the retention job must re-enqueue itself")
	require.True(t, all[0].RunAfter.After(before.Add(h.cfg.Retention.Interval/2)),
		"the next sweep should be scheduled about an interval out, not immediately")
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
