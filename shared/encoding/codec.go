package encoding

import (
	"fmt"
	"image"

	"github.com/opticaltransport/otp/shared/internal/bitio"
	"github.com/opticaltransport/otp/shared/protocol"
)

// plan is a modulation's arrangement of the payload region.
//
// Every encoding puts its payload in the same cells; what differs is the order
// they are visited and whether any auxiliary cells are reserved. Expressing that
// as a plan keeps one render-and-read implementation for all five encodings, so a
// change to the frame structure cannot leave one of them behind.
type plan struct {
	// cells are the payload cells, in write order.
	cells []protocol.Cell

	// finish writes any auxiliary cells after the payload has been rendered.
	finish func(c *protocol.Canvas, symbols []uint32) error

	// diagnose explains a failed frame by reading those auxiliary cells, and is
	// consulted only once the payload has failed its own integrity check.
	//
	// It deliberately cannot reject a frame. Auxiliary checksums are narrower than
	// the footer's CRC32 and SHA-256, so a frame that satisfies the footer is
	// intact whatever they say — and letting them veto it would throw away good
	// frames whose only damage was to the checksums themselves.
	diagnose func(s *protocol.Sampler, symbols []uint32) error
}

// codec is the shared implementation behind every registered encoder.
type codec struct {
	id          uint8
	name        string
	description string
	depths      []uint8
	defaultDep  uint8

	// palette resolves the modulation for a bit depth. Only the grey encoding
	// varies with depth; the others ignore it.
	palette func(bitDepth uint8) Palette

	// planner arranges the payload region at a known symbol width. The width is
	// passed in rather than derived, because a plan that reserves auxiliary cells
	// has to pack its own records at the same density as the payload it protects.
	planner func(l protocol.Layout, bitsPerCell int) plan
}

func (c *codec) ID() uint8                   { return c.id }
func (c *codec) Name() string                { return c.name }
func (c *codec) Description() string         { return c.description }
func (c *codec) DefaultBitDepth() uint8      { return c.defaultDep }
func (c *codec) SupportedBitDepths() []uint8 { return append([]uint8(nil), c.depths...) }

func (c *codec) supports(depth uint8) bool {
	for _, d := range c.depths {
		if d == depth {
			return true
		}
	}
	return false
}

func (c *codec) resolveDepth(depth uint8) (uint8, error) {
	if depth == 0 {
		return c.defaultDep, nil
	}
	if !c.supports(depth) {
		return 0, fmt.Errorf("%w: %s does not offer depth %d, only %v",
			ErrUnsupportedBitDepth, c.name, depth, c.depths)
	}
	return depth, nil
}

// EstimateCapacity reports what one frame carries at this geometry and depth.
func (c *codec) EstimateCapacity(l protocol.Layout, bitDepth uint8) (Capacity, error) {
	depth, err := c.resolveDepth(bitDepth)
	if err != nil {
		return Capacity{}, err
	}
	if err := l.Validate(); err != nil {
		return Capacity{}, err
	}

	bits := c.palette(depth).Bits()
	p := c.planner(l, bits)
	gridCells := l.GridWidth * l.GridHeight
	payloadBits := len(p.cells) * bits

	return Capacity{
		BitsPerCell:   bits,
		PayloadCells:  len(p.cells),
		PayloadBytes:  payloadBits / 8,
		GridCells:     gridCells,
		OverheadCells: gridCells - len(p.cells),
		Efficiency:    float64(payloadBits) / float64(gridCells*bits),
	}, nil
}

// Validate reports whether a frame can be rendered at this geometry and depth.
func (c *codec) Validate(f *protocol.Frame, l protocol.Layout, bitDepth uint8) error {
	if f == nil {
		return fmt.Errorf("encoding: nil frame")
	}
	cap, err := c.EstimateCapacity(l, bitDepth)
	if err != nil {
		return err
	}
	if len(f.Payload) > cap.PayloadBytes {
		return fmt.Errorf("%w: %d bytes exceeds the %d a %s frame carries at %dx%d",
			protocol.ErrPayloadTooLarge, len(f.Payload), cap.PayloadBytes,
			c.name, l.GridWidth, l.GridHeight)
	}
	if int(f.Header.PayloadLength) != len(f.Payload) {
		return fmt.Errorf("encoding: header declares %d payload bytes, frame carries %d",
			f.Header.PayloadLength, len(f.Payload))
	}
	return nil
}

// Encode renders a frame.
//
// The header fields describing the encoding and geometry are filled in here
// rather than left to the caller. A frame whose header disagreed with how it was
// actually rendered would decode to nonsense, and that is too easy a mistake to
// leave available.
func (c *codec) Encode(f *protocol.Frame, l protocol.Layout, bitDepth uint8) (*image.RGBA, error) {
	depth, err := c.resolveDepth(bitDepth)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(f, l, depth); err != nil {
		return nil, err
	}

	f.Header.Version = protocol.Current
	f.Header.EncoderID = c.id
	f.Header.BitDepth = depth
	f.Header.GridWidth = uint16(l.GridWidth)
	f.Header.GridHeight = uint16(l.GridHeight)
	f.Header.CellPixels = uint16(l.CellPixels)

	canvas := protocol.NewCanvas(l)
	if err := canvas.DrawScaffold(); err != nil {
		return nil, err
	}
	if err := canvas.WriteHeaderBand(f.Header); err != nil {
		return nil, err
	}
	if err := canvas.WriteFooterBand(f.Footer); err != nil {
		return nil, err
	}

	pal := c.palette(depth)
	p := c.planner(l, pal.Bits())
	symbols := packSymbols(f.Payload, pal.Bits(), len(p.cells))
	for i, cell := range p.cells {
		canvas.SetCell(cell, pal.Color(symbols[i]))
	}
	if p.finish != nil {
		if err := p.finish(canvas, symbols); err != nil {
			return nil, err
		}
	}
	return canvas.Image(), nil
}

