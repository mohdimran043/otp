package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The stored-settings overlay.
//
// These are the eleven fields the settings API can change. Every one of them has to survive a restart,
// because the whole reason the overlay exists is that they did not: the API applied them to the running
// configuration and wrote nothing down, so the display sink — which is only read at startup — was discarded
// by the very restart needed to pick it up.

func TestWithOverridesLeavesTheConfigAloneWhenThereIsNothingStored(t *testing.T) {
	base := Default()
	base.Display.Sink = "file"

	got, err := base.WithOverrides(nil)

	require.NoError(t, err)
	require.Equal(t, "file", got.Display.Sink)
	require.Equal(t, base, got, "an empty overlay must be the identity")
}

// TestWithOverridesAppliesTheSink is the case the operator actually hit: camera-only chosen in the UI,
// then lost on restart.
func TestWithOverridesAppliesTheSink(t *testing.T) {
	base := Default()
	base.Display.Sink = "file"

	got, err := base.WithOverrides(map[string]string{"sink": "none"})

	require.NoError(t, err)
	require.Equal(t, "none", got.Display.Sink)
}

func TestWithOverridesAppliesEveryFieldTheSettingsAPICanChange(t *testing.T) {
	got, err := Default().WithOverrides(map[string]string{
		"fps":         "25",
		"brightness":  "0.5",
		"gamma":       "2.2",
		"window_size": "32",
		"grid_width":  "384",
		"grid_height": "384",
		"cell_pixels": "5",
		"quiet_zone":  "3",
		"encoder":     "color16",
		"bit_depth":   "4",
		"sink":        "none",
	})

	require.NoError(t, err)
	require.Equal(t, 25.0, got.Display.FPS)
	require.Equal(t, 0.5, got.Display.Brightness)
	require.Equal(t, 2.2, got.Display.Gamma)
	require.Equal(t, 32, got.Display.WindowSize)
	require.Equal(t, 384, got.Optical.GridWidth)
	require.Equal(t, 384, got.Optical.GridHeight)
	require.Equal(t, 5, got.Optical.CellPixels)
	require.Equal(t, 3, got.Optical.QuietZone)
	require.Equal(t, "color16", got.Optical.Encoder)
	require.Equal(t, 4, got.Optical.BitDepth)
	require.Equal(t, "none", got.Display.Sink)
}

// TestWithOverridesOnlyTouchesStoredKeys is what makes the storage sparse rather than a snapshot: a stored
// sink must not drag a stale frame rate along with it, so a field nobody set still follows the file and the
// environment.
func TestWithOverridesOnlyTouchesStoredKeys(t *testing.T) {
	base := Default()
	base.Display.FPS = 10
	base.Optical.GridWidth = 128

	got, err := base.WithOverrides(map[string]string{"sink": "none"})

	require.NoError(t, err)
	require.Equal(t, 10.0, got.Display.FPS, "an unstored field keeps the configured value")
	require.Equal(t, 128, got.Optical.GridWidth)
}

// TestWithOverridesReportsAnUnparseableValue: reported, not skipped. A stored value that cannot be read is a
// real problem an operator needs told about, and silently ignoring it would present the configured value while
// the database disagreed.
func TestWithOverridesReportsAnUnparseableValue(t *testing.T) {
	_, err := Default().WithOverrides(map[string]string{"fps": "as fast as possible"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "fps")
}

func TestWithOverridesReportsAnUnparseableInteger(t *testing.T) {
	_, err := Default().WithOverrides(map[string]string{"grid_width": "wide"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "grid_width")
}

// TestWithOverridesRejectsAnUnknownKey guards against a typo in the store turning into a setting that looks
// applied and is not. The settings API writes these keys itself, so an unknown one means something is wrong.
func TestWithOverridesRejectsAnUnknownKey(t *testing.T) {
	_, err := Default().WithOverrides(map[string]string{"sync": "none"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "sync")
}

// TestWithOverridesDoesNotMutateTheReceiver: callers keep the un-overlaid configuration to fall back on when
// the overlay fails to validate, which only works if the overlay leaves the original alone.
func TestWithOverridesDoesNotMutateTheReceiver(t *testing.T) {
	base := Default()
	base.Display.Sink = "file"

	_, err := base.WithOverrides(map[string]string{"sink": "none"})

	require.NoError(t, err)
	require.Equal(t, "file", base.Display.Sink)
}
