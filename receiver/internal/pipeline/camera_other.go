//go:build !linux

package pipeline

import (
	"errors"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// Video4Linux is Linux's interface, so a camera cannot be opened elsewhere. Reported plainly rather than
// offered and failed: what a binary can open should be decided at compile time and stated honestly.
const cameraAvailable = false

func openCameraSource(config.Capture) (Source, error) {
	return nil, errors.New("pipeline: this platform cannot capture from a camera")
}
