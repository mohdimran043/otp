package protocol

import (
	"image"
	"image/color"
	"image/draw"

	"github.com/opticaltransport/otp/shared/internal/bitio"
)

// Colours for binary cells. The protocol renders bit 1 bright and bit 0 dark on a
// dark field: an emissive panel bleeds less into neighbouring cells with a dark
// background, and it keeps the quiet zone from washing out the finder rings.
var (
	// CellOn is the colour of a set binary cell.
	CellOn = color.RGBA{R: 255, G: 255, B: 255, A: 255}
	// CellOff is the colour of a clear binary cell, and of the quiet zone.
	CellOff = color.RGBA{R: 0, G: 0, B: 0, A: 255}
)

// Canvas is a cell-addressable render target for one frame.
type Canvas struct {
	Layout Layout
	img    *image.RGBA
}

// NewCanvas allocates a canvas filled with the background colour, so the quiet
// zone and any cell left unwritten are already correct.
func NewCanvas(l Layout) *Canvas {
	img := image.NewRGBA(l.Bounds())
	draw.Draw(img, img.Bounds(), &image.Uniform{CellOff}, image.Point{}, draw.Src)
	return &Canvas{Layout: l, img: img}
}

// Image returns the rendered frame.
func (c *Canvas) Image() *image.RGBA { return c.img }

// SetCell paints one whole cell.
func (c *Canvas) SetCell(cell Cell, col color.RGBA) {
	r := c.Layout.CellRect(cell)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		base := c.img.PixOffset(r.Min.X, y)
		row := c.img.Pix[base : base+4*(r.Max.X-r.Min.X)]
		for x := 0; x < len(row); x += 4 {
			row[x] = col.R
			row[x+1] = col.G
			row[x+2] = col.B
			row[x+3] = 255
		}
	}
}

// SetBit paints one cell in the binary palette.
func (c *Canvas) SetBit(cell Cell, on bool) {
	if on {
		c.SetCell(cell, CellOn)
	} else {
		c.SetCell(cell, CellOff)
	}
}

// DrawScaffold paints everything that does not depend on the payload: the four
// fiducials, their separators, the timing columns, and the grid descriptor beside
// each fiducial.
//
// Everything here is derived from the layout alone, which is what lets a decoder
// reconstruct it from geometry it has already measured.
func (c *Canvas) DrawScaffold() error {
	DrawAllFinders(c.Layout, c.SetBit)
	for _, t := range c.Layout.TimingCells() {
		c.SetBit(t.Cell, t.On)
	}
	return c.WriteDescriptors()
}

// WriteDescriptors paints the grid descriptor into all four corner blocks.
//
// The same record goes into every corner. Redundancy is cheap here — four blocks
// cost 224 cells out of tens of thousands — and it buys a great deal: a decoder
// only needs one corner to survive glare, a fingerprint on the lens, or a
// rolling-shutter tear to recover the geometry and go on to read the frame.
func (c *Canvas) WriteDescriptors() error {
	rec, err := DescriptorFor(c.Layout).MarshalBits()
	if err != nil {
		return err
	}
	for corner := 0; corner < 4; corner++ {
		cells := c.Layout.DescriptorCellsAt(corner)
		r := bitio.NewReaderLimit(rec, DescriptorBits)
		for i, cell := range cells {
			if i < DescriptorBits {
				b, err := r.ReadBit()
				if err != nil {
					return err
				}
				c.SetBit(cell, b)
				continue
			}
			// Deterministic filler in the few spare cells, so identical frames
			// render to identical images.
			c.SetBit(cell, i%2 == 0)
		}
	}
	return nil
}

// WriteHeaderBand renders the header into the top band, repeated as many times as
// the geometry allows.
func (c *Canvas) WriteHeaderBand(h Header) error {
	rec, err := h.MarshalBinary()
	if err != nil {
		return err
	}
	return c.writeBand(c.Layout.HeaderCells(), rec, c.Layout.HeaderCopies())
}

// WriteFooterBand renders the footer into the bottom band.
func (c *Canvas) WriteFooterBand(f Footer) error {
	rec, err := f.MarshalBinary()
	if err != nil {
		return err
	}
	return c.writeBand(c.Layout.FooterCells(), rec, c.Layout.FooterCopies())
}

// writeBand lays `copies` copies of a record across the given cells, then fills
// whatever is left with an alternating pattern.
//
// The filler is deterministic so that two encoders producing the same frame
// produce byte-identical images, which is what makes the golden-vector tests
// meaningful. Decoding ignores those cells.
func (c *Canvas) writeBand(cells []Cell, record []byte, copies int) error {
	bits := len(record) * 8
	if copies*bits > len(cells) {
		return ErrShortBuffer
	}
	idx := 0
	for k := 0; k < copies; k++ {
		r := bitio.NewReader(record)
		for i := 0; i < bits; i++ {
			b, err := r.ReadBit()
			if err != nil {
				return err
			}
			c.SetBit(cells[idx], b)
			idx++
		}
	}
	for ; idx < len(cells); idx++ {
		c.SetBit(cells[idx], idx%2 == 0)
	}
	return nil
}
