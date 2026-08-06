//go:build linux

package camera

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This is Video4Linux2 queried directly, through the four ioctls that answer "what is attached and what
// can it do": QUERYCAP, ENUM_FMT, ENUM_FRAMESIZES, ENUM_FRAMEINTERVALS.
//
// Directly rather than through a library because the alternative is worse in both directions. Reading
// /sys/class/video4linux/*/name is easy and wrong — it cannot tell a capture device from the metadata
// node the same camera also registers, and offering an operator a device that produces no images is a
// fault that presents as "the camera does not work". Linking OpenCV to ask the same question would pull
// a hundred megabytes of dependency into a service that does not otherwise need it. The ioctl structs
// are stable kernel ABI; that is what makes reading them directly safe.

const (
	// The request numbers are _IOR/_IOWR over the structs below. They are written out rather than
	// computed so that a struct whose size drifts fails loudly at the ioctl rather than quietly
	// misreading memory.
	vidiocQueryCap          = 0x80685600 // _IOR('V',  0, struct v4l2_capability)  — 104 bytes
	vidiocEnumFmt           = 0xC0405602 // _IOWR('V',  2, struct v4l2_fmtdesc)     —  64 bytes
	vidiocEnumFrameSizes    = 0xC02C564A // _IOWR('V', 74, struct v4l2_frmsizeenum) —  44 bytes
	vidiocEnumFrameInterval = 0xC034564B // _IOWR('V', 75, struct v4l2_frmivalenum) —  52 bytes

	capVideoCapture       = 0x00000001
	capVideoCaptureMPlane = 0x00001000
	capDeviceCaps         = 0x80000000

	bufTypeVideoCapture = 1

	frmsizeDiscrete = 1
	frmivalDiscrete = 1
)

// v4l2Capability mirrors struct v4l2_capability. The trailing reserved words are part of the size the
// ioctl request encodes, so they are declared rather than omitted.
type v4l2Capability struct {
	Driver     [16]byte
	Card       [32]byte
	BusInfo    [32]byte
	Version    uint32
	Caps       uint32
	DeviceCaps uint32
	Reserved   [3]uint32
}

type v4l2FmtDesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]byte
	PixelFormat uint32
	MbusCode    uint32
	Reserved    [3]uint32
}

type v4l2FrmSizeEnum struct {
	Index       uint32
	PixelFormat uint32
	Type        uint32
	// The union is six words wide, its largest member being the stepwise form. Discrete uses the first
	// two: width then height.
	Union    [6]uint32
	Reserved [2]uint32
}

type v4l2FrmIvalEnum struct {
	Index       uint32
	PixelFormat uint32
	Width       uint32
	Height      uint32
	Type        uint32
	// The union is six words; discrete uses the first two as a numerator and denominator.
	Union    [6]uint32
	Reserved [2]uint32
}

func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func cstring(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// fourcc renders a V4L2 pixel format as the four characters drivers and operators both use.
func fourcc(format uint32) string {
	out := []byte{byte(format), byte(format >> 8), byte(format >> 16), byte(format >> 24)}
	return strings.TrimRight(string(out), "\x00 ")
}

// List returns the capture devices attached to this machine, lowest device number first.
//
// A device that cannot be opened is skipped rather than reported: the usual reason is permissions, and on
// a machine with several cameras the ones that *are* readable are still worth offering. A device that
// opens but declares no capture capability is skipped too — that is the metadata node.
func List() ([]Device, error) {
	paths, err := filepath.Glob("/dev/video*")
	if err != nil {
		return nil, err
	}
	sort.Slice(paths, func(i, j int) bool { return deviceNumber(paths[i]) < deviceNumber(paths[j]) })

	var devices []Device
	for _, path := range paths {
		device, err := Describe(path)
		if err != nil {
			continue
		}
		devices = append(devices, device)
	}

	// The default is the first device that can capture. "First" is by device number, which is the order
	// the kernel enumerated them in, and is the closest thing to "the camera someone plugged in" that
	// exists without asking a human.
	if len(devices) > 0 {
		devices[0].Default = true
	}
	return devices, nil
}

// deviceNumber extracts the trailing integer from /dev/videoN, so that video10 sorts after video9 rather
// than after video1.
func deviceNumber(path string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "video"))
	if err != nil {
		return 1 << 30
	}
	return n
}

// ErrNotCapture means the device exists but does not produce video frames.
var ErrNotCapture = errors.New("camera: the device does not support video capture")

