// Package camera finds the capture devices attached to this machine and what they can do.
//
// It exists so that configuring a camera is a choice from a list rather than a guess at a device path.
// An operator standing in front of a rack should be able to see "HD Pro Webcam C920 at /dev/video0,
// 1920×1080 at 30 fps" and pick it, and — more to the point — should not have to discover by trial that
// /dev/video1 on the same camera is a metadata node that produces no images at all.
//
// The modes matter as much as the device. Everything about this platform's throughput is set by how many
// cells a camera can resolve and how many frames a second it can deliver, so the list of resolutions and
// frame intervals is not a detail: it is the ceiling the whole transfer runs into.
package camera

import (
	"errors"
	"fmt"
)

// ErrUnsupported means this platform cannot enumerate capture devices.
var ErrUnsupported = errors.New("camera: enumerating capture devices is not supported on this platform")

// Device is one capture device.
type Device struct {
	// Path is what to open: /dev/video0 and so on.
	Path string `json:"path"`

	// Name is the card name the driver reports — what is written on the camera, usually.
	Name string `json:"name"`

	// Driver and BusInfo identify the kernel driver and where the device is attached. Both are worth
	// showing: two identical cameras have the same name and different bus addresses, and telling them
	// apart is otherwise impossible.
	Driver  string `json:"driver"`
	BusInfo string `json:"bus_info"`

	// Modes are the resolutions and frame rates this device offers, best first.
	Modes []Mode `json:"modes"`

	// Default marks the device this receiver would use with no configuration: the lowest-numbered
	// device that can actually capture video.
	Default bool `json:"default"`
}

// Mode is one resolution-and-rate a device supports.
type Mode struct {
	// Format is the FourCC the driver names, such as MJPG or YUYV.
	Format string `json:"format"`

	// FormatName is the driver's own description of it.
	FormatName string `json:"format_name"`

	Width  int `json:"width"`
	Height int `json:"height"`

	// FPS are the frame rates offered at this size, highest first.
	FPS []float64 `json:"fps"`
}

// Label renders a mode the way an operator would read it.
func (m Mode) Label() string {
	if len(m.FPS) == 0 {
		return fmt.Sprintf("%d×%d %s", m.Width, m.Height, m.Format)
	}
	return fmt.Sprintf("%d×%d at %g fps (%s)", m.Width, m.Height, m.FPS[0], m.Format)
}

// Best returns the mode to prefer: the largest frame, and among equal frames the fastest.
//
// Largest first rather than fastest first, and the order is a real decision. Resolution is the harder
// constraint of the two: a frame the camera cannot resolve does not decode at all, whereas a camera that
// is merely slow decodes every frame it manages to take and the sender simply waits — which the
// acknowledgement rule makes safe. So the mode that maximises cells wins, and speed breaks the tie.
func Best(modes []Mode) (Mode, bool) {
	var best Mode
	found := false
	for _, mode := range modes {
		if !found {
			best, found = mode, true
			continue
		}
		area, bestArea := mode.Width*mode.Height, best.Width*best.Height
		switch {
		case area > bestArea:
			best = mode
		case area == bestArea && topFPS(mode) > topFPS(best):
			best = mode
		}
	}
	return best, found
}

func topFPS(m Mode) float64 {
	if len(m.FPS) == 0 {
		return 0
	}
	return m.FPS[0]
}

// Selected picks the device a configured path refers to, falling back to the default.
//
// A configured device that is no longer present returns the default and says so, because that is what an
// operator needs to hear: a camera unplugged between deploys is a real event, and silently capturing from
// a different one would be worse than either failing or reporting the substitution.
func Selected(devices []Device, configured string) (device Device, substituted bool, ok bool) {
	if configured != "" {
		for _, d := range devices {
			if d.Path == configured {
				return d, false, true
			}
		}
	}
	for _, d := range devices {
		if d.Default {
			return d, configured != "", true
		}
	}
	if len(devices) > 0 {
		return devices[0], configured != "", true
	}
	return Device{}, false, false
}
