package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// Deleting a transfer needs a real database for the same reason the frame archive tests do:
// the interesting behaviour is what happens to rows and objects together, which an in-memory
// fake would have to reimplement rather than exercise.

// deleteHarness is a database, an object store, a job store, and a server — delete touches all
// four, cancelling queued jobs for a pending transfer before removing its rows and objects.
type deleteHarness struct {
	store   *store.Store
	jobs    *jobs.Store
	objects objectstore.Store
	handler http.Handler
}

func newDeleteHarness(t *testing.T) *deleteHarness {
	t.Helper()
	pool := testdb.New(t)

	cfg := config.Default()
	cfg.Storage.Root = t.TempDir()
	objects, err := objectstore.Open(context.Background(), cfg.Storage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, objects.Close()) })

	st := store.New(pool)
	js := jobs.NewStore(pool)
	handler := New(Options{
		Store:   st,
		Jobs:    js,
		Objects: objects,
		Config:  config.NewWatcher("", cfg),
		Log:     zap.NewNop(),
	}).Routes()

	return &deleteHarness{store: st, jobs: js, objects: objects, handler: handler}
}

// seedTransfer writes a file, a transmission at the given status, chunk and frame rows, and
// every object those rows name — the layout a real transfer leaves behind, built through the
// same objectstore.Key calls the pipeline uses so the test exercises the real key scheme.
func (h *deleteHarness) seedTransfer(t *testing.T, status store.TransmissionStatus) store.Transmission {
	t.Helper()
	ctx := context.Background()

	fileID := uuid.New()
	fileKey, err := objectstore.Key("files", fileID.String())
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, fileKey, []byte("the original file")))

	file, err := h.store.Files.Create(ctx, store.File{
		ID: fileID, Filename: "report.pdf", StoredPath: fileKey,
		SizeBytes: 18, SHA256: sum("the original file"),
	})
	require.NoError(t, err)

	tx, err := h.store.Transmissions.Create(ctx, store.Transmission{FileID: file.ID})
	require.NoError(t, err)

	compressedKey, err := objectstore.Key("transmissions", tx.ID.String(), "compressed.bin")
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, compressedKey, []byte("compressed")))

	chunkKey, err := objectstore.Key("transmissions", tx.ID.String(), "chunks", fmt.Sprintf("%08d.bin", 0))
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, chunkKey, []byte("chunk-0")))
	require.NoError(t, h.store.Chunks.InsertMany(ctx, []store.Chunk{{
		TransmissionID: tx.ID, ESI: 0, SizeBytes: 7, SHA256: sum("chunk-0"), StoredPath: chunkKey,
	}}))

	frameKey, err := objectstore.Key("transmissions", tx.ID.String(), "frames", fmt.Sprintf("%08d.png", 0))
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, frameKey, []byte("frame-0")))
	require.NoError(t, h.store.Frames.InsertMany(ctx, []store.Frame{{
		TransmissionID: tx.ID, FrameNumber: 0, IsManifest: true,
		WidthPx: 4, HeightPx: 4, PayloadBytes: 7, StoredPath: frameKey, SHA256: sum("frame-0"),
	}}))

	require.NoError(t, h.store.Transmissions.SetStatus(ctx, tx.ID, status, ""))
	tx, err = h.store.Transmissions.Get(ctx, tx.ID)
	require.NoError(t, err)
	return tx
}

func sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func (h *deleteHarness) delete(t *testing.T, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/v1/transfers/"+id.String(), nil))
	return response
}

