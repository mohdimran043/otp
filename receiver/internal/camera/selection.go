package camera

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Selection is a camera and the mode to run it in.
type Selection struct {
	// Device is the path to open. Empty means the default camera.
	Device string `json:"device"`

	// Format is the FourCC to request, such as MJPG. Empty means the driver's preference.
	//
	// It is worth setting deliberately. A camera that offers 1920×1080 at 30 fps in MJPG often offers
	// the same size at 5 fps in uncompressed YUYV, because the USB bus cannot carry raw frames that
	// fast — so a mode chosen without regard to format can silently cost six times the frame rate.
	Format string `json:"format,omitempty"`

	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`

	// FPS is the frame rate to request. The camera is free to give less.
	FPS float64 `json:"fps,omitempty"`

	// Source is where frames come from: "file" to read a directory, "gocv" to open a camera.
	//
	// It belongs here rather than only in the configuration file because it is the first choice an operator
	// makes and the one that decides whether any of the rest matters — a camera selected while the source is
	// "file" is recorded and never opened. Empty means leave whatever is configured alone, so that saving a
	// camera choice does not silently change the source as a side effect.
	Source string `json:"source,omitempty"`
}

// CheckSource refuses a source that is not in the given list.
//
// The list is passed in rather than declared here, and that is the whole point: what the protocol knows about
// and what a particular binary can open are different sets. The camera source needs OpenCV and lives behind a
// build tag, so a build without it must refuse "gocv" rather than accept it and fail later. Accepting it was
// how a settings page came to be able to stop the receiver from starting — the choice was persisted, and the
// next start could not honour it.
func CheckSource(source string, available []string) error {
	if source == "" {
		return nil
	}
	for _, known := range available {
		if source == known {
			return nil
		}
	}
	return fmt.Errorf("camera: %q is not a capture source this build can open; it offers %s",
		source, strings.Join(available, ", "))
}

// Zero reports whether nothing has been chosen.
func (s Selection) Zero() bool {
	return s.Device == "" && s.Format == "" && s.Width == 0 && s.Height == 0 && s.FPS == 0 &&
		s.Source == ""
}

// String renders a selection for a log line.
func (s Selection) String() string {
	device := s.Device
	if device == "" {
		device = "default camera"
	}
	if s.Width == 0 || s.Height == 0 {
		return device
	}
	mode := fmt.Sprintf("%s at %d×%d", device, s.Width, s.Height)
	if s.FPS > 0 {
		mode += fmt.Sprintf(", %g fps", s.FPS)
	}
	if s.Format != "" {
		mode += " " + s.Format
	}
	return mode
}

// Validate checks a selection against what is actually attached.
//
// Against the hardware rather than against a range, because the failure this prevents is specific: a
// resolution the camera does not offer is not clamped by the driver, it is substituted — and a receiver
// that asked for 1920×1080 and is quietly given 640×480 will fail to resolve the cell grid and report
// it as an optical problem. Refusing the impossible mode up front turns that into a sentence an operator
// can act on.
func (s Selection) Validate(devices []Device) error {
	if s.Zero() {
		return nil
	}

	// A source chosen on its own is a complete selection: switching to "file" is a decision that needs no
	// camera at all, and requiring one would make it impossible to switch back.
	if s.Device == "" && s.Format == "" && s.Width == 0 && s.Height == 0 && s.FPS == 0 {
		return nil
	}
	if s.FPS < 0 {
		return errors.New("camera: the frame rate cannot be negative")
	}
	if (s.Width == 0) != (s.Height == 0) {
		return errors.New("camera: a width needs a height, and a height needs a width")
	}

	// Nothing to check against.
	//
	// This is the case that matters in development and in a container that has not been given a camera yet:
	// enumeration finds nothing, so every choice would be refused and the operator could configure nothing
	// at all. Refusing a mode the camera says it cannot do is only defensible when the camera can be asked.
	// When it cannot, the operator's word is the better evidence — they can see the device on the host, or
	// they are configuring for a passthrough that arrives at the next restart.
	//
	// The path is still checked for being a plausible device rather than accepted blindly, because a typo
	// that silently becomes the configuration is worse than a refusal.
	if len(devices) == 0 {
		if s.Device == "" {
			return errors.New("camera: no capture device is attached, so there is nothing to select")
		}
		return checkDevicePath(s.Device)
	}

	// An exact match is required here, deliberately unlike Selected. Falling back to another camera is
	// the right behaviour at startup — a camera unplugged overnight should not stop the receiver — and the
	// wrong behaviour for an operator choosing one now, because they are looking at a list and the answer
	// to "that device is not there" is to say so rather than to pick a different one on their behalf.
	device, ok := find(devices, s.Device)
	if !ok {
		if s.Device == "" {
			return errors.New("camera: no capture device is attached")
		}
		// Named a device that is not in the list. Accepted only if it is plausible and the list might be
		// incomplete for a reason the receiver cannot see — otherwise refused, because an operator looking
		// at a list deserves to be told their choice is not on it.
		return fmt.Errorf("camera: %s is not one of the %d attached capture devices", s.Device, len(devices))
	}

	if s.Width == 0 && s.Format == "" && s.FPS == 0 {
		return nil // A device with no mode: whatever the driver prefers.
	}

	var sizeSeen, formatSeen bool
	for _, mode := range device.Modes {
		if s.Format != "" && !strings.EqualFold(mode.Format, s.Format) {
			continue
		}
		formatSeen = true
		if s.Width != 0 && (mode.Width != s.Width || mode.Height != s.Height) {
			continue
		}
		sizeSeen = true
		if s.FPS == 0 {
			return nil
		}
		for _, fps := range mode.FPS {
			// Frame rates are reported as exact rationals, so 29.97 arrives as 30000/1001. Compare with
			// a tolerance rather than for equality, or a legitimate choice from this very list is refused.
			if fps >= s.FPS-0.01 && fps <= s.FPS+0.01 {
				return nil
			}
		}
	}

	switch {
	case s.Format != "" && !formatSeen:
		return fmt.Errorf("camera: %s does not offer the %s format", device.Path, s.Format)
	case s.Width != 0 && !sizeSeen:
		return fmt.Errorf("camera: %s does not offer %d×%d", device.Path, s.Width, s.Height)
	default:
		return fmt.Errorf("camera: %s does not offer %g fps at %d×%d", device.Path, s.FPS, s.Width, s.Height)
	}
}

// checkDevicePath accepts what a capture device is plausibly called, and refuses the rest.
//
// Deliberately loose: a Video4Linux node, or a bare index as OpenCV numbers its cameras. The point is to
// catch a typo — a path with a space in it, an empty string, something that is obviously a filename — not to
// predict every way a platform might name a device.
func checkDevicePath(path string) error {
	if strings.TrimSpace(path) != path || path == "" {
		return errors.New("camera: the device path has stray whitespace")
	}
	if index, err := strconv.Atoi(path); err == nil {
		if index < 0 || index > 63 {
			return fmt.Errorf("camera: %q is not a plausible camera index", path)
		}
		return nil
	}
	if !strings.HasPrefix(path, "/dev/") {
		return fmt.Errorf("camera: %q is neither a device path under /dev nor a camera index", path)
	}
	if strings.ContainsAny(path, " \t\n") {
		return fmt.Errorf("camera: %q contains whitespace", path)
	}
	return nil
}

// find returns the named device exactly, or the default when no name was given.
func find(devices []Device, path string) (Device, bool) {
	if path == "" {
		device, _, ok := Selected(devices, "")
		return device, ok
	}
	for _, device := range devices {
		if device.Path == path {
			return device, true
		}
	}
	return Device{}, false
}

// Preferred returns the selection to use when nobody has chosen one: the default camera in its best mode.
//
// The best mode rather than a safe one, because that is what this platform is for. Resolution is bytes
// per frame and frame rate is frames per second, and their product is the transfer rate — so defaulting
// to something conservative would mean shipping a system that runs at a fraction of the speed the
// hardware in front of it can manage, which an operator would have no reason to suspect.
func Preferred(devices []Device) (Selection, bool) {
	return PreferredFor(devices, "")
}

// PreferredFor returns the best mode for one named device.
//
// It is what makes selecting a camera a single act rather than two. An operator picking a device from a list
// has expressed the whole of their intent — "use that camera" — and asking them to then choose between
// eighteen modes is asking them to answer a question the receiver can answer better: the largest frame, and
// among equal frames the fastest. Anyone who wants a different mode can still say so, but nobody should have
// to in order to get a working camera.
//
// An empty path means the default camera, so this is also the rule applied at startup when nothing has been
// configured at all.
func PreferredFor(devices []Device, path string) (Selection, bool) {
	device, ok := find(devices, path)
	if !ok {
		return Selection{}, false
	}
	mode, ok := Best(device.Modes)
	if !ok {
		// A device that reports no modes is still usable: the driver has a preference of its own, and naming
		// the device without a mode is how to ask for it.
		return Selection{Device: device.Path}, true
	}
	selection := Selection{
		Device: device.Path,
		Format: mode.Format,
		Width:  mode.Width,
		Height: mode.Height,
	}
	if len(mode.FPS) > 0 {
		selection.FPS = mode.FPS[0]
	}
	return selection, true
}

// selectionFile is where an operator's choice is kept.
const selectionFile = "camera.json"

// LoadSelection reads the choice an operator made through the UI, from dir.
//
// It is a file of its own rather than a rewrite of the configuration file, and that is deliberate. The
// configuration file is the operator's document — it has their comments and their formatting in it — and
// a service that rewrote it to record a click would be destroying something it does not own. Precedence
// is decided at startup: explicit configuration wins over this file, and this file wins over the default.
func LoadSelection(dir string) (Selection, error) {
	body, err := os.ReadFile(filepath.Join(dir, selectionFile))
	if errors.Is(err, os.ErrNotExist) {
		return Selection{}, nil
	}
	if err != nil {
		return Selection{}, fmt.Errorf("camera: %w", err)
	}
	var selection Selection
	if err := json.Unmarshal(body, &selection); err != nil {
		return Selection{}, fmt.Errorf("camera: %s is not readable: %w", selectionFile, err)
	}
	return selection, nil
}

// SaveSelection records a choice, so it survives a restart.
//
// Written to a temporary name and renamed, because a torn write here would be read at the next start as a
// corrupt selection — and the receiver would refuse to capture over a file that only records a preference.
func SaveSelection(dir string, selection Selection) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("camera: %w", err)
	}
	body, err := json.MarshalIndent(selection, "", "  ")
	if err != nil {
		return fmt.Errorf("camera: %w", err)
	}
	final := filepath.Join(dir, selectionFile)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o640); err != nil {
		return fmt.Errorf("camera: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("camera: %w", err)
	}
	return nil
}
