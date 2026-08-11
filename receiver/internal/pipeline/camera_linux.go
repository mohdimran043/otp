//go:build linux

package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"math"
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

	// minTone is the blank-screen threshold, adjustable while the camera runs.
	minTone atomic.Uint64

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
	if !looksLikeAFrame(frame.Image, math.Float64frombits(s.minTone.Load())) {
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

// Exclusive reports that this source holds a device that cannot be opened twice, which decides the order a
// source swap has to happen in.
func (s *CameraSource) Exclusive() bool { return true }

// SetMinToneFraction adjusts the blank-screen threshold.
func (s *CameraSource) SetMinToneFraction(f float64) {
	s.minTone.Store(math.Float64bits(f))
}
