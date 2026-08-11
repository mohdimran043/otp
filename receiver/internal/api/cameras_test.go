package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// getCameras runs GET /api/v1/cameras against a handler built with the given source and
// BrowserActive function, and decodes the "streaming" field the page reads to light its indicator.
func getCameras(t *testing.T, source string, browserActive func() bool) bool {
	t.Helper()

	cfg := config.Default()
	cfg.Capture.Source = source
	handler := New(Options{
		Config:        config.NewWatcher("", cfg),
		Log:           zap.NewNop(),
		BrowserActive: browserActive,
	}).Routes()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Streaming bool `json:"streaming"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Streaming
}

// TestCamerasReportsStreamingForTheCameraSourceWithoutAsking covers the case that always worked:
// selecting "camera" opens the device immediately, so being selected is being open, and no
// BrowserActive function is needed to say so.
func TestCamerasReportsStreamingForTheCameraSourceWithoutAsking(t *testing.T) {
	require.True(t, getCameras(t, "camera", nil))
}

// TestCamerasReportsNotStreamingForTheFileSource covers the other case that always worked: reading
// from a directory never lights the indicator.
func TestCamerasReportsNotStreamingForTheFileSource(t *testing.T) {
	require.False(t, getCameras(t, "file", func() bool { return true }))
}

// TestCamerasReportsStreamingForABrowserActuallyPostingFrames is the bug this fixes: a browser
// source selected and taking frames must report streaming, not just the "camera" source.
func TestCamerasReportsStreamingForABrowserActuallyPostingFrames(t *testing.T) {
	require.True(t, getCameras(t, "browser", func() bool { return true }))
}

// TestCamerasReportsNotStreamingForABrowserSourceThatIsSilent is the other half of the fix: a
// browser source that is merely selected — nobody pressed Start, or the page navigated away — must
// not report streaming, unlike the old rule that could only say true or false per source name.
func TestCamerasReportsNotStreamingForABrowserSourceThatIsSilent(t *testing.T) {
	require.False(t, getCameras(t, "browser", func() bool { return false }))
}

// TestCamerasReportsNotStreamingForABrowserSourceWithNoActivitySignal covers a build or deployment
// that never wires up BrowserActive at all: it must fail closed rather than assume the browser
// source is live just because it was selected.
func TestCamerasReportsNotStreamingForABrowserSourceWithNoActivitySignal(t *testing.T) {
	require.False(t, getCameras(t, "browser", nil))
}
