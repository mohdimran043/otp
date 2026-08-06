//go:build linux

package pipeline

import (
	"github.com/opticaltransport/otp/receiver/internal/camera"
	"github.com/opticaltransport/otp/receiver/internal/config"
)

// cameraAvailable is true because Video4Linux is compiled in — no cgo, no OpenCV, just the kernel's ioctls.
const cameraAvailable = true

// openCameraSource opens the configured camera.
func openCameraSource(cfg config.Capture) (Source, error) {
	return OpenCamera(camera.Selection{
		Device: cfg.Device,
		Format: cfg.Format,
		Width:  cfg.Width,
		Height: cfg.Height,
		FPS:    cfg.FPS,
	})
}
