package api

import (
	"bytes"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// TestPostFrameRejectsAnOversizedImageWithoutDecodingIt covers the same guard as import.go's,
// applied to the browser-capture path: a posted image whose header declares far more pixels
// than maxDecodedPixels must be refused before image.Decode ever allocates its pixel buffer.
// The body here is a hand-crafted PNG header with no real pixel data — decoding it fully would
// either fail or allocate gigabytes, so a quick 4xx response is the proof the size check runs
// first.
func TestPostFrameRejectsAnOversizedImageWithoutDecodingIt(t *testing.T) {
	huge := hugePNGHeader(60000, 60000) // 3.6 billion pixels, far past the 64-megapixel bound

	pushed := false
	handler := New(Options{
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
		Push: func(img image.Image, raw []byte) (bool, error) {
			pushed = true
			return true, nil
		},
	}).Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/capture/frames", bytes.NewReader(huge))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	require.False(t, pushed, "an oversized posted frame must never reach the capture source")
}

// TestPostFrameAcceptsAnOrdinaryImage is the control case: a real, modestly sized PNG must
// still make it all the way to the injected capture source, so the size guard above is proven
// to be selective rather than accidentally rejecting everything.
func TestPostFrameAcceptsAnOrdinaryImage(t *testing.T) {
	png := solidPNG(t, 4, 4, color.RGBA{R: 255, A: 255})

	var pushedImg image.Image
	handler := New(Options{
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
		Push: func(img image.Image, raw []byte) (bool, error) {
			pushedImg = img
			return true, nil
		},
	}).Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/capture/frames", bytes.NewReader(png))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, pushedImg)
}
