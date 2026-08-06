package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/optical"
)

// The display endpoints are the one part of this API that needs no database: they report what is on the
// screen, and the screen is in memory. So they are tested directly, which is worth doing because their
// contract is what a camera-facing page and an HTTP receiver both depend on — and the interesting parts of
// it are the edges: nothing displayed yet, nothing new to report, and a caller that has fallen behind.

// discard is a sink that accepts everything, standing in for the real channel.
type discard struct{ shown int64 }

func (d *discard) Name() string                              { return "discard" }
func (d *discard) Show(context.Context, optical.Frame) error { d.shown++; return nil }
func (d *discard) Shown() int64                              { return d.shown }
func (d *discard) Close() error                              { return nil }

func newDisplayServer(t *testing.T, live *optical.Live) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Optical.Encoder = "color8"
	cfg.Optical.GridWidth, cfg.Optical.GridHeight = 96, 96
	cfg.Optical.CellPixels = 6
	cfg.Display.FPS = 30
	return New(Options{
		Config:  config.NewWatcher("", cfg),
		Display: live,
		Log:     zap.NewNop(),
	}).Routes()
}

func TestDisplayReportsAnIdleScreen(t *testing.T) {
	handler := newDisplayServer(t, optical.NewLive(&discard{}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/display", nil))
	require.Equal(t, http.StatusOK, response.Code)

	var body struct {
		Sink  string  `json:"sink"`
		Live  bool    `json:"live"`
		FPS   float64 `json:"fps"`
		Frame *struct {
			Sequence int64 `json:"sequence"`
		} `json:"frame"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, "discard", body.Sink)
	require.False(t, body.Live)
	require.Equal(t, 30.0, body.FPS, "configuration is reported even with nothing on screen")
	require.Nil(t, body.Frame)

	// The pixels are a 404 rather than an empty image: a viewer that got a blank PNG would render it, and
	// a black screen is indistinguishable from a frame that failed to load.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/display/frame.png", nil))
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestDisplayReportsTheFrameOnScreen(t *testing.T) {
	live := optical.NewLive(&discard{})
	handler := newDisplayServer(t, live)

	transmission := uuid.New()
	require.NoError(t, live.Show(context.Background(), optical.Frame{
		Number:       3,
		Transmission: transmission,
		WidthPx:      600,
		HeightPx:     600,
		PNG:          []byte("not really a png"),
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/display", nil))
	require.Equal(t, http.StatusOK, response.Code)

	var body struct {
		Live  bool `json:"live"`
		Frame struct {
			Sequence       int64  `json:"sequence"`
			FrameNumber    int    `json:"frame_number"`
			TransmissionID string `json:"transmission_id"`
			WidthPx        int    `json:"width_px"`
			Bytes          int    `json:"bytes"`
			ImageURL       string `json:"image_url"`
			ImagePNG       string `json:"image_png"`
		} `json:"frame"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.True(t, body.Live)
	require.Equal(t, int64(1), body.Frame.Sequence, "the display assigns the sequence")
	require.Equal(t, 3, body.Frame.FrameNumber)
	require.Equal(t, transmission.String(), body.Frame.TransmissionID)
	require.Equal(t, 600, body.Frame.WidthPx)
	require.Equal(t, len("not really a png"), body.Frame.Bytes)
	require.Contains(t, body.Frame.ImageURL, "sequence=1",
		"each frame needs a distinct URL, or a browser serves the previous one from cache")
	require.Empty(t, body.Frame.ImagePNG, "the image is only inlined when it was asked for")

	// The pixels, with the headers that say which frame they are.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/display/frame.png", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "image/png", response.Header().Get("Content-Type"))
	require.Equal(t, "1", response.Header().Get("X-OTP-Sequence"))
	require.Equal(t, "3", response.Header().Get("X-OTP-Frame-Number"))
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"),
		"the live frame is the opposite of cacheable: the whole point is that it changes")
	require.Equal(t, "not really a png", response.Body.String())
}

// TestDisplayInlinesTheImageOnRequest covers the reason include=image exists: at frame rate, a second
// round trip per frame costs a meaningful share of the interval between frames.
func TestDisplayInlinesTheImageOnRequest(t *testing.T) {
	live := optical.NewLive(&discard{})
	handler := newDisplayServer(t, live)
	require.NoError(t, live.Show(context.Background(), optical.Frame{PNG: []byte("pixels")}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/display/next?after=0&include=image", nil))
	require.Equal(t, http.StatusOK, response.Code)

	var frame struct {
		Sequence int64  `json:"sequence"`
		ImagePNG string `json:"image_png"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &frame))
	require.Equal(t, int64(1), frame.Sequence)

	decoded, err := base64.StdEncoding.DecodeString(frame.ImagePNG)
	require.NoError(t, err)
	require.Equal(t, []byte("pixels"), decoded)
}

// TestDisplayNextAnswers204WhenNothingIsNew is the behaviour a follower depends on. An expired poll is the
// normal case on a display that is holding a frame, and answering with an error — or with the same frame
// again — would make a caller either report a fault or spin.
func TestDisplayNextAnswers204WhenNothingIsNew(t *testing.T) {
	live := optical.NewLive(&discard{})
	handler := newDisplayServer(t, live)
	require.NoError(t, live.Show(context.Background(), optical.Frame{PNG: []byte("one")}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/display/next?after=1&timeout=1s", nil))
	require.Equal(t, http.StatusNoContent, response.Code)
	require.Empty(t, response.Body.String())
}

// TestDisplayNextReturnsImmediatelyWhenBehind is the other half: a viewer that has fallen behind wants the
// screen as it is now, not a wait for the next change.
func TestDisplayNextReturnsImmediatelyWhenBehind(t *testing.T) {
	live := optical.NewLive(&discard{})
	handler := newDisplayServer(t, live)
	require.NoError(t, live.Show(context.Background(), optical.Frame{PNG: []byte("a frame")}))

	started := time.Now()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/display/next?after=0&timeout=20s", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Less(t, time.Since(started), 2*time.Second, "a caller that is behind must not be made to wait")
}

func TestDisplayNextRefusesNonsense(t *testing.T) {
	handler := newDisplayServer(t, optical.NewLive(&discard{}))

	for _, query := range []string{"?after=soon", "?timeout=never"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/display/next"+query, nil))
		require.Equal(t, http.StatusBadRequest, response.Code, "query %q", query)
	}
}

// TestDisplayEndpointsSayWhenThereIsNoDisplay keeps a deployment without one from looking broken: a 501
// with a sentence is a different thing from a 500.
func TestDisplayEndpointsSayWhenThereIsNoDisplay(t *testing.T) {
	handler := New(Options{
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
	}).Routes()

	for _, path := range []string{"/api/v1/display", "/api/v1/display/next", "/api/v1/display/frame.png"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotImplemented, response.Code, "path %q", path)
		require.Contains(t, response.Body.String(), "no live display")
	}
}
