package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/receiver/internal/store"
	"github.com/opticaltransport/otp/receiver/internal/testdb"
)

// Deleting a transmission needs a real database and real object stores, for the same reason
// the sender's equivalent test does: the interesting behaviour is what happens to seven tables
// and three object prefixes together, which nothing but the real thing exercises honestly.
//
// The receiver's deletion shape differs from the sender's in the way that matters here: there is
// no foreign key to lean on. Every table but captured_frames carries a bare transmission_id with
// no constraint, so the seven deletes are explicit rather than a single row's cascade — and a
// test that only checked one or two of them would not prove the other five were touched.

// deleteHarness is a database, the main object store, the acks object store, and a server —
// delete touches all four.
type deleteHarness struct {
	store   *store.Store
	objects objectstore.Store
	acks    objectstore.Store
	handler http.Handler
}

func newDeleteHarness(t *testing.T) *deleteHarness {
	t.Helper()
	pool := testdb.New(t)

	objects, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = objects.Close() })

	acks, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = acks.Close() })

	st := store.New(pool)
	handler := New(Options{
		Store:   st,
		Objects: objects,
		Acks:    acks,
		Config:  config.NewWatcher("", config.Default()),
		Log:     zap.NewNop(),
	}).Routes()

	return &deleteHarness{store: st, objects: objects, acks: acks, handler: handler}
}

// seed writes one row into every table a transmission touches, plus every object its layout
// names, so the test proves the cascade removes exactly the right rows and objects rather than
// merely not erroring.
func (h *deleteHarness) seed(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()

	require.NoError(t, h.store.Manifests.Upsert(ctx, store.Manifest{
		TransmissionID: id,
		Filename:       "report.pdf",
		OriginalSize:   18,
		OriginalSHA256: sum("the merged file"),
		ChunkCount:     1,
		ChunkSize:      18,
	}))

	chunkKey := "chunks/" + id.String() + "/00000000.bin"
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, chunkKey, []byte("chunk-0")))
	_, err := h.store.Chunks.Insert(ctx, store.Chunk{
		TransmissionID: id, ChunkNumber: 0, SizeBytes: 7, StoredPath: chunkKey,
		SHA256: sum("chunk-0"),
	})
	require.NoError(t, err)

	require.NoError(t, h.store.Chunks.SetMissing(ctx, id, []int{3}))

	mergedKey := "merged/" + id.String() + "/report.pdf"
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, mergedKey, []byte("the merged file")))
	_, err = h.store.Merged.Upsert(ctx, store.MergedFile{
		TransmissionID: id, Filename: "report.pdf", StoredPath: mergedKey,
		SizeBytes: 15, SHA256: sum("the merged file"), Verified: true,
	})
	require.NoError(t, err)

	_, err = h.store.Acks.Next(ctx, id)
	require.NoError(t, err)

	ackKey := protocol.AckPath(id, 1)
	require.NoError(t, objectstore.PutBytes(ctx, h.acks, ackKey, []byte("an ack record")))

	_, err = h.store.Callbacks.Enqueue(ctx, store.Callback{TransmissionID: &id, URL: "https://example.com/hook", Event: "completed"})
	require.NoError(t, err)

	require.NoError(t, h.store.Stats.Record(ctx, "throughput_bytes_per_second", 1234.5, &id))

	return id
}

func (h *deleteHarness) delete(t *testing.T, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/v1/transmissions/"+id.String(), nil))
	return response
}

