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

// Whether posted frames were gated has to be visible from outside.
//
// The browser source counts every frame it is handed, every one the blank-screen gate turned away, and every one
// dropped because the queue was full — and none of it left the process. So "the camera posted 400 frames and all
// were gated" and "the camera posted nothing at all" looked identical from the API: session counters flat, no
// stored frames, no failures, nothing in the log. Three separate debugging sessions dead-ended on exactly that
// ambiguity, each one a guess about the operator's aim when the answer was a counter nobody could read.
//
// The browser reports these numbers to its own user in the chips under the preview. Anyone looking at the
// receiver could not see them at all.

func camerasBody(t *testing.T, stats func() (int64, int64, int64)) map[string]any {
	t.Helper()
	cfg := config.Default()
	cfg.Capture.Source = "browser"
	handler := New(Options{
		Config:       config.NewWatcher("", cfg),
		Log:          zap.NewNop(),
		BrowserStats: stats,
	}).Routes()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// TestCamerasReportsWhatTheBrowserSourceSaw is the case that was invisible: frames arriving and every one turned
// away by the gate. Received above zero with idle equal to it says "posting, all rejected" — which is a framing
// or lighting problem — as against received zero, which says the camera is not posting at all.
func TestCamerasReportsWhatTheBrowserSourceSaw(t *testing.T) {
	body := camerasBody(t, func() (int64, int64, int64) { return 412, 412, 0 })

	require.EqualValues(t, 412, body["posted"], "frames handed to the source")
	require.EqualValues(t, 412, body["gated"], "frames the blank-screen test turned away")
	require.EqualValues(t, 0, body["dropped"])
}

func TestCamerasReportsAHealthyBrowserSource(t *testing.T) {
	body := camerasBody(t, func() (int64, int64, int64) { return 300, 12, 3 })

	require.EqualValues(t, 300, body["posted"])
	require.EqualValues(t, 12, body["gated"])
	require.EqualValues(t, 3, body["dropped"], "queue was full, so the decoder is behind")
}

// A build or deployment with no browser source wired up reports zeroes rather than omitting the fields, so a
// reader never has to guess whether absent means none or means unknown.
func TestCamerasReportsZeroesWithoutABrowserSource(t *testing.T) {
	body := camerasBody(t, nil)

	require.EqualValues(t, 0, body["posted"])
	require.EqualValues(t, 0, body["gated"])
	require.EqualValues(t, 0, body["dropped"])
}
