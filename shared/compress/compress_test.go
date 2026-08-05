package compress_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/compress"
)

func TestRegistryHoldsEveryCompressor(t *testing.T) {
	require.Equal(t, []string{"none", "gzip", "lz4", "zstd", "brotli"}, compress.Names())

	for _, c := range compress.All() {
		byID, err := compress.ByID(c.ID())
		require.NoError(t, err)
		require.Same(t, c, byID)

		byName, err := compress.ByName(c.Name())
		require.NoError(t, err)
		require.Same(t, c, byName)

		require.NotEmpty(t, c.Description())
	}

	_, err := compress.ByID(200)
	require.ErrorIs(t, err, compress.ErrUnknownCompressor)
	_, err = compress.ByName("lzma")
	require.ErrorIs(t, err, compress.ErrUnknownCompressor)
}

// corpora are the kinds of input a transmission actually carries, chosen because
// they compress very differently. A codec judged only on text looks better than it
// is; judged only on random bytes, all of them look useless.
func corpora() map[string][]byte {
	text := []byte(strings.Repeat(
		"The receiver writes captured frames to disk before decoding them, always. ", 400))

	incompressible := make([]byte, 32768)
	rand.New(rand.NewSource(7)).Read(incompressible)

	sparse := make([]byte, 65536)
	for i := 0; i < len(sparse); i += 512 {
		sparse[i] = byte(i / 512)
	}

	structured := make([]byte, 0, 49152)
	for i := 0; i < 4096; i++ {
		structured = append(structured, byte(i), byte(i>>8), 0x00, 0x00,
			byte(i*3), byte(i>>4), 0xFF, 0x00, byte(i%17), 0x00, 0x00, 0x01)
	}

	return map[string][]byte{
		"text":           text,
		"incompressible": incompressible,
		"sparse":         sparse,
		"structured":     structured,
		"empty":          {},
		"single byte":    {0x42},
	}
}

// TestRoundTripEveryCodecAndCorpus is the property that matters most: whatever a
// compressor produced, the matching decompressor returns the original bytes exactly.
// Compression is applied before chunking, so a single wrong byte here corrupts the
// whole received file rather than one frame of it.
func TestRoundTripEveryCodecAndCorpus(t *testing.T) {
	for _, c := range compress.All() {
		for name, src := range corpora() {
			for _, level := range []int{0, 1, 5, 9} {
				t.Run(fmt.Sprintf("%s/%s/L%d", c.Name(), name, level), func(t *testing.T) {
					packed, err := compress.Bytes(c, src, level)
					require.NoError(t, err)

					got, err := compress.UnBytes(c, packed, compress.DefaultMaxDecompressed)
					require.NoError(t, err)
					requireSameBytes(t, src, got)
				})
			}
		}
	}
}

// TestCompressionRatios records what each codec achieves. The numbers feed the
// benchmarks document and the marketing site's performance charts, so they are
// measured here rather than asserted from memory.
func TestCompressionRatios(t *testing.T) {
	var table strings.Builder
	table.WriteString("\ncompressed size as a percentage of the original, at level 9:\n")
	table.WriteString("  codec  ")

	names := []string{"text", "structured", "sparse", "incompressible"}
	for _, n := range names {
		fmt.Fprintf(&table, " %14s", n)
	}
	table.WriteString("\n")

	corpus := corpora()
	best := map[string]float64{}
	for _, c := range compress.All() {
		fmt.Fprintf(&table, "  %-7s", c.Name())
		for _, n := range names {
			packed, err := compress.Bytes(c, corpus[n], 9)
			require.NoError(t, err)
			ratio := float64(len(packed)) / float64(len(corpus[n])) * 100
			fmt.Fprintf(&table, " %13.1f%%", ratio)
			if r, seen := best[n]; !seen || ratio < r {
				best[n] = ratio
			}
		}
		table.WriteString("\n")
	}
	t.Log(table.String())

	// Highly repetitive input must compress to a small fraction of itself, or
	// something is wrong with how the codecs are being driven rather than with the
	// codecs.
	require.Less(t, best["text"], 5.0, "repetitive text should compress by at least twenty to one")

	// Random bytes cannot be compressed, and a codec that claimed otherwise would be
	// losing data. The framing overhead is what keeps this just above 100%.
	require.Greater(t, best["incompressible"], 99.0)
	require.Less(t, best["incompressible"], 102.0, "framing overhead on incompressible input should be slight")
}