// Decode recovers a frame from a captured image.
func (c *codec) Decode(img image.Image, opts protocol.LocateOptions) (*protocol.Frame, error) {
	g, err := protocol.Locate(img, opts)
	if err != nil {
		return nil, err
	}
	return decodeAt(c, g, img, opts)
}

// reading is one pass over a frame's payload region: the symbols as sampled, the
// bytes they unpack to, and the sampler and plan that produced them, which the
// plan's diagnostics need afterwards.
type reading struct {
	payload []byte
	symbols []uint32
	sampler *protocol.Sampler
	plan    plan
}

// read demodulates the payload region at an already-resolved geometry, without
// judging what it found. Decoding adds the integrity checks; diagnostics want the
// bytes as they actually came off the grid.
func (c *codec) read(g *protocol.Geometry, img image.Image) (*reading, error) {
	depth, err := c.resolveDepth(g.Header.BitDepth)
	if err != nil {
		return nil, err
	}

	s := protocol.NewSamplerAt(g, img)
	pal := c.palette(depth)
	p := c.planner(g.Layout, pal.Bits())

	// Sampled colours are normalised against the frame's own fiducials before they
	// are matched, so the palette is compared like for like rather than against
	// whatever exposure and optics the capture happened to use.
	ref := newReference(s, g.Layout)
	symbols := make([]uint32, len(p.cells))
	for i, cell := range p.cells {
		symbols[i] = pal.Value(ref.normalize(s.Color(cell), cell))
	}

	payload, err := unpackSymbols(symbols, pal.Bits(), int(g.Header.PayloadLength))
	if err != nil {
		return nil, err
	}
	return &reading{payload: payload, symbols: symbols, sampler: s, plan: p}, nil
}

// decodeAt reads a frame's payload at an already-resolved geometry. Splitting it
// out lets the package-level Decode locate once and then dispatch, rather than
// locating a second time inside the chosen encoder.
func decodeAt(e Encoder, g *protocol.Geometry, img image.Image, opts protocol.LocateOptions) (*protocol.Frame, error) {
	if g.Header.EncoderID != e.ID() {
		return nil, fmt.Errorf("%w: frame declares encoder %d, %s is %d",
			ErrEncoderMismatch, g.Header.EncoderID, e.Name(), e.ID())
	}
	c, ok := e.(*codec)
	if !ok {
		return nil, fmt.Errorf("encoding: %s does not support geometry reuse", e.Name())
	}

	s := protocol.NewSamplerAt(g, img)
	footer, err := s.ReadFooter()
	if err != nil {
		return nil, err
	}

	r, err := c.read(g, img)
	if err != nil {
		return nil, err
	}

	f := &protocol.Frame{Header: g.Header, Payload: r.payload, Footer: footer}
	if err := f.Verify(); err != nil {
		// A bare CRC mismatch says only that the frame is bad. Where the encoding
		// can say *where* it went bad, that is what the receiver records and what
		// makes a misaimed camera diagnosable from the logs.
		if r.plan.diagnose != nil {
			if d := r.plan.diagnose(r.sampler, r.symbols); d != nil {
				return nil, fmt.Errorf("%w (%w)", err, d)
			}
		}
		return nil, err
	}
	return f, nil
}

// packSymbols splits payload bytes into fixed-width symbols, padding the
// remaining cells deterministically.
//
// The padding has to be deterministic so that rendering the same frame twice
// produces byte-identical images, which is what makes the golden-vector tests
// meaningful. Its value is never read back: the header states the payload length,
// so decoding stops at the last real byte.
func packSymbols(payload []byte, bitsPerCell, cells int) []uint32 {
	out := make([]uint32, cells)
	r := bitio.NewReader(payload)
	i := 0
	for ; i < cells && r.Remaining() >= bitsPerCell; i++ {
		v, err := r.ReadBits(bitsPerCell)
		if err != nil {
			break
		}
		out[i] = v
	}
	// A payload whose bit length is not a multiple of the symbol width leaves a
	// partial symbol; pad it with zeroes on the right.
	if i < cells && r.Remaining() > 0 {
		rem := r.Remaining()
		v, err := r.ReadBits(rem)
		if err == nil {
			out[i] = v << uint(bitsPerCell-rem)
			i++
		}
	}
	max := uint32(1<<uint(bitsPerCell)) - 1
	for ; i < cells; i++ {
		out[i] = uint32(i) & max
	}
	return out
}

// unpackSymbols reassembles payload bytes from symbols.
func unpackSymbols(symbols []uint32, bitsPerCell, payloadLen int) ([]byte, error) {
	if payloadLen < 0 || payloadLen > len(symbols)*bitsPerCell/8 {
		return nil, fmt.Errorf("%w: header declares %d payload bytes, the grid holds at most %d",
			protocol.ErrPayloadTooLarge, payloadLen, len(symbols)*bitsPerCell/8)
	}
	w := bitio.NewWriter(len(symbols) * bitsPerCell)
	for _, v := range symbols {
		if err := w.WriteBits(v, bitsPerCell); err != nil {
			return nil, err
		}
	}
	return w.Bytes()[:payloadLen], nil
}

// rowMajorPlan is the default arrangement: payload cells in reading order, with
// no auxiliary cells reserved.
func rowMajorPlan(l protocol.Layout, _ int) plan {
	return plan{cells: l.PayloadCells()}
}
