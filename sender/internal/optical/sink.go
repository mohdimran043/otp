// Package optical is the sender's side of the channel: where a rendered frame goes to be seen.
package optical

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// Sink is where frames are displayed.
//
// The interface is what keeps the scheduler honest about what it does and does not know. It picks
// which frame should be on screen next and hands it over; whether that means writing a file to a
// shared volume or drawing a texture on a monitor is not its business, and a scheduler that knew
// the difference would end up with two subtly different display policies.
type Sink interface {
	// Name is the sink's configuration name, for logs and diagnostics.
	Name() string

	// Show displays a frame. It returns once the frame is visible, or as close to that as the sink
	// can tell — for a file sink, once the bytes are durable and named.
	Show(ctx context.Context, frame Frame) error

	// Shown is how many frames this sink has displayed.
	Shown() int64

	// Close releases the sink.
	Close() error
}

// Frame is one image on its way to the channel.
type Frame struct {
	// Sequence is the display order, counted by the sink rather than by the transmission: a frame
	// re-displayed as a retransmission gets a new sequence number and the same frame number, which
	// is what lets the two sides talk about the channel separately from the file.
	Sequence int64

	// Number is the frame's own number within its transmission, for logs.
	Number int

	// Transmission and Manifest identify what is on screen, for an operator watching the display and
	// for anything following it over HTTP. They travel with the frame rather than being looked up from
	// its sequence number, because by the time a viewer asks, the display has usually moved on.
	Transmission uuid.UUID
	Manifest     bool

	// WidthPx and HeightPx are the image's dimensions, so a page can lay out space for a frame before
	// it has decoded one, and refuse a scale factor that would not fit.
	WidthPx  int
	HeightPx int

	// PNG is the encoded image. It is passed already encoded because the pipeline stored it that
	// way, and re-encoding a frame on every display would spend real time producing identical bytes.
	PNG []byte

	// Cleared marks the end of a transfer rather than a picture: the screen is now empty.
	//
	// Published as a frame, with its own sequence, rather than by simply forgetting the last one. A
	// viewer following the display is parked in a long poll waiting for the sequence to advance, so a
	// clear that only dropped state would leave every watcher holding the frame that is no longer there
	// until its own timeout expired — and the receiver would go on photographing a picture the sender
	// had stopped showing.
	Cleared bool

	// Image is the decoded image, or nil. A sink that draws pixels needs it; a sink that writes
	// files does not, so it is only decoded when something asks.
	Image image.Image
}

// FileSink writes frames into a directory the receiver reads.
//
// It is the virtual optical channel, and the reason the whole platform can be tested without a
// monitor or a camera. A frame written here is a frame displayed; the receiver's file source reading
// it is a camera capturing it. Everything above this — scheduling, acknowledgement, retransmission —
// behaves identically whether the channel is a directory or a room.
type FileSink struct {
	dir string

	// keep controls whether a displayed frame is left behind. Off by default, because a display is
	// not an archive: a frame that stays visible would be captured again and again, and the
	// receiver would spend its time acknowledging duplicates instead of reading new chunks.
	keep bool

	// written is the names still on the channel, oldest first, and it is what bounds the directory.
	//
	// Without it the directory grows for as long as the display runs. That is not hypothetical: the
	// display writes a frame per tick while the receiver reads several a second, so at any realistic
	// frame rate the sender outruns the reader by two orders of magnitude. The growth went unnoticed for
	// a long time because the display sequence used to restart at one for every transmission, so a later
	// transfer's frames overwrote the previous one's leavings — accidental cleanup that stopped the
	// moment the counter became global, which is what it had to become for concurrent transfers not to
	// collide.
	//
	// A backlog rather than a single frame, because the file channel stands in for a screen that a
	// slower reader is watching, and some slack is what lets it lose nothing. Bounded slack, though: a
	// reader further behind than this was never going to catch up, and the acknowledgement rule will
	// have the frame shown again anyway.
	mu      sync.Mutex
	written []string

	shown atomic.Int64
}

// displayBacklog is how many displayed frames stay on the channel.
//
// Generous on purpose — a few tens of megabytes at the densest geometry — because the cost of being too
// small is a frame the receiver never sees, and the cost of being too large is disk. What matters is that
// it is a bound at all.
const displayBacklog = 4096

