package encoding

import (
	"fmt"
	"image"

	"github.com/opticaltransport/otp/shared/protocol"
)

// SoftCell is one payload cell as the sampler actually read it: the symbol chosen,
// the symbol that came second, and how far apart the two were.
type SoftCell struct {
	// Index is the cell's position in the payload symbol sequence, which is the
	// plan's write order and not any grid order. It is the index a caller
	// substitutes at, and getting this wrong on the rolling encoding — whose plan
	// interleaves across bands — would corrupt a frame while appearing to correct it.
	Index int

	// Cell is where it sits on the grid. Carried for diagnostics: marginal cells
	// clustered in one corner mean lens falloff or glare, and marginal cells spread
	// evenly mean focus.
	Cell protocol.Cell

	// Symbol and Second are the nearest and second-nearest palette entries.
	Symbol, Second uint32

	// Margin is the distance between them. See Palette.ValueWithMargin.
	Margin float64
}

// SoftReading is a frame's payload region demodulated with its confidence retained,
// together with everything needed to test a corrected symbol sequence without
// touching the image again.
//
// That last part is the point. A candidate search tries thousands of sequences, and
// if each one re-sampled the image it would cost thousands of homography evaluations
// per frame. Sampling once and keeping the result makes the search arithmetic rather
// than optical.
type SoftReading struct {
	// Symbols are the payload symbols as read, index-aligned with Cells.
	Symbols []uint32

	// Cells carry the per-cell confidence, in the same order as Symbols.
	Cells []SoftCell

	// Palette and BitsPerCell describe the modulation, so a caller can size its own
	// work without resolving the encoder a second time.
	Palette     Palette
	BitsPerCell int

	// header and footer are the records this reading was taken against. The footer is
	// the oracle Verify checks candidates with, so it is read once here — and a footer
	// that cannot be read means no candidate can ever be confirmed, which is why
	// SoftRead fails on it rather than leaving a search to discover it the expensive way.
	header     protocol.Header
	footer     protocol.Footer
	payloadLen int
}

// SoftRead demodulates a frame's payload at an already-resolved geometry, keeping the
// confidence of every cell.
//
// It is the same read decodeAt performs — same sampler, same photometric reference
// fitted from the frame's own fiducials and timing columns, same plan — with the
// margins retained instead of discarded. Sharing that machinery rather than
// reimplementing it is deliberate: the plan differs per encoding and the reference is
// what makes palette matching valid at all, so a second implementation would drift
// from the decoder and "recover" frames into wrong bytes.
func SoftRead(g *protocol.Geometry, img image.Image) (*SoftReading, error) {
	if g == nil {
		return nil, fmt.Errorf("encoding: soft read needs a resolved geometry")
	}
	c, err := codecFor(g)
	if err != nil {
		return nil, err
	}
	depth, err := c.resolveDepth(g.Header.BitDepth)
	if err != nil {
		return nil, err
	}

	s := protocol.NewSamplerAt(g, img)
	footer, err := s.ReadFooter()
	if err != nil {
		return nil, fmt.Errorf("encoding: soft read cannot verify anything without the footer: %w", err)
	}

	pal := c.palette(depth)
	p := c.planner(g.Layout, pal.Bits())
	ref := newReference(s, g.Layout)

	r := &SoftReading{
		Symbols:     make([]uint32, len(p.cells)),
		Cells:       make([]SoftCell, len(p.cells)),
		Palette:     pal,
		BitsPerCell: pal.Bits(),
		header:      g.Header,
		footer:      footer,
		payloadLen:  int(g.Header.PayloadLength),
	}
	for i, cell := range p.cells {
		best, second, margin := pal.ValueWithMargin(ref.normalize(s.Color(cell), cell))
		r.Symbols[i] = best
		r.Cells[i] = SoftCell{Index: i, Cell: cell, Symbol: best, Second: second, Margin: margin}
	}
	return r, nil
}

// Verify unpacks a candidate symbol sequence and checks it against the footer this
// reading was taken with.
//
// It touches no pixels, which is what makes a search over thousands of candidates
// affordable. A sequence that satisfies both the footer's CRC32 and its SHA-256 is the
// frame: a false accept would need a simultaneous collision in both, so a caller may
// treat success as final rather than as a hypothesis.
func (r *SoftReading) Verify(symbols []uint32) (*protocol.Frame, error) {
	if len(symbols) != len(r.Symbols) {
		return nil, fmt.Errorf("encoding: %d candidate symbols for a %d-cell payload region",
			len(symbols), len(r.Symbols))
	}
	payload, err := unpackSymbols(symbols, r.BitsPerCell, r.payloadLen)
	if err != nil {
		return nil, err
	}
	f := &protocol.Frame{Header: r.header, Payload: payload, Footer: r.footer}
	if err := f.Verify(); err != nil {
		return nil, err
	}
	return f, nil
}

// codecFor resolves the codec a located frame declares in its header.
func codecFor(g *protocol.Geometry) (*codec, error) {
	e, err := ByID(g.Header.EncoderID)
	if err != nil {
		return nil, err
	}
	c, ok := e.(*codec)
	if !ok {
		return nil, fmt.Errorf("encoding: %s does not support soft reading", e.Name())
	}
	return c, nil
}
