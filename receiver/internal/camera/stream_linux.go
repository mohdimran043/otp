//go:build linux

package camera

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Streaming capture from a camera, through Video4Linux's memory-mapped buffer interface.
//
// Directly rather than through OpenCV, for the same reason the decoder is written directly: the alternative is
// a hundred megabytes of dependency and a cgo build for something this file does in a few hundred lines. The
// sequence is the standard one — set the format, ask for buffers, map them, queue them, stream on, then
// dequeue a filled buffer, use it, and queue it back.
//
// The buffers are mapped rather than copied by the kernel on every frame, which is the whole point of the
// interface: at 1920×1080 and thirty frames a second a copy per frame is sixty megabytes a second of pure
// waste. What this does copy is the one frame it is handing on, because the buffer must go back to the kernel
// immediately and whatever is holding the image cannot be looking at memory the driver is refilling.

const (
	// struct v4l2_format is 208 bytes, not 204: its union holds a struct with a pointer in it, so the union is
	// eight-byte aligned and the leading type field is followed by four bytes of padding. Getting that wrong
	// costs an ENOTTY — "inappropriate ioctl for device" — which reads like a driver problem and is arithmetic.
	vidiocSFmt      = 0xC0D05605 // _IOWR('V',  5, struct v4l2_format)      — 208 bytes
	vidiocReqBufs   = 0xC0145608 // _IOWR('V',  8, struct v4l2_requestbuffers) — 20 bytes
	vidiocQueryBuf  = 0xC0585609 // _IOWR('V',  9, struct v4l2_buffer)      —  88 bytes
	vidiocQBuf      = 0xC058560F // _IOWR('V', 15, struct v4l2_buffer)
	vidiocDQBuf     = 0xC0585611 // _IOWR('V', 17, struct v4l2_buffer)
	vidiocStreamOn  = 0x40045612 // _IOW('V', 18, int)
	vidiocStreamOff = 0x40045613 // _IOW('V', 19, int)
	vidiocSParm     = 0xC0CC5616 // _IOWR('V', 22, struct v4l2_streamparm)  — 204 bytes

	memoryMMAP = 1

	// fourccMJPG and fourccYUYV are the two formats worth supporting. MJPEG because it is what a webcam
	// offers at full rate — the bus cannot carry raw frames faster — and the standard library decodes it.
	// YUYV because it is the universal fallback, and cheap to convert.
	fourccMJPG = 0x47504A4D // 'M','J','P','G'
	fourccYUYV = 0x56595559 // 'Y','U','Y','V'

	// bufferCount is how many frames the kernel may have in flight. Four is the usual choice: enough that the
	// driver is never waiting for a buffer to be returned, few enough that a slow reader is looking at a
	// recent frame rather than a queue of stale ones.
	bufferCount = 4
)

// v4l2Format mirrors struct v4l2_format. The pixel-format union is written out as a byte array of the right
// size, with the fields this needs poked into their offsets — which is what the kernel does on its side too.
type v4l2Format struct {
	Type uint32
	_    uint32 // padding to the union's 8-byte alignment
	Fmt  [200]byte
}

// pix is the v4l2_pix_format at the head of the union: width, height, pixelformat, field, bytesperline,
// sizeimage, colorspace...
type v4l2PixFormat struct {
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	Colorspace   uint32
	Priv         uint32
}

type v4l2RequestBuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint8
	Reserved     [3]uint8
}

type v4l2Buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	Timestamp unix.Timeval
	Timecode  [16]byte
	Sequence  uint32
	Memory    uint32
	Offset    uint32 // the union's first member for MMAP; the rest of the union is padding below
	_         uint32
	Length    uint32
	Reserved2 uint32
	RequestFD int32
	_         uint32
}

type v4l2StreamParm struct {
	Type uint32
	_    uint32
	Parm [200]byte
}

// v4l2CaptureParm is the head of the streamparm union for a capture device.
type v4l2CaptureParm struct {
	Capability   uint32
	CaptureMode  uint32
	TimePerFrame struct{ Numerator, Denominator uint32 }
	ExtendedMode uint32
	ReadBuffers  uint32
}

// Stream is an open, streaming camera.
type Stream struct {
	fd     int
	path   string
	format uint32
	width  int
	height int

	buffers [][]byte

	mu     sync.Mutex
	closed bool
}

