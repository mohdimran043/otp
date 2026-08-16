package config

import (
	"fmt"
	"sort"
	"strconv"
)

// WithOverrides returns this configuration with stored settings laid over it.
//
// The settings API changes the running configuration; this is how those changes come back after a restart.
// Without it they did not come back at all — Apply mutates the in-memory watcher and nothing was written
// down, so the display sink, which is only ever read at startup, was thrown away by the restart it needed to
// take effect. The channel toggle could therefore never do anything.
//
// The overlay is sparse on purpose, and that is the whole design rather than an optimisation. Only the keys
// actually present are applied, so a stored sink does not drag a stale frame rate along with it, and every
// field the operator has never touched keeps following the file and the environment. It is the same shape the
// settings API already has, where each field is a pointer and a PATCH states exactly what it means to change.
//
// Stored values win over the file and the environment for these fields. That is the point: "I changed it in
// the UI" has to outlive a restart. The accepted cost is that editing such a field in .env no longer wins
// once it has been set through the UI — changing it back in the UI overwrites the stored value.
//
// An unreadable value is an error rather than something to skip. A silently ignored one would leave the page
// showing the configured value while the database held something else, which is precisely the class of
// confusion this exists to remove. Callers keep the un-overlaid configuration to fall back on, so this does
// not modify the receiver.
func (c Config) WithOverrides(stored map[string]string) (Config, error) {
	if len(stored) == 0 {
		return c, nil
	}

	// Addressed on the copy, so a failure part-way through cannot leave the caller's configuration half
	// overlaid. c is already a value, but Optical and Display are reached through it.
	next := c

	// A stored rate is one an operator chose through the settings page, so it counts as explicit and
	// the scheduler must not recompute it from geometry. See Display.FPSExplicit.
	if _, ok := stored["fps"]; ok {
		next.Display.FPSExplicit = true
	}

	floats := map[string]*float64{
		"fps":        &next.Display.FPS,
		"brightness": &next.Display.Brightness,
		"gamma":      &next.Display.Gamma,
	}
	integers := map[string]*int{
		"window_size": &next.Display.WindowSize,
		"lanes":       &next.Optical.Lanes,
		"grid_width":  &next.Optical.GridWidth,
		"grid_height": &next.Optical.GridHeight,
		"cell_pixels": &next.Optical.CellPixels,
		"quiet_zone":  &next.Optical.QuietZone,
		"bit_depth":   &next.Optical.BitDepth,
	}
	strings := map[string]*string{
		"encoder": &next.Optical.Encoder,
		"sink":    &next.Display.Sink,
	}

	// Sorted so a configuration with two bad values always reports the same one, rather than whichever the
	// map happened to yield first. A test that fails intermittently on the error text is worse than useless.
	keys := make([]string, 0, len(stored))
	for key := range stored {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		raw := stored[key]
		switch {
		case floats[key] != nil:
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return c, fmt.Errorf("stored setting %s: %q is not a number", key, raw)
			}
			*floats[key] = value
		case integers[key] != nil:
			value, err := strconv.Atoi(raw)
			if err != nil {
				return c, fmt.Errorf("stored setting %s: %q is not a whole number", key, raw)
			}
			*integers[key] = value
		case strings[key] != nil:
			*strings[key] = raw
		default:
			// Not tolerated, because this package writes these keys itself: an unknown one means a typo or a
			// downgrade, and either way a setting the operator believes is applied is not.
			return c, fmt.Errorf("stored setting %s is not a setting this build understands", key)
		}
	}

	return next, nil
}

// SettingKeys are the settings that can be stored, which is exactly what the settings API can change.
//
// Exported so the API can persist only what it recognises, and so a test can assert the two lists have not
// drifted apart — a field added to the request struct and forgotten here would apply and then vanish on the
// next restart, which is the original bug returning by a different route.
func SettingKeys() []string {
	return []string{
		"fps", "brightness", "gamma", "window_size", "lanes",
		"grid_width", "grid_height", "cell_pixels", "quiet_zone", "encoder", "bit_depth",
		"sink",
	}
}
