// Package pipeline is the receiver's whole job: watch the optical channel, decode what it sees,
// acknowledge every chunk, and once a file is complete, merge it, verify it, deliver it, and tell
// the sender what happened.
//
// The order of those steps is what the design is about. Every captured frame is written to disk
// before it is decoded, so a session can be replayed later under a different decoder profile —
// which is the only way to debug a bad capture after the fact, since the frames themselves are
// long gone from the display. Every chunk is acknowledged as soon as it is verified rather than at
// the end, because the sender's window is what limits how far ahead it can run, and a late
// acknowledgement stalls it. And the merged file is verified against the hash the manifest
// declared before it is delivered anywhere, because a receiver that posted an unverified file
// would turn a silent optical error into corrupt data in somebody else's system.
package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// Source is where captured frames come from.
//
// The interface exists so the decode path never knows whether it is looking at a real camera. A
// file-backed source over a shared volume and a GoCV capture from a USB camera present the same
// thing: a stream of images with a sequence number. Everything downstream — persistence,
// decoding, acknowledgement, merging — is identical, which is what makes the whole pipeline
// testable without hardware.
type Source interface {
	// Name is the source's configuration name, for logs.
	Name() string

	// Next returns the next captured frame, blocking until one is available or the context is
	// done. It returns ErrNoFrame when the channel is idle rather than an error, because an idle
	// channel is the normal state between transmissions.
	Next(ctx context.Context) (Capture, error)

	// Close releases the source.
	Close() error
}

// ErrNoFrame means nothing new has appeared on the channel.
var ErrNoFrame = errors.New("pipeline: no frame available")

// Capture is one image off the channel.
type Capture struct {
	// Sequence orders captures within a session. It is the camera's count, not the sender's
	// frame number — a frame displayed three times is captured three times and gets three
	// sequence numbers, which is what lets the receiver report on the channel rather than only
	// on the transmission.
	Sequence int64

	Image image.Image

	// Raw is the encoded image as captured, or nil if the source produced pixels directly. It is
	// stored verbatim when present, so a replay decodes exactly what the camera saw rather than a
	// re-encoding of it.
	Raw []byte

	CapturedAt time.Time
}

// FileSource reads frames a sender wrote to a shared volume.
//
// It is the virtual optical channel: the sender's file sink writes a frame, this reads it. That
// makes the whole pipeline exercisable with no camera and no display, and — more usefully — makes
// the channel's behaviour controllable, so a test can degrade frames or drop them on purpose and
// see what the receiver does about it.
type FileSource struct {
	dir     string
	degrade simulate.Profile
	drop    func(sequence int64) bool
	consume bool

	mu       sync.Mutex
	seen     map[string]bool
	sequence int64
}

// FileSourceOptions configure a file-backed source.
type FileSourceOptions struct {
	// Dir is the directory the sender writes frames into.
	Dir string

	// Degrade is applied to every frame before it is decoded, standing in for the optics. The
	// zero value is a perfect channel.
	Degrade simulate.Profile

	// Drop reports whether a given capture should be discarded unread, standing in for a frame
	// the camera missed — a tear, a hand in front of the lens, a refresh caught mid-scan. It is
	// how loss is injected deliberately, because a channel that never loses anything cannot
	// demonstrate that loss is recovered.
	Drop func(sequence int64) bool

	// Consume deletes each frame after reading it, which is what makes the shared directory
	// behave like a channel rather than an archive: a frame goes by once.
	Consume bool
}

// NewFileSource returns a source over a directory.
func NewFileSource(opts FileSourceOptions) (*FileSource, error) {
	if opts.Dir == "" {
		return nil, errors.New("pipeline: the file source needs a directory")
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}
	return &FileSource{
		dir:     opts.Dir,
		degrade: opts.Degrade,
		drop:    opts.Drop,
		consume: opts.Consume,
		seen:    map[string]bool{},
	}, nil
}

// Name returns the source's configuration name.
func (s *FileSource) Name() string { return "file" }