// Open starts streaming from a camera in the mode a selection asks for.
//
// The mode is requested and then read back, because a driver handed a format it cannot provide does not fail —
// it substitutes one and reports what it actually set. Reading it back is the only way to know what is really
// arriving, and a receiver that believed it was getting 1920×1080 while being fed 640×480 would fail to
// resolve the cell grid and report it as an optical fault.
func Open(selection Selection) (*Stream, error) {
	path := selection.Device
	if path == "" {
		return nil, errors.New("camera: no device to open")
	}

	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("camera: opening %s: %w", path, err)
	}

	s := &Stream{fd: fd, path: path}
	if err := s.configure(selection); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := s.mapBuffers(); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := ioctl(fd, vidiocStreamOn, unsafe.Pointer(&[]int32{bufTypeVideoCapture}[0])); err != nil {
		s.unmap()
		unix.Close(fd)
		return nil, fmt.Errorf("camera: starting the stream on %s: %w", path, err)
	}
	return s, nil
}

// configure sets the format and frame rate, and records what the driver actually agreed to.
func (s *Stream) configure(selection Selection) error {
	format := fourccMJPG
	if selection.Format == "YUYV" {
		format = fourccYUYV
	}

	width, height := selection.Width, selection.Height
	if width == 0 || height == 0 {
		width, height = 1280, 720
	}

	var f v4l2Format
	f.Type = bufTypeVideoCapture
	pix := v4l2PixFormat{
		Width:       uint32(width),
		Height:      uint32(height),
		PixelFormat: uint32(format),
		Field:       1, // V4L2_FIELD_NONE — progressive
	}
	*(*v4l2PixFormat)(unsafe.Pointer(&f.Fmt[0])) = pix

	if err := ioctl(s.fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
		return fmt.Errorf("camera: setting the format on %s: %w", s.path, err)
	}

	// Read back what was actually set. The driver substitutes rather than refuses.
	agreed := *(*v4l2PixFormat)(unsafe.Pointer(&f.Fmt[0]))
	s.format = agreed.PixelFormat
	s.width, s.height = int(agreed.Width), int(agreed.Height)
	if s.format != fourccMJPG && s.format != fourccYUYV {
		return fmt.Errorf("camera: %s gave format %s, which this build cannot decode",
			s.path, fourcc(s.format))
	}

	// The frame rate is requested as an interval, and a driver that cannot manage it says so by reporting a
	// different one. It is not an error: a camera slower than asked only makes the sender wait, which the
	// acknowledgement rule already makes safe.
	if selection.FPS > 0 {
		var parm v4l2StreamParm
		parm.Type = bufTypeVideoCapture
		capture := v4l2CaptureParm{}
		capture.TimePerFrame.Numerator = 1
		capture.TimePerFrame.Denominator = uint32(selection.FPS + 0.5)
		*(*v4l2CaptureParm)(unsafe.Pointer(&parm.Parm[0])) = capture
		_ = ioctl(s.fd, vidiocSParm, unsafe.Pointer(&parm))
	}
	return nil
}

// mapBuffers asks the driver for buffers, maps them, and queues them all.
func (s *Stream) mapBuffers() error {
	request := v4l2RequestBuffers{Count: bufferCount, Type: bufTypeVideoCapture, Memory: memoryMMAP}
	if err := ioctl(s.fd, vidiocReqBufs, unsafe.Pointer(&request)); err != nil {
		return fmt.Errorf("camera: requesting buffers on %s: %w", s.path, err)
	}
	if request.Count < 2 {
		return fmt.Errorf("camera: %s offered only %d buffers", s.path, request.Count)
	}

	for i := uint32(0); i < request.Count; i++ {
		buf := v4l2Buffer{Index: i, Type: bufTypeVideoCapture, Memory: memoryMMAP}
		if err := ioctl(s.fd, vidiocQueryBuf, unsafe.Pointer(&buf)); err != nil {
			s.unmap()
			return fmt.Errorf("camera: querying buffer %d on %s: %w", i, s.path, err)
		}
		data, err := unix.Mmap(s.fd, int64(buf.Offset), int(buf.Length),
			unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			s.unmap()
			return fmt.Errorf("camera: mapping buffer %d on %s: %w", i, s.path, err)
		}
		s.buffers = append(s.buffers, data)

		queued := v4l2Buffer{Index: i, Type: bufTypeVideoCapture, Memory: memoryMMAP}
		if err := ioctl(s.fd, vidiocQBuf, unsafe.Pointer(&queued)); err != nil {
			s.unmap()
			return fmt.Errorf("camera: queueing buffer %d on %s: %w", i, s.path, err)
		}
	}
	return nil
}

// ErrNoFrame means the camera had nothing ready within the wait.
//
// It is not a failure and must not be reported as one. A camera pointed at a display that is not showing
// anything is working perfectly; so is one whose next frame has not been exposed yet. Both are simply the
// channel being quiet, which is the normal state between transmissions.
var ErrNoFrame = errors.New("camera: no frame ready")

