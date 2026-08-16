package scheduler

import (
	"time"

	"github.com/opticaltransport/otp/shared/readable"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// assumedCaptureWidth and assumedCaptureHeight are the picture the receiving camera is taken to
// produce, for the purpose of choosing a frame rate.
//
// The sender cannot ask. The two applications share a protocol and a directory and nothing else, and
// that separation is deliberate — so this is an assumption, and it is the receiver's own pinned
// capture size rather than a guess: receiver/web requests 1920x1080 for every camera, on purpose,
// because past that the sensor starts resolving the panel's pixel grid instead of the frame on it.
//
// Being wrong here is mild and in the safe direction. A camera capturing at more than this resolves
// more pixels a cell than assumed, so it needs fewer photographs a frame than this asks for, and the
// transfer is slower than it had to be rather than unreadable. An explicit rate overrides it.
const (
	assumedCaptureWidth  = 1920
	assumedCaptureHeight = 1080
)

// displayInterval is how long one frame of this transmission should be shown for.
//
// An explicitly configured rate wins. Two reasons, and the second is the one that matters: an
// operator turning the rate down while watching a receiver fall behind is the main lever they have,
// and a derived rate that overrode them would take it away. The derived rate is what to do when
// nobody has expressed a preference — which is the usual case, and the case that was quietly wrong
// when the default was ten a second regardless of what was being sent.
func displayInterval(cfg config.Config, tx store.Transmission) time.Duration {
	if cfg.Display.FPSExplicit {
		return cfg.FrameInterval()
	}

	// Zero geometry means a transmission recorded before the grid was stored, or a test fixture.
	// Fall back rather than dividing by it.
	if tx.GridWidth <= 0 {
		return cfg.FrameInterval()
	}

	depth := uint8(tx.BitDepth)
	if depth == 0 {
		depth = 1
	}
	// The lane count matters as much as the grid. Four lanes span twice the cells across the display,
	// so every cell resolves to half the camera pixels and each frame needs more photographs — which
	// means a slower rate, not the same one.
	lanes := cfg.Optical.Lanes
	if lanes < 1 {
		lanes = 1
	}
	fps := readable.DisplayFPSForLanes(
		tx.GridWidth, tx.QuietZone, depth, assumedCaptureWidth, assumedCaptureHeight, lanes)
	if fps <= 0 {
		return cfg.FrameInterval()
	}
	return time.Duration(float64(time.Second) / fps)
}
