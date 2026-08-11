package reap_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/reap"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// harness gives every test a database and an object store, with objects laid out exactly the
// way the pipeline lays them out — the same key shapes pipeline.go builds — so a passing test
// says something about the real key scheme rather than a stand-in for it.
type harness struct {
	store   *store.Store
	objects objectstore.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := testdb.New(t)

	cfg := config.Default()
	cfg.Storage.Root = t.TempDir()
	objects, err := objectstore.Open(context.Background(), cfg.Storage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, objects.Close()) })

	return &harness{store: store.New(pool), objects: objects}
}

// seedTransfer writes a file, a transmission, chunk and frame rows, and every object those rows
// name — the shape of a completed transfer, ready to be deleted.
func (h *harness) seedTransfer(t *testing.T, status store.TransmissionStatus) store.Transmission {
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

	var chunks []store.Chunk
	for esi := 0; esi < 3; esi++ {
		key, err := objectstore.Key("transmissions", tx.ID.String(), "chunks", fmt.Sprintf("%08d.bin", esi))
		require.NoError(t, err)
		body := fmt.Sprintf("chunk-%d", esi)
		require.NoError(t, objectstore.PutBytes(ctx, h.objects, key, []byte(body)))
		chunks = append(chunks, store.Chunk{
			TransmissionID: tx.ID, ESI: esi, SizeBytes: len(body),
			SHA256: sum(body), StoredPath: key,
		})
	}
	require.NoError(t, h.store.Chunks.InsertMany(ctx, chunks))

	var frames []store.Frame
	for n := 0; n < 2; n++ {
		key, err := objectstore.Key("transmissions", tx.ID.String(), "frames", fmt.Sprintf("%08d.png", n))
		require.NoError(t, err)
		body := fmt.Sprintf("frame-%d", n)
		require.NoError(t, objectstore.PutBytes(ctx, h.objects, key, []byte(body)))
		frames = append(frames, store.Frame{
			TransmissionID: tx.ID, FrameNumber: n, IsManifest: n == 0,
			WidthPx: 4, HeightPx: 4, PayloadBytes: len(body),
			StoredPath: key, SHA256: sum(body),
		})
	}
	require.NoError(t, h.store.Frames.InsertMany(ctx, frames))

	require.NoError(t, h.store.Transmissions.SetStatus(ctx, tx.ID, status, ""))
	tx, err = h.store.Transmissions.Get(ctx, tx.ID)
	require.NoError(t, err)
	return tx
}

func sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func TestTransferRemovesRowsAndObjects(t *testing.T) {
	h := newHarness(t)
	tx := h.seedTransfer(t, store.TxCompleted)
	ctx := context.Background()

	require.NoError(t, reap.Transfer(ctx, h.store, h.objects, zap.NewNop(), tx.ID))

	_, err := h.store.Transmissions.Get(ctx, tx.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = h.store.Files.Get(ctx, tx.FileID)
	require.ErrorIs(t, err, store.ErrNotFound)

	chunkObjects, err := h.objects.List(ctx, fmt.Sprintf("transmissions/%s/chunks/", tx.ID))
	require.NoError(t, err)
	require.Empty(t, chunkObjects)

	frameObjects, err := h.objects.List(ctx, fmt.Sprintf("transmissions/%s/frames/", tx.ID))
	require.NoError(t, err)
	require.Empty(t, frameObjects)

	compressedKey, err := objectstore.Key("transmissions", tx.ID.String(), "compressed.bin")
	require.NoError(t, err)
	exists, err := h.objects.Exists(ctx, compressedKey)
	require.NoError(t, err)
	require.False(t, exists, "the compressed stream must be gone too")

	fileKey, err := objectstore.Key("files", tx.FileID.String())
	require.NoError(t, err)
	exists, err = h.objects.Exists(ctx, fileKey)
	require.NoError(t, err)
	require.False(t, exists, "the uploaded file's own object must be gone")
}

// TestTransferOfUnknownIDIsErrNotFound is what the API handler and the retention job both rely
// on to answer 404 or to skip an already-gone row without treating it as a fault.
func TestTransferOfUnknownIDIsErrNotFound(t *testing.T) {
	h := newHarness(t)
	err := reap.Transfer(context.Background(), h.store, h.objects, zap.NewNop(), uuid.New())
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestTransferIsIdempotent covers what makes the retention job and a repeated DELETE both
// safe: deleting an object twice, or listing a prefix nothing lives under any more, is not an
// error, so a transfer already reaped by a previous run causes no new failures — the only
// error is the ErrNotFound above, once the row itself is gone.
func TestTransferIsIdempotent(t *testing.T) {
	h := newHarness(t)
	tx := h.seedTransfer(t, store.TxFailed)
	ctx := context.Background()

	require.NoError(t, reap.Transfer(ctx, h.store, h.objects, zap.NewNop(), tx.ID))
	err := reap.Transfer(ctx, h.store, h.objects, zap.NewNop(), tx.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "the second call finds the row already gone")
}