// Frame is one captured image and the bytes it came from.
type Frame struct {
	Image image.Image

	// JPEG is the frame as the camera produced it, when the camera produces JPEG. Kept because it is what a
	// stored capture should be: the bytes the decoder was given, not a re-encoding of them.
	JPEG []byte

	At time.Time
}

// Next waits for a frame, up to the given timeout.
func (s *Stream) Next(timeout time.Duration) (Frame, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return Frame{}, errors.New("camera: the stream is closed")
	}

	// Waited for with poll rather than a blocking read, so a quiet camera costs a timeout rather than a
	// goroutine stuck in the kernel until something arrives.
	fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(timeout.Milliseconds()))
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return Frame{}, ErrNoFrame
		}
		return Frame{}, fmt.Errorf("camera: waiting on %s: %w", s.path, err)
	}
	if n == 0 {
		return Frame{}, ErrNoFrame
	}

	buf := v4l2Buffer{Type: bufTypeVideoCapture, Memory: memoryMMAP}
	if err := ioctl(s.fd, vidiocDQBuf, unsafe.Pointer(&buf)); err != nil {
		if errors.Is(err, unix.EAGAIN) {
			return Frame{}, ErrNoFrame
		}
		return Frame{}, fmt.Errorf("camera: dequeuing a frame from %s: %w", s.path, err)
	}
	if int(buf.Index) >= len(s.buffers) {
		return Frame{}, fmt.Errorf("camera: %s returned buffer %d of %d", s.path, buf.Index, len(s.buffers))
	}

	// Copied before the buffer goes back. The driver refills it as soon as it is queued, and an image holding
	// a reference to memory being overwritten is a frame that changes while it is being decoded.
	raw := make([]byte, buf.BytesUsed)
	copy(raw, s.buffers[buf.Index][:buf.BytesUsed])

	requeue := v4l2Buffer{Index: buf.Index, Type: bufTypeVideoCapture, Memory: memoryMMAP}
	if err := ioctl(s.fd, vidiocQBuf, unsafe.Pointer(&requeue)); err != nil {
		return Frame{}, fmt.Errorf("camera: requeueing a buffer on %s: %w", s.path, err)
	}

	frame := Frame{At: time.Now()}
	switch s.format {
	case fourccMJPG:
		img, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			// A corrupt JPEG from the camera is a channel event, not a fault in this process: a USB frame can
			// arrive short. Reported as no frame so the next one is simply taken.
			return Frame{}, ErrNoFrame
		}
		frame.Image, frame.JPEG = img, raw
	case fourccYUYV:
		frame.Image = yuyvToRGBA(raw, s.width, s.height)
	}
	if frame.Image == nil {
		return Frame{}, ErrNoFrame
	}
	return frame, nil
}

// Mode describes what the camera actually agreed to, which is not always what was asked for.
func (s *Stream) Mode() (width, height int, format string) {
	return s.width, s.height, fourcc(s.format)
}

// Close stops the stream and releases the buffers.
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	_ = ioctl(s.fd, vidiocStreamOff, unsafe.Pointer(&[]int32{bufTypeVideoCapture}[0]))
	s.unmap()
	return unix.Close(s.fd)
}

func (s *Stream) unmap() {
	for _, b := range s.buffers {
		_ = unix.Munmap(b)
	}
	s.buffers = nil
}

// yuyvToRGBA converts a packed 4:2:2 frame, where each four bytes carry two pixels sharing their colour.
//
// The full BT.601 conversion rather than a grey approximation, because the payload encodings this platform
// uses put information in hue: reducing a colour frame to luma would throw away three of the four bits a
// color16 cell carries.
func yuyvToRGBA(raw []byte, width, height int) image.Image {
	if width <= 0 || height <= 0 || len(raw) < width*height*2 {
		return nil
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	for i, j := 0, 0; i+3 < len(raw) && j+7 < len(out.Pix); i, j = i+4, j+8 {
		y0, u, y1, v := float64(raw[i]), float64(raw[i+1])-128, float64(raw[i+2]), float64(raw[i+3])-128
		r0, g0, b0 := yuvToRGB(y0, u, v)
		r1, g1, b1 := yuvToRGB(y1, u, v)
		out.Pix[j], out.Pix[j+1], out.Pix[j+2], out.Pix[j+3] = r0, g0, b0, 255
		out.Pix[j+4], out.Pix[j+5], out.Pix[j+6], out.Pix[j+7] = r1, g1, b1, 255
	}
	return out
}

func yuvToRGB(y, u, v float64) (uint8, uint8, uint8) {
	return clamp8(y + 1.402*v), clamp8(y - 0.344*u - 0.714*v), clamp8(y + 1.772*u)
}

func clamp8(value float64) uint8 {
	switch {
	case value <= 0:
		return 0
	case value >= 255:
		return 255
	default:
		return uint8(value + 0.5)
	}
}