// TestDeleteTransmissionRemovesEveryTableAndObject is the whole point: every one of the seven
// tables keyed by transmission_id must be empty afterwards, and every object under the three
// key prefixes must be gone too.
func TestDeleteTransmissionRemovesEveryTableAndObject(t *testing.T) {
	h := newDeleteHarness(t)
	id := h.seed(t)
	ctx := context.Background()

	response := h.delete(t, id)
	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	require.Empty(t, response.Body.String())

	_, err := h.store.Manifests.Get(ctx, id)
	require.ErrorIs(t, err, store.ErrNotFound)

	chunks, err := h.store.Chunks.List(ctx, id)
	require.NoError(t, err)
	require.Empty(t, chunks)

	missing, err := h.store.Chunks.Missing(ctx, id)
	require.NoError(t, err)
	require.Empty(t, missing)

	_, err = h.store.Merged.Get(ctx, id)
	require.ErrorIs(t, err, store.ErrNotFound)

	deliveries, err := h.store.Callbacks.ForTransmission(ctx, id)
	require.NoError(t, err)
	require.Empty(t, deliveries)

	var statCount int
	require.NoError(t, h.store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM statistics WHERE transmission_id = $1`, id).Scan(&statCount))
	require.Zero(t, statCount)

	var ackCount int
	require.NoError(t, h.store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM ack_state WHERE transmission_id = $1`, id).Scan(&ackCount))
	require.Zero(t, ackCount)

	objects, err := h.objects.List(ctx, "chunks/"+id.String()+"/")
	require.NoError(t, err)
	require.Empty(t, objects)

	objects, err = h.objects.List(ctx, "merged/"+id.String()+"/")
	require.NoError(t, err)
	require.Empty(t, objects)

	acks, err := h.acks.List(ctx, protocol.AckDir(id)+"/")
	require.NoError(t, err)
	require.Empty(t, acks)
}

// TestDeleteTransmissionLeavesCapturedFramesAlone proves the one deliberate exception: frames
// are the capture audit log, keyed by session_id and cascading from capture_sessions, not from
// the transmission — so a delete must not touch them even when a frame names this transmission.
func TestDeleteTransmissionLeavesCapturedFramesAlone(t *testing.T) {
	h := newDeleteHarness(t)
	id := h.seed(t)
	ctx := context.Background()

	session, err := h.store.Sessions.Create(ctx, "file")
	require.NoError(t, err)
	require.NoError(t, h.store.Frames.Record(ctx, store.CapturedFrame{
		SessionID: session.ID, Sequence: 0, StoredPath: "frames/x.png",
		SHA256:         sum("a captured frame"),
		TransmissionID: &id,
	}))

	response := h.delete(t, id)
	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())

	frames, err := h.store.Frames.Recent(ctx, session.ID, 10)
	require.NoError(t, err)
	require.Len(t, frames, 1, "captured_frames is the audit log, not the file; it must survive")
}

// TestDeleteTransmissionWithoutManifestStillDeletes reproduces a documented-normal case the
// receiver's own pipeline describes: chunks can arrive before the manifest does, and are stored
// and counted while it waits — so a transmission can have real decoded_chunks rows and real
// chunks/<id>/* objects with no manifests row at all. A delete must still find and remove it
// rather than 404 while quietly destroying those objects on the way there.
func TestDeleteTransmissionWithoutManifestStillDeletes(t *testing.T) {
	h := newDeleteHarness(t)
	ctx := context.Background()
	id := uuid.New()

	chunkKey := "chunks/" + id.String() + "/00000000.bin"
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, chunkKey, []byte("chunk-0")))
	_, err := h.store.Chunks.Insert(ctx, store.Chunk{
		TransmissionID: id, ChunkNumber: 0, SizeBytes: 7, StoredPath: chunkKey,
		SHA256: sum("chunk-0"),
	})
	require.NoError(t, err)

	// No manifest row is written: this is the case a manifest-only existence check gets wrong.
	_, err = h.store.Manifests.Get(ctx, id)
	require.ErrorIs(t, err, store.ErrNotFound, "precondition: no manifest exists for this id")

	response := h.delete(t, id)
	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())

	chunks, err := h.store.Chunks.List(ctx, id)
	require.NoError(t, err)
	require.Empty(t, chunks, "the decoded_chunks row must be gone")

	objects, err := h.objects.List(ctx, "chunks/"+id.String()+"/")
	require.NoError(t, err)
	require.Empty(t, objects, "the chunk object must be gone")
}

func TestDeleteUnknownTransmissionIs404(t *testing.T) {
	h := newDeleteHarness(t)
	response := h.delete(t, uuid.New())
	require.Equal(t, http.StatusNotFound, response.Code)
}

func sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func TestDeleteTransmissionRefusesBadUUID(t *testing.T) {
	h := newDeleteHarness(t)
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/v1/transmissions/not-a-uuid", nil))
	require.Equal(t, http.StatusBadRequest, response.Code)
}
