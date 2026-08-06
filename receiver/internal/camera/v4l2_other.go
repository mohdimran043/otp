//go:build !linux

package camera

// Video4Linux is Linux's interface, so elsewhere there is nothing to enumerate.
//
// Reporting that plainly rather than returning an error matters for what the settings page can say: "this
// platform cannot list cameras" is a different message from "no cameras are attached", and an operator
// developing on a laptop deserves the first one.

// List returns no devices.
func List() ([]Device, error) { return nil, nil }

// Describe cannot open a device on this platform.
func Describe(path string) (Device, error) {
	return Device{}, ErrUnsupported
}

// Available reports that this platform cannot enumerate cameras.
func Available() bool { return false }