// Describe opens one device and reports what it is and what it can do.
func Describe(path string) (Device, error) {
	// Non-blocking, because opening a camera that another process holds can otherwise wait
	// indefinitely — and enumerating devices must not hang a settings page.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return Device{}, fmt.Errorf("camera: opening %s: %w", path, err)
	}
	defer unix.Close(fd)

	var cap v4l2Capability
	if err := ioctl(fd, vidiocQueryCap, unsafe.Pointer(&cap)); err != nil {
		return Device{}, fmt.Errorf("camera: querying %s: %w", path, err)
	}

	// device_caps describes this node; capabilities describes the whole physical device. Using the
	// former when it is present is the only way to tell a camera's capture node from its metadata node,
	// because both report the same capabilities for the device as a whole.
	effective := cap.Caps
	if cap.Caps&capDeviceCaps != 0 {
		effective = cap.DeviceCaps
	}
	if effective&(capVideoCapture|capVideoCaptureMPlane) == 0 {
		return Device{}, fmt.Errorf("%w: %s", ErrNotCapture, path)
	}

	device := Device{
		Path:    path,
		Name:    cstring(cap.Card[:]),
		Driver:  cstring(cap.Driver[:]),
		BusInfo: cstring(cap.BusInfo[:]),
		Modes:   modes(fd),
	}
	if device.Name == "" {
		device.Name = filepath.Base(path)
	}
	return device, nil
}

// modes enumerates every format, size, and frame interval the device offers, best first.
func modes(fd int) []Mode {
	var out []Mode
	for index := uint32(0); index < 64; index++ {
		desc := v4l2FmtDesc{Index: index, Type: bufTypeVideoCapture}
		if err := ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&desc)); err != nil {
			break // EINVAL is how V4L2 says "that was the last one".
		}
		format := fourcc(desc.PixelFormat)
		name := cstring(desc.Description[:])

		for sizeIndex := uint32(0); sizeIndex < 256; sizeIndex++ {
			size := v4l2FrmSizeEnum{Index: sizeIndex, PixelFormat: desc.PixelFormat}
			if err := ioctl(fd, vidiocEnumFrameSizes, unsafe.Pointer(&size)); err != nil {
				break
			}
			if size.Type != frmsizeDiscrete {
				// A stepwise or continuous range: report its maximum, which is the only point in the
				// range an operator would choose here. Width and height maxima are the third and
				// fourth words of the stepwise struct.
				out = append(out, Mode{
					Format:     format,
					FormatName: name,
					Width:      int(size.Union[1]),
					Height:     int(size.Union[3]),
					FPS:        intervals(fd, desc.PixelFormat, size.Union[1], size.Union[3]),
				})
				break
			}
			width, height := size.Union[0], size.Union[1]
			out = append(out, Mode{
				Format:     format,
				FormatName: name,
				Width:      int(width),
				Height:     int(height),
				FPS:        intervals(fd, desc.PixelFormat, width, height),
			})
		}
	}

	// Largest frame first, fastest breaking the tie — the same ordering Best applies, so that a list
	// shown to an operator reads in the order of preference rather than in driver order.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if area, bArea := a.Width*a.Height, b.Width*b.Height; area != bArea {
			return area > bArea
		}
		return topFPS(a) > topFPS(b)
	})
	return out
}

// intervals returns the frame rates offered at one size, highest first.
func intervals(fd int, format, width, height uint32) []float64 {
	var out []float64
	for index := uint32(0); index < 64; index++ {
		ival := v4l2FrmIvalEnum{Index: index, PixelFormat: format, Width: width, Height: height}
		if err := ioctl(fd, vidiocEnumFrameInterval, unsafe.Pointer(&ival)); err != nil {
			break
		}
		if ival.Type != frmivalDiscrete {
			// Stepwise: the fastest rate is the shortest interval, which is the minimum of the range —
			// the first two words.
			if numerator, denominator := ival.Union[0], ival.Union[1]; numerator > 0 {
				out = append(out, float64(denominator)/float64(numerator))
			}
			break
		}
		// A frame *interval* is seconds per frame, so the rate is its reciprocal. Getting this the wrong
		// way round yields 0.033 fps rather than 30, which is why it is spelled out.
		numerator, denominator := ival.Union[0], ival.Union[1]
		if numerator == 0 {
			continue
		}
		out = append(out, float64(denominator)/float64(numerator))
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(out)))
	return out
}

// Available reports whether this platform can enumerate cameras at all, which is what lets the API say
// "this build cannot see your cameras" rather than "you have none".
func Available() bool { return true }
