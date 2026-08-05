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
	"sync/atomic"

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

	// PNG is the encoded image. It is passed already encoded because the pipeline stored it that
	// way, and re-encoding a frame on every display would spend real time producing identical bytes.
	PNG []byte

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

	shown atomic.Int64
}

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
	return nil
}

// Shown is how many frames this sink has displayed.
func (s *FileSink) Shown() int64 { return s.shown.Load() }

// Close releases nothing.
func (s *FileSink) Close() error { return nil }

// Open returns the sink a configuration selects.
func Open(cfg config.Display) (Sink, error) {
	switch cfg.Sink {
	case "file":
		return NewFileSink(cfg.Dir, cfg.RetainFrames)
	case "opengl":
		// The OpenGL sink is behind a build tag, so a binary without it says so plainly rather than
		// falling back to a channel the operator did not ask for and may not notice.
		return nil, errors.New("optical: this build has no OpenGL sink; rebuild with -tags opengl")
	default:
		return nil, fmt.Errorf("optical: %q is not a known display sink", cfg.Sink)
	}
}
