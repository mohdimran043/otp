package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// Settings that survive a restart.
//
// The bug these cover: the settings API applied a change to the running configuration and wrote nothing down,
// so the display sink — which is only read when the process starts — was discarded by the very restart it
// needed in order to take effect. Choosing "camera" as the transfer channel therefore did nothing, ever.

func TestDisplaySettingsStoreRoundTrip(t *testing.T) {
	st := store.New(testdb.New(t))
	ctx := context.Background()

	stored, err := st.DisplaySettings.All(ctx)
	require.NoError(t, err)
	require.Empty(t, stored, "nothing is stored until an operator changes something")

	require.NoError(t, st.DisplaySettings.Set(ctx, map[string]string{"sink": "none", "fps": "25"}))

	stored, err = st.DisplaySettings.All(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"sink": "none", "fps": "25"}, stored)
}

// TestDisplaySettingsSetOverwritesRatherThanDuplicating — the key is the primary key, so changing the same
// setting twice must leave one row holding the newer value.
func TestDisplaySettingsSetOverwritesRatherThanDuplicating(t *testing.T) {
	st := store.New(testdb.New(t))
	ctx := context.Background()

	require.NoError(t, st.DisplaySettings.Set(ctx, map[string]string{"sink": "none"}))
	require.NoError(t, st.DisplaySettings.Set(ctx, map[string]string{"sink": "file"}))

	stored, err := st.DisplaySettings.All(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"sink": "file"}, stored)
}

// TestDisplaySettingsSetLeavesOtherKeysAlone is what keeps the storage sparse: changing the frame rate must
// not disturb a sink stored earlier, or the two would overwrite each other and only the last change made
// through the UI would ever survive.
func TestDisplaySettingsSetLeavesOtherKeysAlone(t *testing.T) {
	st := store.New(testdb.New(t))
	ctx := context.Background()

	require.NoError(t, st.DisplaySettings.Set(ctx, map[string]string{"sink": "none"}))
	require.NoError(t, st.DisplaySettings.Set(ctx, map[string]string{"fps": "25"}))

	stored, err := st.DisplaySettings.All(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"sink": "none", "fps": "25"}, stored)
}

// patchSettings sends a settings change through the real handler and returns the recorder.
//
// newSettingsHarness is reused rather than rebuilt because it already sets the two secrets that have no
// default: Validate runs over the whole configuration on every change, so without them a sink change is
// rejected for a reason that has nothing to do with the sink.
func patchSettings(t *testing.T, h *settingsHarness, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// TestPatchingTheSinkStoresIt is the end of the chain the operator actually walks: change the transfer
// channel in the UI, and the choice is on disk rather than only in memory. Combined with the overlay at
// startup, that is what makes it outlive the restart.
func TestPatchingTheSinkStoresIt(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]string{"sink": "none"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored, err := h.store.DisplaySettings.All(context.Background())
	require.NoError(t, err)
	require.Equal(t, "none", stored["sink"], "the sink the operator chose has to be on disk, not only applied")
}

// TestPatchingStoresOnlyWhatTheRequestNamed: a PATCH carrying one field must not freeze the other ten at
// whatever they happened to be, or the first change through the UI would pin the whole configuration and
// later edits to .env would stop working for fields nobody ever touched.
func TestPatchingStoresOnlyWhatTheRequestNamed(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]any{"fps": 25})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored, err := h.store.DisplaySettings.All(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"fps"}, keysOf(stored), "only the field the request named should be stored")
}

// TestStoredSettingsSurviveAConfigReload is the whole feature in one assertion: what the API stored, laid
// over a freshly loaded configuration, is what the process would come back up with.
func TestStoredSettingsSurviveAConfigReload(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]string{"sink": "none"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A restart: the configuration comes back from file and environment knowing nothing of the change.
	restarted := config.Default()
	require.Equal(t, "file", restarted.Display.Sink, "the configured default is still file")

	stored, err := h.store.DisplaySettings.All(context.Background())
	require.NoError(t, err)
	overlaid, err := restarted.WithOverrides(stored)
	require.NoError(t, err)

	require.Equal(t, "none", overlaid.Display.Sink,
		"the operator's channel choice has to come back after a restart; before this it did not")
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