func TestDeleteTransferRemovesRowsAndObjects(t *testing.T) {
	h := newDeleteHarness(t)
	tx := h.seedTransfer(t, store.TxCompleted)
	ctx := context.Background()

	response := h.delete(t, tx.ID)
	require.Equal(t, http.StatusNoContent, response.Code)
	require.Empty(t, response.Body.String())

	_, err := h.store.Transmissions.Get(ctx, tx.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = h.store.Files.Get(ctx, tx.FileID)
	require.ErrorIs(t, err, store.ErrNotFound)

	objects, err := h.objects.List(ctx, fmt.Sprintf("transmissions/%s/", tx.ID))
	require.NoError(t, err)
	require.Empty(t, objects, "chunks, frames, and the compressed stream must all be gone")

	fileKey, err := objectstore.Key("files", tx.FileID.String())
	require.NoError(t, err)
	exists, err := h.objects.Exists(ctx, fileKey)
	require.NoError(t, err)
	require.False(t, exists)
}

// TestDeleteTransferWhileDisplayingIs409 covers the state where the pipeline and the display
// loop hold this transmission's identifier and are actively acting on it: deleting out from
// under them would either resurrect an object or fail a running stage on a vanished row.
func TestDeleteTransferWhileDisplayingIs409(t *testing.T) {
	h := newDeleteHarness(t)
	tx := h.seedTransfer(t, store.TxTransmitting)

	response := h.delete(t, tx.ID)
	require.Equal(t, http.StatusConflict, response.Code)

	_, err := h.store.Transmissions.Get(context.Background(), tx.ID)
	require.NoError(t, err, "a refused delete must leave the row in place")
}

// TestDeleteRefusesPreparingAndPaused covers the other two states in which a stage or the
// display loop is holding the row, alongside transmitting above.
func TestDeleteRefusesPreparingAndPaused(t *testing.T) {
	h := newDeleteHarness(t)
	for _, status := range []store.TransmissionStatus{store.TxPreparing, store.TxPaused} {
		tx := h.seedTransfer(t, status)
		response := h.delete(t, tx.ID)
		require.Equal(t, http.StatusConflict, response.Code, "status %s", status)
	}
}

func TestDeleteUnknownTransferIs404(t *testing.T) {
	h := newDeleteHarness(t)
	response := h.delete(t, uuid.New())
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestDeleteRefusesBadUUID(t *testing.T) {
	h := newDeleteHarness(t)
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/v1/transfers/not-a-uuid", nil))
	require.Equal(t, http.StatusBadRequest, response.Code)
}

// TestDeletePendingTransferWithQueuedJobsStillSucceeds exercises the pending-specific branch
// that asks queued jobs to stop before the row goes: a pending transfer has no stage running
// yet, only jobs waiting to be claimed, and those are cancelled first so a worker that claims
// one after the delete starts does not write a chunk or a frame back into a namespace the
// delete just emptied. The row's own cascade removes the job rows regardless of that call, so
// what this proves is that the cancel-then-delete path runs cleanly end to end rather than
// erroring on a job that is about to be swept away anyway.
func TestDeletePendingTransferWithQueuedJobsStillSucceeds(t *testing.T) {
	h := newDeleteHarness(t)
	tx := h.seedTransfer(t, store.TxPending)
	ctx := context.Background()

	id := tx.ID
	_, err := h.jobs.Enqueue(ctx, jobs.Spec{Type: "compress", TransmissionID: &id, FileID: &tx.FileID}, 3)
	require.NoError(t, err)

	response := h.delete(t, tx.ID)
	require.Equal(t, http.StatusNoContent, response.Code)

	_, err = h.store.Transmissions.Get(ctx, tx.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	list, err := h.jobs.List(ctx, jobs.Filter{TransmissionID: &id, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, list, "the transmission's cascade takes its jobs with it")
}

// TestDeleteReadyCompletedFailedCancelledAllSucceed covers every remaining status the spec
// allows deletion from, so the switch in the handler cannot silently narrow over time.
func TestDeleteReadyCompletedFailedCancelledAllSucceed(t *testing.T) {
	h := newDeleteHarness(t)
	for _, status := range []store.TransmissionStatus{
		store.TxReady, store.TxCompleted, store.TxFailed, store.TxCancelled,
	} {
		tx := h.seedTransfer(t, status)
		response := h.delete(t, tx.ID)
		require.Equal(t, http.StatusNoContent, response.Code, "status %s", status)
	}
}
