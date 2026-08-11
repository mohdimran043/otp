package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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

// controlHarness provides a database, job store, and server with a transmit spy
// for testing control endpoints like start, resume, pause, and cancel.
type controlHarness struct {
	store            *store.Store
	jobs             *jobs.Store
	handler          http.Handler
	transmitCalls    []uuid.UUID // spy: track which transmissions were started
	transmitCallFunc func(ctx context.Context, id uuid.UUID)
}

func newControlHarness(t *testing.T) *controlHarness {
	t.Helper()
	pool := testdb.New(t)

	cfg := config.Default()
	cfg.Storage.Root = t.TempDir()
	objects, err := objectstore.Open(context.Background(), cfg.Storage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, objects.Close()) })

	st := store.New(pool)
	js := jobs.NewStore(pool)

	h := &controlHarness{store: st, jobs: js}

	// transmit is a spy: it records calls so we can assert on them in tests.
	h.transmitCallFunc = func(ctx context.Context, id uuid.UUID) {
		h.transmitCalls = append(h.transmitCalls, id)
	}

	handler := New(Options{
		Store:   st,
		Jobs:    js,
		Objects: objects,
		Config:  config.NewWatcher("", cfg),
		Log:     zap.NewNop(),
		Transmit: h.transmitCallFunc,
	}).Routes()

	h.handler = handler
	return h
}

// seedTransfer writes a file and transmission with the given status, similar to delete tests.
func (h *controlHarness) seedTransfer(t *testing.T, status store.TransmissionStatus) store.Transmission {
	t.Helper()
	ctx := context.Background()

	fileID := uuid.New()
	fileKey, err := objectstore.Key("files", fileID.String())
	require.NoError(t, err)

	file, err := h.store.Files.Create(ctx, store.File{
		ID: fileID, Filename: "report.pdf", StoredPath: fileKey,
		SizeBytes: 18, SHA256: hashString("the original file"),
	})
	require.NoError(t, err)

	tx, err := h.store.Transmissions.Create(ctx, store.Transmission{FileID: file.ID})
	require.NoError(t, err)

	require.NoError(t, h.store.Transmissions.SetStatus(ctx, tx.ID, status, ""))
	tx, err = h.store.Transmissions.Get(ctx, tx.ID)
	require.NoError(t, err)
	return tx
}

func hashString(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// TestStartReadyTransferReturns200AndInvokesTransmit checks that a ready transfer
// can be started: the endpoint returns 200 with the correct response shape, and
// the transmit spy is invoked to actually display it.
func TestStartReadyTransferReturns200AndInvokesTransmit(t *testing.T) {
	h := newControlHarness(t)
	tx := h.seedTransfer(t, store.TxReady)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/transfers/"+tx.ID.String()+"/start", nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var resp controlResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &resp))
	require.Equal(t, tx.ID.String(), resp.TransmissionID)
	require.Equal(t, string(store.TxReady), resp.Status)
	require.Equal(t, tx.AckedChunks, resp.AckedChunks)
	require.Equal(t, tx.ChunkCount, resp.ChunkCount)
	require.Contains(t, resp.Note, "Displaying")

	// The transmit spy was invoked.
	require.Len(t, h.transmitCalls, 1)
	require.Equal(t, tx.ID, h.transmitCalls[0])
}

// TestStartNonReadyTransferReturns409 ensures that only ready transfers can be started.
// A completed or pending transfer should return 409 Conflict.
func TestStartNonReadyTransferReturns409(t *testing.T) {
	h := newControlHarness(t)
	for _, status := range []store.TransmissionStatus{store.TxCompleted, store.TxPending, store.TxTransmitting} {
		tx := h.seedTransfer(t, status)

		response := httptest.NewRecorder()
		h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/transfers/"+tx.ID.String()+"/start", nil))
		require.Equal(t, http.StatusConflict, response.Code, "status %s should not be startable", status)

		// The transmit spy should never be called for non-ready transfers.
		require.Empty(t, h.transmitCalls)
	}
}

// TestStartUnknownTransferReturns404 ensures that starting a non-existent transfer
// returns 404 Not Found.
func TestStartUnknownTransferReturns404(t *testing.T) {
	h := newControlHarness(t)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/transfers/"+uuid.New().String()+"/start", nil))
	require.Equal(t, http.StatusNotFound, response.Code)

	require.Empty(t, h.transmitCalls, "transmit should not be called for unknown transfers")
}

// TestStartBadUUIDReturns400 ensures that a malformed UUID in the path
// returns 400 Bad Request.
func TestStartBadUUIDReturns400(t *testing.T) {
	h := newControlHarness(t)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/transfers/not-a-uuid/start", nil))
	require.Equal(t, http.StatusBadRequest, response.Code)

	require.Empty(t, h.transmitCalls, "transmit should not be called for bad UUIDs")
}