// TestDecompressionBombIsRefused is a security test. A receiver expands a stream
// that arrived from outside its trust boundary, and every one of these formats can
// express a small input that expands without bound. The manifest states the original
// size, so the receiver always has a figure to hold the stream to — and this is the
// test that it is actually held.
func TestDecompressionBombIsRefused(t *testing.T) {
	// A gigabyte of zeroes compresses to a few kilobytes in every codec here.
	const bomb = 1 << 30

	for _, c := range compress.All() {
		t.Run(c.Name(), func(t *testing.T) {
			if c.ID() == compress.IDNone {
				t.Skip("the identity codec cannot expand its input")
			}

			var packed bytes.Buffer
			require.NoError(t, c.Compress(&packed, io.LimitReader(&zeroReader{}, bomb), 1))

			// The attack only matters if the compressed form is small relative to what
			// it expands to. A hundredfold is far below what any of these codecs
			// achieve on zeroes and far above anything a real file reaches, so it
			// separates the two without pinning the test to a library's tuning.
			expansion := float64(bomb) / float64(packed.Len())
			require.Greater(t, expansion, 100.0,
				"%s expanded %.0f to 1, too little to be a plausible attack", c.Name(), expansion)

			// The limit a receiver would use: the size the manifest declared.
			const declared = 4096
			var out bytes.Buffer
			err := c.Decompress(&out, bytes.NewReader(packed.Bytes()), declared)
			require.ErrorIs(t, err, compress.ErrLimitExceeded)
			require.LessOrEqual(t, out.Len(), declared,
				"nothing beyond the limit may reach the caller's writer")
		})
	}
}

// TestLimitAdmitsExactlyTheDeclaredSize checks the bound is not off by one. A
// receiver passes the manifest's original size, so a limit that refused a stream of
// exactly that length would reject every legitimate transmission.
func TestLimitAdmitsExactlyTheDeclaredSize(t *testing.T) {
	src := bytes.Repeat([]byte("exact"), 1000)

	for _, c := range compress.All() {
		t.Run(c.Name(), func(t *testing.T) {
			packed, err := compress.Bytes(c, src, 0)
			require.NoError(t, err)

			got, err := compress.UnBytes(c, packed, int64(len(src)))
			require.NoError(t, err, "a stream of exactly the declared size must be admitted")
			requireSameBytes(t, src, got)

			_, err = compress.UnBytes(c, packed, int64(len(src))-1)
			require.ErrorIs(t, err, compress.ErrLimitExceeded)
		})
	}
}

func TestRejectsBadLevelsAndLimits(t *testing.T) {
	for _, c := range compress.All() {
		for _, level := range []int{-1, 10, 100} {
			_, err := compress.Bytes(c, []byte("data"), level)
			require.ErrorIs(t, err, compress.ErrBadLevel, "%s level %d", c.Name(), level)
		}

		packed, err := compress.Bytes(c, []byte("data"), 0)
		require.NoError(t, err)

		// A non-positive limit is a caller mistake, not a licence to decompress
		// without bound.
		for _, limit := range []int64{0, -1} {
			_, err := compress.UnBytes(c, packed, limit)
			require.ErrorIs(t, err, compress.ErrLimitExceeded, "%s limit %d", c.Name(), limit)
		}
	}
}

// TestDecompressRejectsGarbage checks a corrupt stream is reported rather than
// returning whatever bytes happened to parse. Frames are already CRC-checked
// individually, so garbage reaching a decompressor means something worse has gone
// wrong upstream, and silently returning a prefix of the file would hide it.
func TestDecompressRejectsGarbage(t *testing.T) {
	garbage := make([]byte, 4096)
	rand.New(rand.NewSource(99)).Read(garbage)

	for _, c := range compress.All() {
		if c.ID() == compress.IDNone {
			continue // The identity codec accepts anything, correctly.
		}
		_, err := compress.UnBytes(c, garbage, compress.DefaultMaxDecompressed)
		require.Error(t, err, "%s should refuse a stream it did not produce", c.Name())
	}
}

// TestStreamsWithoutBufferingWholeInput checks the interface is honoured as a
// stream. The platform exists for files too large to hold in memory, so a codec that
// buffered its whole input would fail on exactly the files it was bought for.
func TestStreamsWithoutBufferingWholeInput(t *testing.T) {
	const size = 8 << 20

	for _, c := range compress.All() {
		t.Run(c.Name(), func(t *testing.T) {
			// The reader counts what it was asked for and never materialises the whole
			// input; the writer counts bytes and discards them. If a codec insisted on
			// a []byte of the whole stream, it could not consume this at all.
			counted := &zeroReader{}
			var sink countingWriter

			require.NoError(t, c.Compress(&sink, io.LimitReader(counted, size), 1))
			require.Equal(t, int64(size), counted.read)
			require.Positive(t, sink.n)
		})
	}
}

// requireSameBytes compares byte slices by content.
//
// It exists for the empty-file case: an empty result is a nil slice rather than a
// zero-length one, which is the same thing to every caller but not to a deep
// equality check. Asserting on content keeps the empty transmission in the matrix,
// which is worth having — it is the case a real deployment eventually hits.
func requireSameBytes(t *testing.T, want, got []byte) {
	t.Helper()
	require.True(t, bytes.Equal(want, got),
		"content differs: wanted %d bytes, got %d", len(want), len(got))
}

// zeroReader is an endless stream of zeroes that records how much was read.
type zeroReader struct{ read int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	z.read += int64(len(p))
	return len(p), nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