// NewFileSink returns a sink over a directory, creating it if necessary.
func NewFileSink(dir string, keep bool) (*FileSink, error) {
	if dir == "" {
		return nil, errors.New("optical: the file sink needs a directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("optical: %w", err)
	}
	return &FileSink{dir: dir, keep: keep}, nil
}

// Name returns the sink's configuration name.
func (s *FileSink) Name() string { return "file" }

// Dir is where frames are written.
func (s *FileSink) Dir() string { return s.dir }

// Show writes a frame into the directory.
//
// The write goes to a dotted temporary name and is then renamed, and both halves matter. The
// receiver is reading this directory continuously, so a frame it saw mid-write would decode as
// garbage and be counted as an optical error — a fault attributed to the channel that was really a
// local mistake. The rename makes the frame appear complete or not at all, and the dot keeps the
// partial file out of the receiver's view even during the moment it exists.
func (s *FileSink) Show(ctx context.Context, frame Frame) error {
	if len(frame.PNG) == 0 {
		if frame.Image == nil {
			return errors.New("optical: a frame needs either encoded bytes or an image")
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, frame.Image); err != nil {
			return fmt.Errorf("optical: %w", err)
		}
		frame.PNG = buf.Bytes()
	}

	// Named by display sequence rather than frame number, so a retransmission does not overwrite the
	// frame it is repeating before the receiver has had a chance to see either.
	name := fmt.Sprintf("%012d.png", frame.Sequence)
	final := filepath.Join(s.dir, name)
	tmp := filepath.Join(s.dir, "."+name+".tmp")

	if err := os.WriteFile(tmp, frame.PNG, 0o640); err != nil {
		return fmt.Errorf("optical: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("optical: %w", err)
	}

	s.shown.Add(1)
	s.prune(final)
	return nil
}

// prune records a written frame and removes the ones that have fallen off the back of the channel.
func (s *FileSink) prune(final string) {
	if s.keep {
		return
	}

	s.mu.Lock()
	s.written = append(s.written, final)
	var stale []string
	if excess := len(s.written) - displayBacklog; excess > 0 {
		stale = append(stale, s.written[:excess]...)
		s.written = s.written[excess:]
	}
	s.mu.Unlock()

	for _, path := range stale {
		// The receiver deletes what it reads, so a frame is often already gone. That is the normal case
		// rather than an error: both ends removing the same file is two halves of the same intention.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Nothing to be done about it here, and it must not stop the display. A directory that
			// cannot be pruned grows, which is a slow problem; a display that stopped would be an
			// immediate one.
			continue
		}
	}
}

// Shown is how many frames this sink has displayed.
func (s *FileSink) Shown() int64 { return s.shown.Load() }

// Close releases nothing.
func (s *FileSink) Close() error { return nil }

// noneSink discards every frame it is given.
//
// It is what camera-only mode configures: the receiver watches the physical display with its own
// camera, so a copy of every frame also landing in the shared directory would be a second, pointless
// channel and disk nobody reads back. It still has to accept the frame rather than refuse it, because
// Live sits above every sink and publishes to Current/Next only after the wrapped Show succeeds — so a
// sink that errored here would take the browser's Display page and camera-view down with it, which is
// the one thing camera-only mode must not do.
type noneSink struct {
	shown atomic.Int64
}

// newNoneSink returns a sink that discards every frame.
func newNoneSink() *noneSink { return &noneSink{} }

// Name returns the sink's configuration name.
func (s *noneSink) Name() string { return "none" }

// Show discards the frame. Nothing is written anywhere.
func (s *noneSink) Show(ctx context.Context, frame Frame) error {
	s.shown.Add(1)
	return nil
}

// Shown is how many frames this sink has accepted and discarded.
func (s *noneSink) Shown() int64 { return s.shown.Load() }

// Close releases nothing.
func (s *noneSink) Close() error { return nil }

// Open returns the sink a configuration selects, wrapped so the display has one sequence and one answer
// to what is on it.
//
// The wrapper is not optional, and that is the point of returning it from here: the display sequence is
// assigned by Live, so a bare sink reaching a scheduler would mean every frame written to the same name.
func Open(cfg config.Display) (*Live, error) {
	inner, err := open(cfg)
	if err != nil {
		return nil, err
	}
	return NewLive(inner), nil
}

func open(cfg config.Display) (Sink, error) {
	switch cfg.Sink {
	case "file":
		return NewFileSink(cfg.Dir, cfg.RetainFrames)
	case "none":
		return newNoneSink(), nil
	case "opengl":
		// The OpenGL sink is behind a build tag, so a binary without it says so plainly rather than
		// falling back to a channel the operator did not ask for and may not notice.
		return nil, errors.New("optical: this build has no OpenGL sink; rebuild with -tags opengl")
	default:
		return nil, fmt.Errorf("optical: %q is not a known display sink", cfg.Sink)
	}
}
