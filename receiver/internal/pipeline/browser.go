package pipeline

import (
	"context"
	"errors"
	"image"
	"sync"
	"sync/atomic"
	"time"
)

// BrowserSource takes frames posted by a browser that is holding the camera.
//
// It exists because of who is allowed to ask. A server process opening /dev/video0 gets no permission prompt and
// produces no indicator light beyond the one the driver turns on — the "permission" was granted when an operator
// passed the device into the container. A browser asking for a camera produces the dialog everybody recognises
// and the indicator the operating system controls, and it can do that from any machine that can reach the
// receiver rather than only the one the receiver runs on.
//
// So the camera is held by the page and the frames are posted here. The receiver treats them exactly as it
// treats a frame read from a directory or captured through V4L2 — persist, decode, acknowledge, merge, verify,
// deliver — because that is the point of the Source interface.
//
// What this trades away is throughput: encoding a frame in a canvas and posting it over HTTP will not keep up
// with the direct V4L2 path. That is the right trade for setting a camera up and the wrong one for moving fifty
// megabytes, so both sources exist and neither pretends to be the other.
type BrowserSource struct {
	// frames is a small buffer. Small on purpose: a deep queue would let the browser run ahead and hand the
	// decoder frames that left the screen seconds ago, which is the same mistake as reading a directory
	// oldest-first. When it is full the oldest waiting frame is dropped, because the newest is the one that
	// matters and a browser cannot be asked to wait.
	frames chan Capture

	sequence atomic.Int64

	// received and dropped count what arrived and what was discarded for arriving faster than it could be
	// decoded. The pair is the honest measure of a page posting faster than the receiver can keep up.
	received atomic.Int64
	dropped  atomic.Int64

	// idle counts posted frames that held nothing — a camera pointed at a display showing nothing. Counted
	// rather than recorded, for the same reason the V4L2 source does it: thousands of stored images of a blank
	// screen bury the failures that mean something.
	idle atomic.Int64

	mu     sync.Mutex
	closed bool

	// lastSeen is when a frame last arrived, so the receiver can tell "the browser is posting and the display is
	// blank" from "the browser has gone away".
	lastSeen atomic.Int64
}

// NewBrowserSource returns a source fed by posted frames.
func NewBrowserSource() *BrowserSource {
	return &BrowserSource{frames: make(chan Capture, 4)}
}

// Name is the source's configuration name.
func (s *BrowserSource) Name() string { return "browser" }

// Received, Dropped and Idle are the figures a settings page needs to explain itself.
func (s *BrowserSource) Received() int64 { return s.received.Load() }
func (s *BrowserSource) Dropped() int64  { return s.dropped.Load() }
func (s *BrowserSource) Idle() int64     { return s.idle.Load() }

// LastSeen is when a frame last arrived, or the zero time if none has.
func (s *BrowserSource) LastSeen() time.Time {
	unixMilli := s.lastSeen.Load()
	if unixMilli == 0 {
		return time.Time{}
	}
	return time.UnixMilli(unixMilli)
}

// ErrSourceClosed means frames were posted to a source that is no longer taking them.
var ErrSourceClosed = errors.New("pipeline: the browser source is closed")

// Push offers a frame captured by a browser.
//
// The idle gate is applied here rather than in the handler, so that every source applies the same rule in the
// same place: a frame with nothing in it is not a failure and is not recorded, because a camera watching a blank
// display is working perfectly and there is nothing to say about it.
//
// It never blocks. A browser posting faster than the receiver decodes would otherwise have its requests pile up
// waiting, and the honest answer to "I have a newer frame than you can use" is to take the new one and drop the
// old — which is what a camera does to a frame nobody read in time.
func (s *BrowserSource) Push(img image.Image, raw []byte) (accepted bool, err error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return false, ErrSourceClosed
	}

	s.received.Add(1)
	s.lastSeen.Store(time.Now().UnixMilli())

	if !looksLikeAFrame(img) {
		s.idle.Add(1)
		return false, nil
	}

	capture := Capture{
		Sequence:   s.sequence.Add(1),
		Image:      img,
		Raw:        raw,
		CapturedAt: time.Now(),
	}

	select {
	case s.frames <- capture:
		return true, nil
	default:
		// Full. Drop the oldest and take this one, because the newest frame is the one still on the display.
		select {
		case <-s.frames:
			s.dropped.Add(1)
		default:
		}
		select {
		case s.frames <- capture:
			return true, nil
		default:
			s.dropped.Add(1)
			return false, nil
		}
	}
}

// browserWait is how long Next waits for a posted frame before reporting the channel quiet.
const browserWait = 300 * time.Millisecond

// Next takes the next posted frame.
func (s *BrowserSource) Next(ctx context.Context) (Capture, error) {
	select {
	case capture, ok := <-s.frames:
		if !ok {
			return Capture{}, ErrNoFrame
		}
		return capture, nil
	case <-ctx.Done():
		return Capture{}, ctx.Err()
	case <-time.After(browserWait):
		// Nothing posted. Not an event: a browser that has not been opened yet, or a display with nothing on it,
		// are both this.
		return Capture{}, ErrNoFrame
	}
}

// Close stops taking frames.
func (s *BrowserSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.frames)
	return nil
}
