package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// What is stored has to be a configuration the sender can start on.
//
// The encoder and the bit depth constrain each other — binary is one bit a cell, color8 three — and the
// settings API knows it: an encoder change that names no depth adopts the one the encoding offers, so the
// running configuration is always a valid pair. The store was not held to the same rule. It is written from
// the fields the *request* named, and an encoder change names no depth, so the depth left over from the
// previous encoding stayed behind as a row.
//
// That divergence is invisible until a restart, and then it is total. The startup overlay validates the
// stored settings as a whole and drops *all* of them when they do not hold together, so one stale depth
// silently reverts every setting the operator ever changed — the frame rate, the geometry, and the display
// sink with them. A sender configured for a camera comes back writing PNGs to a directory, and the display
// page an operator is watching stays blank with nothing to say why.
//
// So the invariant is not "the depth is stored" but the stronger one these assert: whatever is in the store
// must still overlay into a configuration that validates.

// overlaidFromStore lays everything the API has stored over a default configuration, the way startup does.
func overlaidFromStore(t *testing.T, h *settingsHarness) (config.Config, error) {
	t.Helper()

	stored, err := h.store.DisplaySettings.All(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, stored, "the patches under test should have stored something")

	// The same two secrets the harness sets: Validate covers the whole configuration, so without them a
	// stored display setting would be blamed for an unrelated failure.
	base := config.Default()
	base.Ack.Secret = "test acknowledgement secret"
	base.Auth.JWTSecret = "a test jwt secret long enough to sign"

	overlaid, err := base.WithOverrides(stored)
	if err != nil {
		return config.Config{}, err
	}
	return overlaid, overlaid.Validate()
}

// TestSwitchingEncoderLeavesTheStoreStartable is the path that wedged the deployment: pick binary at one bit
// a cell, then pick color8 and say nothing about depth. The response is correct either way — this is about
// what the next restart reads.
func TestSwitchingEncoderLeavesTheStoreStartable(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]any{"encoder": "binary", "bit_depth": 1})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = patchSettings(t, h, map[string]any{"encoder": "color8"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	overlaid, err := overlaidFromStore(t, h)
	require.NoError(t, err,
		"the stored settings no longer make a valid configuration, so the next restart discards every one of them")
	require.Equal(t, "color8", overlaid.Optical.Encoder)
	require.Equal(t, 3, overlaid.Optical.BitDepth,
		"the depth the encoder adopted has to be stored beside it, or the pair is only valid in memory")
}

// TestSwitchingEncoderBackLeavesTheStoreStartable is the other direction, since color8's three bits are
// equally wrong for binary.
func TestSwitchingEncoderBackLeavesTheStoreStartable(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]any{"encoder": "color8", "bit_depth": 3})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = patchSettings(t, h, map[string]any{"encoder": "binary"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	overlaid, err := overlaidFromStore(t, h)
	require.NoError(t, err)
	require.Equal(t, "binary", overlaid.Optical.Encoder)
	require.Equal(t, 1, overlaid.Optical.BitDepth)
}

// TestAStoredSinkSurvivesAnEncoderChange is the operator-visible consequence, and the reason this matters
// more than a stale integer: the sink is read only at startup, so it is the setting with the most to lose
// from an overlay that gets dropped. Choosing the camera channel and then changing encoding must not put
// the sender back on the file sink at the next restart.
func TestAStoredSinkSurvivesAnEncoderChange(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]any{"sink": "none"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = patchSettings(t, h, map[string]any{"encoder": "binary", "bit_depth": 1})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = patchSettings(t, h, map[string]any{"encoder": "color8"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	overlaid, err := overlaidFromStore(t, h)
	require.NoError(t, err)
	require.Equal(t, "none", overlaid.Display.Sink,
		"an unrelated encoder change must not cost the operator the transfer channel they chose")
}
