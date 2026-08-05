package compress

import (
	"compress/gzip"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// The five registered compressors.
//
// Every codec here takes a level in 1..9, because a configuration file and a
// profile UI need one scale rather than four. None of the underlying libraries uses
// that scale: gzip happens to, zstd has four named speeds, brotli has twelve
// levels, and lz4 has a fast mode plus nine high-compression ones. Each codec below
// maps 1..9 onto whatever it really has, so an operator who sets level 7 gets
// "near the slow end" from all of them.
var (
	// None is the identity codec, for data that is already compressed. It is not a
	// placeholder: sending an already-compressed archive through a second
	// compressor costs time and usually makes it slightly larger.
	None = &codec{
		id:          IDNone,
		name:        "none",
		description: "No compression. The right choice for archives and media that are already compressed.",
		compress: func(dst io.Writer, src io.Reader, _ int) error {
			_, err := io.Copy(dst, src)
			return err
		},
		decompress: func(dst io.Writer, src io.Reader, limit int64) error {
			return copyLimited(dst, src, limit)
		},
	}

	// Gzip is the conservative choice: modest ratio, modest speed, and readable by
	// anything, which matters when a receiver's output is handed to other tools.
	Gzip = &codec{
		id:          IDGzip,
		name:        "gzip",
		description: "Deflate. Moderate ratio and speed, and universally readable.",
		compress: func(dst io.Writer, src io.Reader, level int) error {
			resolved := gzip.DefaultCompression
			if level > 0 {
				resolved = level // gzip's own scale is already 1..9.
			}
			w, err := gzip.NewWriterLevel(dst, resolved)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, src); err != nil {
				w.Close()
				return err
			}
			return w.Close()
		},
		decompress: func(dst io.Writer, src io.Reader, limit int64) error {
			r, err := gzip.NewReader(src)
			if err != nil {
				return err
			}
			defer r.Close()
			return copyLimited(dst, r, limit)
		},
	}

	// LZ4 is the fast one. Its ratio is the weakest here, but it compresses far
	// faster than the optical channel can drain, which makes it the choice when the
	// sender is displaying frames as it produces them rather than preparing them
	// ahead of time.
	LZ4 = &codec{
		id:          IDLZ4,
		name:        "lz4",
		description: "LZ4. The fastest codec here, at the weakest ratio.",
		compress: func(dst io.Writer, src io.Reader, level int) error {
			w := lz4.NewWriter(dst)
			if level > 0 {
				if err := w.Apply(lz4.CompressionLevelOption(lz4Levels[level-1])); err != nil {
					return err
				}
			}
			if _, err := io.Copy(w, src); err != nil {
				w.Close()
				return err
			}
			return w.Close()
		},
		decompress: func(dst io.Writer, src io.Reader, limit int64) error {
			return copyLimited(dst, lz4.NewReader(src), limit)
		},
	}

	// Zstd is the default recommendation, and the reason is the shape of its
	// trade-off rather than any single figure: it reaches within a few percent of
	// brotli's ratio at several times the speed, so on a channel this slow it is
	// almost never the bottleneck.
	Zstd = &codec{
		id:          IDZstd,
		name:        "zstd",
		description: "Zstandard. Near-brotli ratio at several times the speed; the default recommendation.",
		compress: func(dst io.Writer, src io.Reader, level int) error {
			opts := []zstd.EOption{}
			if level > 0 {
				opts = append(opts, zstd.WithEncoderLevel(zstdLevels[level-1]))
			}
			w, err := zstd.NewWriter(dst, opts...)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, src); err != nil {
				w.Close()
				return err
			}
			return w.Close()
		},
		decompress: func(dst io.Writer, src io.Reader, limit int64) error {
			r, err := zstd.NewReader(src)
			if err != nil {
				return err
			}
			defer r.Close()
			return copyLimited(dst, r, limit)
		},
	}

	// Brotli is the smallest output, and the slowest to produce it. On an optical
	// channel that is often the right trade: the sender has time to spare while the
	// display works through the frames it has already been given, and every percent
	// off the size is a percent off the transmission.
	Brotli = &codec{
		id:          IDBrotli,
		name:        "brotli",
		description: "Brotli. The smallest output here, and the slowest to produce.",
		compress: func(dst io.Writer, src io.Reader, level int) error {
			resolved := brotli.DefaultCompression
			if level > 0 {
				resolved = brotliLevels[level-1]
			}
			w := brotli.NewWriterLevel(dst, resolved)
			if _, err := io.Copy(w, src); err != nil {
				w.Close()
				return err
			}
			return w.Close()
		},
		decompress: func(dst io.Writer, src io.Reader, limit int64) error {
			return copyLimited(dst, brotli.NewReader(src), limit)
		},
	}
)

// Level maps for the codecs whose own scale is not 1..9. Each is indexed by
// level-1, so the tables are read straight across from the common scale.
var (
	// lz4's fast mode is genuinely different in kind from its high-compression
	// levels, so level 1 selects it and the rest walk up the HC range.
	lz4Levels = [9]lz4.CompressionLevel{
		lz4.Fast,
		lz4.Level1, lz4.Level2, lz4.Level3, lz4.Level4,
		lz4.Level5, lz4.Level6, lz4.Level7, lz4.Level8,
	}

	// zstd exposes four speeds rather than nine levels, so the common scale is
	// divided between them.
	zstdLevels = [9]zstd.EncoderLevel{
		zstd.SpeedFastest, zstd.SpeedFastest,
		zstd.SpeedDefault, zstd.SpeedDefault, zstd.SpeedDefault,
		zstd.SpeedBetterCompression, zstd.SpeedBetterCompression,
		zstd.SpeedBestCompression, zstd.SpeedBestCompression,
	}

	// brotli has twelve levels; 9 maps to its maximum so the common scale can reach
	// the smallest output the codec can produce.
	brotliLevels = [9]int{1, 2, 3, 4, 6, 7, 8, 9, 11}
)

// codec is the shared implementation behind every registered compressor. The
// codecs differ only in the two functions, so there is one place where levels are
// validated and one place where the output limit is enforced.
type codec struct {
	id          uint8
	name        string
	description string
	compress    func(dst io.Writer, src io.Reader, level int) error
	decompress  func(dst io.Writer, src io.Reader, limit int64) error
}

func (c *codec) ID() uint8           { return c.id }
func (c *codec) Name() string        { return c.name }
func (c *codec) Description() string { return c.description }

// Compress validates the level before dispatching.
//
// The check happens here rather than in each codec so that every compressor
// refuses the same levels, including the identity codec, which has nothing to do
// with a level at all. An operator who mistypes one while compression is set to
// "none" should hear about it then, not weeks later when the profile is switched to
// zstd and the same configuration suddenly fails.
func (c *codec) Compress(dst io.Writer, src io.Reader, level int) error {
	if err := checkLevel(level); err != nil {
		return err
	}
	return c.compress(dst, src, level)
}

func (c *codec) Decompress(dst io.Writer, src io.Reader, limit int64) error {
	return c.decompress(dst, src, limit)
}

func init() {
	for _, c := range []Compressor{None, Gzip, LZ4, Zstd, Brotli} {
		Register(c)
	}
}
