package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Switching encoder should not require knowing its bit depths.
//
// Each encoding accepts its own set: binary is one bit a cell, color8 is three, color16 is four, grayscale takes
// two or three. The bit depth was applied from the request independently of the encoder, so a depth left over
// from the previous encoding was carried into the new one and validation refused the whole change:
//
//	optical.bit_depth 1 is not one the color8 encoder offers ([3])
//
// Reachable straight from the settings page — pick binary, then pick color8 again, and the form is stuck with an
// error naming an internal field the operator never set. It is worse with the settings now persisted, because the
// stale depth survives a restart, so the deployment stays wedged until someone sends both fields together.
//
// An encoder change with no depth named means "use this encoding", so the depth follows it. A depth named
// explicitly is respected, and rejected on its own terms if it is wrong.

// TestSwitchingEncoderAdoptsADepthItSupports is the operator's path: choose an encoding, nothing else.
func TestSwitchingEncoderAdoptsADepthItSupports(t *testing.T) {
	h := newSettingsHarness(t)

	// Start on binary at one bit a cell, as a camera-only setup would.
	rec := patchSettings(t, h, map[string]any{"encoder": "binary", "bit_depth": 1})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Now choose color8 and say nothing about depth. This used to fail.
	rec = patchSettings(t, h, map[string]any{"encoder": "color8"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "color8", got["encoder"])
	require.EqualValues(t, 3, got["bit_depth"], "the depth follows the encoding it belongs to")
}

// TestSwitchingBackAlsoWorks — the other direction, since color8's three bits are equally wrong for binary.
func TestSwitchingBackAlsoWorks(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]any{"encoder": "color8", "bit_depth": 3})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = patchSettings(t, h, map[string]any{"encoder": "binary"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "binary", got["encoder"])
	require.EqualValues(t, 1, got["bit_depth"])
}

// TestAnExplicitDepthIsHonoured: the operator who does know is not overridden. color16 offers only four, so
// asking for four with the encoder must keep four rather than being replaced by a default.
func TestAnExplicitDepthIsHonoured(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]any{"encoder": "color16", "bit_depth": 4})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "color16", got["encoder"])
	require.EqualValues(t, 4, got["bit_depth"])
}

// TestAnExplicitlyWrongDepthIsStillRefused — the fix must not paper over a genuine mistake by silently
// substituting something that works.
func TestAnExplicitlyWrongDepthIsStillRefused(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]any{"encoder": "color8", "bit_depth": 1})
	require.NotEqual(t, http.StatusOK, rec.Code, "asking for a depth the encoding cannot do is an error")
	require.Contains(t, rec.Body.String(), "bit_depth")
}

// TestChangingDepthAloneStillWorks: no encoder in the request, so nothing about the encoding is being decided and
// grayscale's two supported depths must both remain reachable.
func TestChangingDepthAloneStillWorks(t *testing.T) {
	h := newSettingsHarness(t)

	rec := patchSettings(t, h, map[string]any{"encoder": "grayscale", "bit_depth": 2})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = patchSettings(t, h, map[string]any{"bit_depth": 3})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.EqualValues(t, 3, got["bit_depth"])
}
