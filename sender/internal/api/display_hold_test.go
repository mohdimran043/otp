package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/optical"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// Stopping the display, and moving it by hand.
//
// The page at /display is not a viewer onto the channel. Under ?camera=1 it *is* the channel's transmitting
// end — the surface a camera is pointed at — so a pause that froze only the local page would be a lie in the
// deployment that matters: the phone acting as the display would keep advancing while the laptop showed a
// still, and the operator would be aiming at something other than what they were looking at.
//
// So the hold is server-side, on the real display, and these endpoints are how it is driven. Stepping
// requires it: one rule instead of a permissive one whose behaviour depends on transfer status the operator
// cannot see.

// holdHarness is a database, an object store, and a live display — everything the step endpoint reads.
type holdHarness struct {
	store   *store.Store
	objects objectstore.Store
	display *optical.Live
	handler http.Handler
}

func newHoldHarness(t *testing.T) *holdHarness {
	t.Helper()
	pool := testdb.New(t)

	cfg := config.Default()
	cfg.Storage.Root = t.TempDir()

	objects, err := objectstore.Open(context.Background(), cfg.Storage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, objects.Close()) })

	st := store.New(pool)
	display := optical.NewLive(&discard{})
	handler := New(Options{
		Store:   st,
		Objects: objects,
		Config:  config.NewWatcher("", cfg),
		Display: display,
		Log:     zap.NewNop(),
	}).Routes()

	return &holdHarness{store: st, objects: objects, display: display, handler: handler}
}

func (h *holdHarness) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// heldNow reads the hold out of the display status, which is where every viewer learns it.
func (h *holdHarness) heldNow(t *testing.T) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/display", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Held bool `json:"held"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Held
}

// seedFrame stores one frame of a transmission and returns the transmission it belongs to.
func (h *holdHarness) seedFrame(t *testing.T, number int) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	fileID := uuid.New()
	fileKey, err := objectstore.Key("files", fileID.String())
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, fileKey, []byte("payload")))
	file, err := h.store.Files.Create(ctx, store.File{
		ID: fileID, Filename: "held.bin", StoredPath: fileKey,
		SizeBytes: 7, SHA256: hashString("payload"),
	})
	require.NoError(t, err)

	tx, err := h.store.Transmissions.Create(ctx, store.Transmission{FileID: file.ID})
	require.NoError(t, err)

	pixels := solidPNG(t, 8, 8, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	key, err := objectstore.Key("frames", tx.ID.String(), fmt.Sprintf("%d.png", number))
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, key, pixels))

	require.NoError(t, h.store.Frames.InsertMany(ctx, []store.Frame{{
		TransmissionID: tx.ID, FrameNumber: number, WidthPx: 8, HeightPx: 8,
		StoredPath: key, PayloadBytes: len(pixels), SHA256: hashString(key),
	}}))
	return tx.ID
}

// TestHoldingTheDisplayIsVisibleToEveryViewer: a second tab, or a page reloaded mid-hold, has to be able to
// read the state rather than infer it from frames that stopped arriving.
func TestHoldingTheDisplayIsVisibleToEveryViewer(t *testing.T) {
	h := newHoldHarness(t)

	require.False(t, h.heldNow(t))

	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/hold", "").Code)
	require.True(t, h.heldNow(t))

	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/release", "").Code)
	require.False(t, h.heldNow(t))
}

// TestHoldingTwiceIsNotAnError — two tabs, or two operators, pressing the same button. The UI cannot reliably
// know the current state at the moment of the click, and a conflict response would mean it had to.
func TestHoldingTwiceIsNotAnError(t *testing.T) {
	h := newHoldHarness(t)

	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/hold", "").Code)
	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/hold", "").Code)
	require.True(t, h.heldNow(t))

	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/release", "").Code)
	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/release", "").Code)
	require.False(t, h.heldNow(t))
}

// TestSteppingPutsTheChosenFrameOnTheDisplay is the feature: the operator picks a frame and that frame is
// what a camera pointed at the screen would see.
func TestSteppingPutsTheChosenFrameOnTheDisplay(t *testing.T) {
	h := newHoldHarness(t)
	id := h.seedFrame(t, 4)

	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/hold", "").Code)

	rec := h.post(t, "/api/v1/display/frame",
		fmt.Sprintf(`{"transmission_id":%q,"frame_number":4}`, id))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	frame, _, have := h.display.Current()
	require.True(t, have, "the frame has to reach the real display, not just the response")
	require.Equal(t, 4, frame.Number)
	require.Equal(t, id, frame.Transmission)
}

// TestSteppingIsRefusedWhileTheDisplayRuns is the one rule, and the reason it is worth the extra click.
//
// A running scheduler overwrites the operator's choice within a frame interval, so a step that succeeded here
// would be a control that appears to work and does not. Refusing says which of the two is in charge.
func TestSteppingIsRefusedWhileTheDisplayRuns(t *testing.T) {
	h := newHoldHarness(t)
	id := h.seedFrame(t, 0)

	rec := h.post(t, "/api/v1/display/frame",
		fmt.Sprintf(`{"transmission_id":%q,"frame_number":0}`, id))

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "hold")

	_, _, have := h.display.Current()
	require.False(t, have, "a refused step must not have reached the display")
}

// TestSteppingToAFrameThatDoesNotExist — an operator stepping past the end, or a stale page.
func TestSteppingToAFrameThatDoesNotExist(t *testing.T) {
	h := newHoldHarness(t)
	id := h.seedFrame(t, 0)

	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/hold", "").Code)

	rec := h.post(t, "/api/v1/display/frame",
		fmt.Sprintf(`{"transmission_id":%q,"frame_number":99}`, id))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// TestSteppingRejectsANegativeFrameNumber: frames are numbered from zero, so there is nothing below it.
func TestSteppingRejectsANegativeFrameNumber(t *testing.T) {
	h := newHoldHarness(t)
	id := h.seedFrame(t, 0)

	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/hold", "").Code)

	rec := h.post(t, "/api/v1/display/frame",
		fmt.Sprintf(`{"transmission_id":%q,"frame_number":-1}`, id))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestSteppingRejectsAnUnreadableBody — the ordinary malformed-request path.
func TestSteppingRejectsAnUnreadableBody(t *testing.T) {
	h := newHoldHarness(t)

	require.Equal(t, http.StatusOK, h.post(t, "/api/v1/display/hold", "").Code)

	rec := h.post(t, "/api/v1/display/frame", `{"transmission_id":"not-a-uuid"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}
