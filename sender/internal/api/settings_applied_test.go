package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every settable field must actually reach the running configuration.
//
// There is already a test asserting that the request struct and SettingKeys have not drifted apart, and
// it passed while the lane count was broken — because a setting travels through three places, not two.
// It has to be a field on the request, named in SettingKeys so it is persisted, *and* copied onto the
// configuration in updateSettings. The lane count was in the first two and missing from the third, so
// the API accepted the change, wrote it to the database, reported success, and displayed the old value
// forever. Nothing in the request/keys comparison can see that gap: both lists agreed.
//
// So this asserts the third leg directly. Each case changes one setting to a value the default is not,
// and reads the configuration back. A field added to the request and forgotten in the apply block fails
// here rather than in front of an operator clicking a control that does nothing.

func TestEverySettableFieldReachesTheRunningConfig(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		// got reads the field back out of the configuration, so the assertion is about the value the
		// scheduler and the display will actually use rather than about the response body.
		got func(h *settingsHarness) any
		// want is what the field should hold afterwards, and is deliberately not the default.
		want any
	}{
		{"fps", `{"fps":3}`, func(h *settingsHarness) any { return h.watcher.Current().Display.FPS }, 3.0},
		{"brightness", `{"brightness":0.75}`, func(h *settingsHarness) any { return h.watcher.Current().Display.Brightness }, 0.75},
		{"gamma", `{"gamma":1.8}`, func(h *settingsHarness) any { return h.watcher.Current().Display.Gamma }, 1.8},
		{"window_size", `{"window_size":7}`, func(h *settingsHarness) any { return h.watcher.Current().Display.WindowSize }, 7},
		{"lanes", `{"lanes":2}`, func(h *settingsHarness) any { return h.watcher.Current().Optical.Lanes }, 2},
		{"grid_width", `{"grid_width":64}`, func(h *settingsHarness) any { return h.watcher.Current().Optical.GridWidth }, 64},
		{"grid_height", `{"grid_height":64}`, func(h *settingsHarness) any { return h.watcher.Current().Optical.GridHeight }, 64},
		{"cell_pixels", `{"cell_pixels":6}`, func(h *settingsHarness) any { return h.watcher.Current().Optical.CellPixels }, 6},
		{"quiet_zone", `{"quiet_zone":3}`, func(h *settingsHarness) any { return h.watcher.Current().Optical.QuietZone }, 3},
		{"encoder", `{"encoder":"binary"}`, func(h *settingsHarness) any { return h.watcher.Current().Optical.Encoder }, "binary"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newSettingsHarness(t)

			require.NotEqual(t, c.want, c.got(h),
				"the test value must differ from the default, or an unapplied setting would still pass")

			response := h.patch(t, c.body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())

			require.Equal(t, c.want, c.got(h),
				"%s was accepted by the API but never copied onto the configuration", c.name)
		})
	}
}

// TestChangingLanesIsAllowedMidTransfer, which is the reason the lane count is not treated as geometry.
//
// The grid and the sink are frozen while frames are in flight because the receiver would be looking at
// the wrong thing. The lane count is not like that: each lane is a whole independent frame with its own
// fiducials and header, so a display that goes from four tiles to two keeps emitting frames the receiver
// reads exactly as before — it simply finds fewer of them per photograph. Refusing the change mid-flight
// would take away the one adjustment an operator can make when a camera cannot resolve four tiles, at
// precisely the moment they discover it.
func TestChangingLanesIsAllowedMidTransfer(t *testing.T) {
	h := newSettingsHarness(t)
	h.seedTransmitting(t)

	response := h.patch(t, `{"lanes":1}`)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, 1, h.watcher.Current().Optical.Lanes)
}

// TestRejectingALaneCountThatCannotBeTiled, at the click rather than at the next frame.
func TestRejectingALaneCountThatCannotBeTiled(t *testing.T) {
	h := newSettingsHarness(t)
	before := h.watcher.Current().Optical.Lanes

	response := h.patch(t, `{"lanes":3}`)

	require.Equal(t, http.StatusUnprocessableEntity, response.Code)
	require.Equal(t, before, h.watcher.Current().Optical.Lanes, "a rejected change must not half-apply")
}
