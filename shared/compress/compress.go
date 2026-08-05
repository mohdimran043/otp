// Package compress implements the compressors a transmission may use, behind a
// plug-in interface.
//
// Compression is where the platform's throughput actually comes from. An optical
// channel carries a fixed number of bytes per frame at a fixed frame rate, so the
// only way to move a large file faster is to have fewer bytes to move. A file that
// compresses two to one halves the transmission time outright, which is why the
// compressor is an operator-visible choice with real trade-offs rather than a
// hidden implementation detail.
//
// The interface is stream-first. This platform's whole purpose is files too large
// to move any other way, and a compressor that had to hold its input in memory
// would fail on precisely the files it was bought for.
package compress

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/opticaltransport/otp/shared/internal/registry"
)

// Compressor identifiers, carried in the frame header and the manifest so a
// receiver knows what to run in reverse. They are wire values: never renumber them.
const (
	IDNone   uint8 = 0
	IDGzip   uint8 = 1
	IDLZ4    uint8 = 2
	IDZstd   uint8 = 3
	IDBrotli uint8 = 4
)

// Errors returned by compressors.
var (
	// ErrUnknownCompressor means no compressor is registered under that name or id.
	ErrUnknownCompressor = errors.New("compress: unknown compressor")

	// ErrLimitExceeded means a stream expanded past the limit the caller allowed.
	// It is a rejection, not a failure: see the note on Decompress.
	ErrLimitExceeded = errors.New("compress: decompressed output exceeds the permitted size")

	// ErrBadLevel means a compression level was outside 1..9.
	ErrBadLevel = errors.New("compress: level must be between 1 and 9, or 0 for the codec's default")
)

// DefaultMaxDecompressed bounds decompression when the caller has no better figure
// to offer. Sixteen gigabytes is far above any plausible transmission and far below
// anything that would exhaust a server.
const DefaultMaxDecompressed int64 = 16 << 30

// Compressor is the plug-in interface every compressor implements.
type Compressor interface {
	// ID is the wire identifier written into the frame header and the manifest.
	ID() uint8

	// Name is the stable configuration name, such as "zstd".
	Name() string

	// Description is one line for the profile UI and generated documentation.
	Description() string

	// Compress copies src to dst, compressed. level is 1..9 from fastest to
	// smallest, or 0 for the codec's own default; each codec maps that range onto
	// whatever scale it actually has.
	Compress(dst io.Writer, src io.Reader, level int) error

	// Decompress copies src to dst, expanded, and refuses to write more than limit
	// bytes.
	//
	// The limit is not optional, and it is not a memory optimisation. A receiver
	// decompresses a stream that arrived from outside its trust boundary, and every
	// one of these formats can express a small input that expands without bound —
	// a few kilobytes of frames could otherwise fill a disk. The caller knows the
	// figure to use: the manifest states the original size, so anything beyond it
	// is by definition not the file that was sent. Pass DefaultMaxDecompressed only
	// when no manifest is available.
	Decompress(dst io.Writer, src io.Reader, limit int64) error
}

var compressors = registry.New[Compressor]("compressor", ErrUnknownCompressor)

// Register adds a compressor to the registry. It panics on a duplicate id or name,
// because a collision would mean streams decompressed by the wrong codec.
func Register(c Compressor) { compressors.Register(c) }

// ByID returns the compressor with that wire identifier.
func ByID(id uint8) (Compressor, error) { return compressors.ByID(id) }

// ByName returns the compressor with that configuration name.
func ByName(name string) (Compressor, error) { return compressors.ByName(name) }

// All returns every registered compressor, ordered by id.
func All() []Compressor { return compressors.All() }

// Names returns every registered compressor name, ordered by id.
func Names() []string { return compressors.Names() }

// Bytes compresses a byte slice. It is for records small enough to hold in memory
// — a manifest, a chunk, a test vector — and never for a whole file.
func Bytes(c Compressor, src []byte, level int) ([]byte, error) {
	var out bytes.Buffer
	if err := c.Compress(&out, bytes.NewReader(src), level); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// UnBytes expands a byte slice, refusing to produce more than limit bytes.
func UnBytes(c Compressor, src []byte, limit int64) ([]byte, error) {
	var out bytes.Buffer
	if err := c.Decompress(&out, bytes.NewReader(src), limit); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// checkLevel validates a level. Zero is permitted and means the codec's default.
func checkLevel(level int) error {
	if level != 0 && (level < 1 || level > 9) {
		return fmt.Errorf("%w: got %d", ErrBadLevel, level)
	}
	return nil
}

// limitedWriter passes through at most limit bytes and then fails.
//
// Enforcing the bound on the way out rather than by inspecting the compressed
// stream is what makes it codec-independent: no format has to be trusted to
// declare its own output size honestly, because none of them is asked.
type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.remaining {
		// Write what is still permitted before failing, so a caller that streams to
		// disk sees a prefix of the file rather than nothing, and can report where
		// the stream went wrong.
		if l.remaining > 0 {
			n, err := l.w.Write(p[:l.remaining])
			l.remaining -= int64(n)
			if err != nil {
				return n, err
			}
			return n, ErrLimitExceeded
		}
		return 0, ErrLimitExceeded
	}
	n, err := l.w.Write(p)
	l.remaining -= int64(n)
	return n, err
}

// copyLimited copies src to dst, stopping at limit.
func copyLimited(dst io.Writer, src io.Reader, limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("%w: limit must be positive, got %d", ErrLimitExceeded, limit)
	}
	_, err := io.Copy(&limitedWriter{w: dst, remaining: limit}, src)
	return err
}
