//go:build linux

package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"sync/atomic"
	"time"

	"github.com/opticaltransport/otp/receiver/internal/camera"
)

// CameraSource captures from a real camera.
//
// It is the same Source the file-backed channel implements, and that is the point of the interface: persisting,
// decoding, acknowledging, merging, verifying and delivering are identical whatever produced the image. What
// differs is only the two facts below.
//
// **A camera never runs out of frames.** The file channel goes quiet between transmissions and says so with
// ErrNoFrame; a camera pointed at a dark screen keeps producing images of a dark screen, thirty times a second,
// for as long as it is on. Every one of those would otherwise be persisted and recorded as an unreadable
// capture — thousands of rows and thousands of stored images saying nothing except that the sender has not
// started yet. So a frame in which no grid can be found at all is reported as no frame, which is what it
// honestly is: the channel is idle, and the camera is waiting for something to be shown.
//
// **A camera cannot be asked to repeat itself.** The file channel keeps a frame until it is read; a camera's
// last frame is gone the moment the next is exposed. Nothing downstream depends on being able to re-read one,
// because the sender re-shows every chunk until it is acknowledged.
type CameraSource struct {
	stream *camera.Stream

	sequence atomic.Int64

	// idle counts frames in which nothing was found. It is the receiver's own measure of a camera that is
	// running and seeing nothing — aimed at the wrong thing, or simply waiting for the display to start.
	idle atomic.Int64

	// seen counts frames in which a grid was found, so an operator can tell "waiting" from "misaimed".
	seen atomic.Int64

	width, height int
	format        string
	device        string
}

// OpenCamera starts capturing from the camera a selection names.
func OpenCamera(selection camera.Selection) (*CameraSource, error) {
	stream, err := camera.Open(selection)
	if err != nil {
		return nil, err
	}
	width, height, format := stream.Mode()
	return &CameraSource{
		stream: stream,
		width:  width, height: height, format: format,
		device: selection.Device,
	}, nil
}

// Name is the source's configuration name.
func (s *CameraSource) Name() string { return "camera" }

// Mode is what the camera actually agreed to, which is not always what was asked for.
func (s *CameraSource) Mode() (width, height int, format string) {
	return s.width, s.height, s.format
}

// Idle and Seen are how many frames held nothing and how many held a grid.
func (s *CameraSource) Idle() int64 { return s.idle.Load() }
func (s *CameraSource) Seen() int64 { return s.seen.Load() }

// cameraWait is how long to wait for the next frame before reporting the channel quiet.
//
// Longer than a frame interval at any rate a camera runs at, so a frame is not missed for want of patience, and
// short enough that a shutdown is noticed promptly.
const cameraWait = 500 * time.Millisecond

// Next captures one frame, or reports the channel quiet.
func (s *CameraSource) Next(ctx context.Context) (Capture, error) {
	if err := ctx.Err(); err != nil {
		return Capture{}, err
	}

	frame, err := s.stream.Next(cameraWait)
	switch {
	case err == nil:
	case errors.Is(err, camera.ErrNoFrame):
		// The camera had nothing ready. Not an event: between exposures is where a camera spends its time.
		return Capture{}, ErrNoFrame
	default:
		return Capture{}, err
	}

	// Nothing found in the image at all: no fiducials, no grid, nothing that could be a frame. That is a camera
	// looking at a display which is not showing anything, and it is the state a receiver sits in while it waits
	// for a transfer to start. Recording it would fill the failure log with thousands of images of a blank
	// screen and bury the failures that mean something.
	if !looksLikeAFrame(frame.Image) {
		s.idle.Add(1)
		return Capture{}, ErrNoFrame
	}
	s.seen.Add(1)

	sequence := s.sequence.Add(1)
	captured := Capture{Sequence: sequence, Image: frame.Image, CapturedAt: frame.At}

	// The stored bytes are what the camera produced when it produced JPEG, because that is the honest record of
	// what the decoder was given. Otherwise the image is encoded as PNG so that a stored capture can be looked
	// at at all.
	if len(frame.JPEG) > 0 {
		captured.Raw = frame.JPEG
	} else {
		var buf bytes.Buffer
		if err := png.Encode(&buf, frame.Image); err != nil {
			return Capture{}, fmt.Errorf("pipeline: encoding a captured frame: %w", err)
		}
		captured.Raw = buf.Bytes()
	}
	return captured, nil
}

// Close stops the camera.
func (s *CameraSource) Close() error { return s.stream.Close() }

// looksLikeAFrame is a cheap test for "is there anything on that screen".
//
// It exists to keep a camera that is waiting from filling the failure log. Locating a frame properly means
// finding fiducials, fitting a homography and sampling every cell — hundreds of milliseconds — and doing it on
// every image of a blank screen would spend the whole receiver on discovering nothing, as well as storing a
// picture of nothing each time.
//
// The test is a property every frame this protocol renders has and almost nothing else does: **a lot of pure
// black and a lot of pure white, at once**. The quiet zone, the four fiducials and the always-binary header and
// footer bands guarantee both. A dark room has the black and none of the white; a bright blank screen has the
// white and none of the black; a photograph of a desk has plenty of midtones and little of either.
//
// It is a gate, not a decision. Anything that passes still goes through the real decoder, which will reject it
// on its checksums if it was a false positive — so the threshold is set low enough to let a badly lit or
// off-axis frame through and accept the occasional wasted decode.
func looksLikeAFrame(img image.Image) bool {
	if img == nil {
		return false
	}
	bounds := img.Bounds()
	if bounds.Dx() < 32 || bounds.Dy() < 32 {
		return false
	}

	// Every eighth pixel in each direction: a sixty-fourth of the work, and a frame's bands are thousands of
	// pixels across so nothing that matters is missed.
	const step = 8
	var dark, light, total int
	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			r, g, b, _ := img.At(x, y).RGBA()
			// Rec. 601 luma on the 16-bit values the image package returns, shifted back to 0..255.
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(b>>8)) / 1000
			switch {
			case luma < 64:
				dark++
			case luma > 192:
				light++
			}
			total++
		}
	}
	if total == 0 {
		return false
	}

	// A twelfth of each. A frame's quiet zone alone is well past this, and it is far below what any evenly lit
	// scene produces at both ends of the range at once.
	const floor = 12
	return dark*floor >= total && light*floor >= total
}
