package camera

import (
	"context"
	"time"
)

// Watcher notices cameras appearing and disappearing, and configures the receiver for them.
//
// It exists because a camera is not a fixed part of a machine. A laptop's built-in camera is there at boot,
// but the one an operator actually wants is often plugged in afterwards — and a receiver that had settled on
// "no camera is attached" at startup would go on believing it for as long as it ran, however many were
// plugged in later. Requiring a restart to notice a USB device is the kind of thing that reads as broken.
//
// The rule it applies is deliberately narrow, because guessing wrongly here means capturing from the wrong
// camera. It configures a camera only when the receiver has none it can use:
//
//   - nothing configured and a device appears: take it, in its best mode
//   - the configured device disappears and another is present: take that instead, and say so loudly, because
//     capturing from a camera nobody chose is exactly the surprise this must not spring quietly
//
// What it never does is override a working choice. An operator who selected the second camera keeps it when a
// third appears, because their decision is better evidence than the enumeration order.
type Watcher struct {
	// Interval is how often the devices are re-enumerated. Enumeration opens each device briefly, so this is
	// seconds rather than milliseconds — a camera plugged in is not an event anyone needs answered instantly.
	Interval time.Duration

	// List enumerates devices. Injected so a test can present a camera arriving without one arriving.
	List func() ([]Device, error)

	// Current is the selection in force, and Apply installs a new one. Both are injected because the watcher
	// has no business knowing about configuration watchers, databases, or the file the choice is saved to.
	Current func() Selection
	Apply   func(Selection, string) error
}

// Run watches until the context ends.
func (w Watcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Checked once immediately, so a camera already attached at startup is configured without waiting out the
	// first interval.
	w.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.check(ctx)
		}
	}
}

// check looks once, and configures a camera if the receiver has none it can use.
func (w Watcher) check(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	devices, err := w.List()
	if err != nil || len(devices) == 0 {
		// Nothing attached, or the platform cannot say. Neither is an event: a receiver reading frames from a
		// directory is in this state permanently and correctly.
		return
	}

	current := w.Current()

	// Case one: nothing has been chosen. Take the default camera in its best mode.
	if current.Device == "" {
		if best, ok := Preferred(devices); ok {
			_ = w.Apply(best, "a camera was detected and configured automatically")
		}
		return
	}

	// Case two: the chosen camera is still here. Leave it alone — including when others appear, because an
	// operator's choice outranks the enumeration order.
	for _, device := range devices {
		if device.Path == current.Device {
			return
		}
	}

	// Case three: the chosen camera has gone and another is present. Substituting is better than capturing
	// nothing, and saying so is not optional: an operator whose camera was swapped underneath them needs to
	// find out from the receiver rather than from the images.
	if best, ok := Preferred(devices); ok {
		_ = w.Apply(best, "the configured camera "+current.Device+" is no longer attached")
	}
}