// Next returns the next unread frame in the directory.
func (s *FileSource) Next(ctx context.Context) (Capture, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return Capture{}, fmt.Errorf("pipeline: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		// A name beginning with a dot is a write in progress. Reading one would produce a
		// truncated image that failed to decode and was counted as an optical error, when in fact
		// nothing had gone wrong optically at all.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !s.consume && s.seen[e.Name()] {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return Capture{}, ErrNoFrame
	}
	// Sorted, so frames are captured in the order they were displayed. A camera has no choice
	// about this; a directory does, and taking them out of order would be a fiction.
	sort.Strings(names)

	name := names[0]
	path := filepath.Join(s.dir, name)
	s.seen[name] = true
	s.sequence++
	sequence := s.sequence

	raw, err := os.ReadFile(path)
	if s.consume {
		// Removed whether or not it read cleanly: the frame has gone by either way, and leaving it
		// would have the receiver retry an image the display has already replaced.
		_ = os.Remove(path)
	}
	if err != nil {
		return Capture{}, fmt.Errorf("pipeline: %w", err)
	}

	if s.drop != nil && s.drop(sequence) {
		// The frame was displayed and the camera missed it. Reported as no frame rather than as an
		// error, because that is exactly what it is from the receiver's point of view.
		return Capture{}, ErrNoFrame
	}

	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return Capture{}, fmt.Errorf("pipeline: %w", err)
	}

	captured := Capture{Sequence: sequence, Image: img, Raw: raw, CapturedAt: time.Now()}
	if s.degrade != (simulate.Profile{}) {
		degraded := s.degrade.Apply(img)
		captured.Image = degraded

		// The stored bytes are what was captured, so a replay sees the degraded image the decoder
		// actually worked on rather than the pristine one the sender wrote.
		var buf bytes.Buffer
		if err := png.Encode(&buf, degraded); err == nil {
			captured.Raw = buf.Bytes()
		}
	}
	return captured, nil
}

// Close releases nothing.
func (s *FileSource) Close() error { return nil }

// OpenSource returns the source a configuration selects.
func OpenSource(cfg config.Capture) (Source, error) {
	switch cfg.Source {
	case "file":
		return NewFileSource(FileSourceOptions{
			Dir:     cfg.Dir,
			Consume: cfg.Consume,
			Degrade: simulatedOptics(cfg.Simulate),
		})
	default:
		return nil, fmt.Errorf("pipeline: %q is not a known capture source", cfg.Source)
	}
}

// simulatedOptics maps a configured profile name onto a degradation.
//
// The zero value is a perfect channel, which is what a file-to-file deployment gets by default — and which
// proves only that the decoder can read the encoder's own output. Naming a profile makes the virtual channel
// behave like a real one: blur, sensor noise, an off-axis camera, vignetting, and the compression a camera
// applies on the way out.
func simulatedOptics(profile string) simulate.Profile {
	switch profile {
	case "clean":
		return simulate.Clean
	case "typical":
		return simulate.Typical
	case "harsh":
		return simulate.Harsh
	case "rolling-shutter":
		return simulate.RollingShutter
	default:
		return simulate.Profile{}
	}
}

// storeCapture writes a captured frame to the object store and returns its key and hash.
func (r *Receiver) storeCapture(ctx context.Context, sessionID uuid.UUID, c Capture) (string, []byte, error) {
	raw := c.Raw
	if len(raw) == 0 {
		var buf bytes.Buffer
		if err := png.Encode(&buf, c.Image); err != nil {
			return "", nil, err
		}
		raw = buf.Bytes()
	}

	key := fmt.Sprintf("captures/%s/%012d.png", sessionID, c.Sequence)
	if err := r.objects.Put(ctx, key, bytes.NewReader(raw)); err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return key, sum[:], nil
}

// readObject reads a stored object with a bound on its size.
func (r *Receiver) readObject(ctx context.Context, key string, limit int64) ([]byte, error) {
	reader, err := r.objects.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("pipeline: %s is larger than the %d bytes expected", key, limit)
	}
	return data, nil
}

// decodeQuality summarises how well a frame was read, for the operator's display.
func decodeQuality(g *protocol.Geometry) (finder, timing, contrast float64) {
	if g == nil {
		return 0, 0, 0
	}
	return g.FinderScore, g.TimingScore, g.Contrast
}

// decodeFrame reads a captured image.
//
// The geometry is located once and the frame decoded at it, rather than letting the encoder locate
// again, so the quality figures reported to the operator describe the same read that produced the
// payload.
func decodeFrame(img image.Image, opts protocol.LocateOptions) (*protocol.Frame, *protocol.Geometry, error) {
	g, err := protocol.Locate(img, opts)
	if err != nil {
		return nil, nil, err
	}
	frame, err := encoding.Decode(img, opts)
	if err != nil {
		return nil, g, err
	}
	return frame, g, nil
}

// zapFrame renders a frame's identity for a log line.
func zapFrame(frame *protocol.Frame) []zap.Field {
	if frame == nil {
		return nil
	}
	return []zap.Field{
		zap.String("transmission", frame.Header.TransmissionID.String()),
		zap.Uint32("frame", frame.Header.FrameNumber),
		zap.Uint32("chunk", frame.Header.ChunkNumber),
		zap.String("flags", frame.Header.Flags.String()),
	}
}
