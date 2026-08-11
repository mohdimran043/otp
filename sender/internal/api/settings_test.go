package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// settingsHarness is a server backed by a real database, which the settings endpoint needs: the
// guard on sink and geometry changes asks the store how many transmissions are active.
type settingsHarness struct {
	store   *store.Store
	watcher *config.Watcher
	handler http.Handler
}

func newSettingsHarness(t *testing.T) *settingsHarness {
	t.Helper()
	pool := testdb.New(t)
	st := store.New(pool)

	// Validate is run again on every settings change, over the whole configuration — so the two
	// secrets that have no default (see config.Validate) have to be set here or an unrelated change
	// like the sink would be rejected for a reason that has nothing to do with it.
	cfg := config.Default()
	cfg.Ack.Secret = "test acknowledgement secret"
	cfg.Auth.JWTSecret = "a test jwt secret long enough to sign"
	watcher := config.NewWatcher("", cfg)

	handler := New(Options{
		Store:  st,
		Config: watcher,
		Log:    zap.NewNop(),
	}).Routes()

	return &settingsHarness{store: st, watcher: watcher, handler: handler}
}

// seedTransmitting creates a file and a transmission in the "transmitting" state, which is what
// CountActive counts and what should make the settings endpoint refuse a sink or geometry change.
func (h *settingsHarness) seedTransmitting(t *testing.T) store.Transmission {
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

	require.NoError(t, h.store.Transmissions.SetStatus(ctx, tx.ID, store.TxTransmitting, ""))
	tx, err = h.store.Transmissions.Get(ctx, tx.ID)
	require.NoError(t, err)
	return tx
}

func (h *settingsHarness) patch(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	return response
}

// TestGetSettingsReportsTheConfiguredSink covers the response side: an operator reading the settings
// page needs to see which channel is active, not only the geometry and frame rate.
func TestGetSettingsReportsTheConfiguredSink(t *testing.T) {
	h := newSettingsHarness(t)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var view settingsView
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &view))
	require.Equal(t, "file", view.Sink, "the default deployment writes to the shared directory")
}

// TestPatchSettingsAppliesSinkNoneWhileIdle is the camera-only path: with nothing in flight, switching
// to the discard sink is applied and echoed back in the response.
func TestPatchSettingsAppliesSinkNoneWhileIdle(t *testing.T) {
	h := newSettingsHarness(t)

	response := h.patch(t, `{"sink": "none"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var view settingsView
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &view))
	require.Equal(t, "none", view.Sink)

	require.Equal(t, "none", h.watcher.Current().Display.Sink,
		"the running configuration must carry the change, even though the already-open sink cannot swap")
}

// TestPatchSettingsRefusesSinkWhileTransmitting covers the guard: swapping the channel mid-transfer
// would move frames somewhere the receiver on the other end is not watching, the same hazard a
// geometry change is refused for.
func TestPatchSettingsRefusesSinkWhileTransmitting(t *testing.T) {
	h := newSettingsHarness(t)
	h.seedTransmitting(t)

	response := h.patch(t, `{"sink": "none"}`)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())

	require.Equal(t, "file", h.watcher.Current().Display.Sink, "a refused change must not be applied")
}

// TestPatchSettingsRejectsAnUnknownSink covers the validation path: a typo in the sink name must be
// caught here rather than surfacing later as "this build has no OpenGL sink" for a name that was never
// opengl to begin with.
func TestPatchSettingsRejectsAnUnknownSink(t *testing.T) {
	h := newSettingsHarness(t)

	response := h.patch(t, `{"sink": "crt"}`)
	require.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
}
