package protocol

import (
	"fmt"
	"image"
)

// Grid geometry constants.
const (
	// FinderPattern is the side length, in cells, of a corner finder's
	// concentric-square pattern.
	FinderPattern = 7

	// FinderBox is the side length, in cells, of the area a finder reserves:
	// the pattern plus a one-cell separator on its inner edges. Nothing else may
	// be drawn inside a finder box, or finder detection loses its contrast.
	FinderBox = 8

	// BandRepeat is the *minimum* number of times the header and footer records
	// are written into their bands. Decoding majority-votes across the copies, so
	// any bit corrupted in a minority of them is repaired. It must stay odd,
	// otherwise a bit position could tie.
	//
	// Bands are sized to guarantee at least this many copies. Wide grids leave
	// spare room in the eight-row minimum band, and HeaderCopies reclaims it by
	// writing as many further copies as fit — see that method.
	BandRepeat = 3

	// DefaultQuietZone is the margin, in cells, of background left around the
	// grid. Finder detection needs unbroken background on the outer edges.
	DefaultQuietZone = 2

	// DefaultGridWidth and DefaultGridHeight give a 16:9 grid that fits a 1080p
	// display at DefaultCellPixels with room for the quiet zone.
	DefaultGridWidth  = 192
	DefaultGridHeight = 108

	// DefaultCellPixels is the on-screen size of one cell. Larger cells survive
	// a worse lens and a longer throw; smaller cells carry more data per frame.
	DefaultCellPixels = 8

	// MinGridDimension is the smallest grid side the layout permits. Below this
	// the fixed bands crowd out the payload entirely.
	MinGridDimension = 48

	// MaxGridDimension bounds the grid so a malformed header cannot make the
	// decoder allocate an enormous buffer.
	MaxGridDimension = 4096

	// MaxCellPixels bounds cell size for the same reason.
	MaxCellPixels = 256
)

// Cell is a grid coordinate, in cells, with the origin at the top-left of the
// grid proper — the quiet zone is outside this coordinate space.
type Cell struct {
	X, Y int
}

// Layout is the resolved geometry of a frame. Every field is derived from the
// grid dimensions, so an encoder and a decoder that agree on width, height, and
// cell size agree on the position of every cell without exchanging anything else.
type Layout struct {
	GridWidth      int
	GridHeight     int
	CellPixels     int
	QuietZone      int
	HeaderBandRows int
	FooterBandRows int
	BandRepeat     int
}

// NewLayout resolves the geometry for a grid, sizing the header and footer bands
// to hold BandRepeat copies of their records.
func NewLayout(gridWidth, gridHeight, cellPixels int) (Layout, error) {
	return NewLayoutQuiet(gridWidth, gridHeight, cellPixels, DefaultQuietZone)
}

// NewLayoutQuiet is NewLayout with an explicit quiet-zone width.
func NewLayoutQuiet(gridWidth, gridHeight, cellPixels, quietZone int) (Layout, error) {
	if gridWidth < MinGridDimension || gridWidth > MaxGridDimension {
		return Layout{}, fmt.Errorf("%w: width %d outside %d..%d",
			ErrGridBounds, gridWidth, MinGridDimension, MaxGridDimension)
	}
	if gridHeight < MinGridDimension || gridHeight > MaxGridDimension {
		return Layout{}, fmt.Errorf("%w: height %d outside %d..%d",
			ErrGridBounds, gridHeight, MinGridDimension, MaxGridDimension)
	}
	if cellPixels < 1 || cellPixels > MaxCellPixels {
		return Layout{}, fmt.Errorf("%w: cell size %d outside 1..%d",
			ErrGridBounds, cellPixels, MaxCellPixels)
	}
	if quietZone < 0 || quietZone > 64 {
		return Layout{}, fmt.Errorf("%w: quiet zone %d outside 0..64", ErrGridBounds, quietZone)
	}

	headerRows, err := bandRows(gridWidth, HeaderSize, BandRepeat)
	if err != nil {
		return Layout{}, fmt.Errorf("header band: %w", err)
	}
	footerRows, err := bandRows(gridWidth, FooterSize, BandRepeat)
	if err != nil {
		return Layout{}, fmt.Errorf("footer band: %w", err)
	}

	l := Layout{
		GridWidth:      gridWidth,
		GridHeight:     gridHeight,
		CellPixels:     cellPixels,
		QuietZone:      quietZone,
		HeaderBandRows: headerRows,
		FooterBandRows: footerRows,
		BandRepeat:     BandRepeat,
	}
	if err := l.Validate(); err != nil {
		return Layout{}, err
	}
	return l, nil
}

// LayoutFor resolves the geometry a header describes. The receiver uses it to
// reproduce the sender's layout exactly from the decoded header.
func LayoutFor(h Header) (Layout, error) {
	return NewLayout(int(h.GridWidth), int(h.GridHeight), int(h.CellPixels))
}

// bandExcludedCells is how many cells in a band are unavailable to its record:
// two finder boxes and the two descriptor blocks beside them.
const bandExcludedCells = 2*FinderBox*FinderBox + 2*DescriptorCells

// bandRows returns the smallest band height, in rows, that can hold `repeat`
// copies of a record of `recordBytes` bytes once the finder boxes and descriptor
// blocks in that band are excluded.
func bandRows(gridWidth, recordBytes, repeat int) (int, error) {
	need := recordBytes * 8 * repeat
	for rows := FinderBox; rows <= MaxGridDimension; rows++ {
		if rows*gridWidth-bandExcludedCells >= need {
			return rows, nil
		}
	}
	return 0, fmt.Errorf("%w: no band height up to %d rows holds %d bits at width %d",
		ErrGridTooSmall, MaxGridDimension, need, gridWidth)
}

// Validate reports whether the geometry leaves a usable payload region.
func (l Layout) Validate() error {
	if l.GridWidth < 2*(FinderBox+DescriptorCols)+2 {
		return fmt.Errorf("%w: width %d cannot hold two finder boxes, two descriptor blocks, and the timing columns",
			ErrGridTooSmall, l.GridWidth)
	}
	if l.HeaderBandRows < DescriptorRows || l.FooterBandRows < DescriptorRows {
		return fmt.Errorf("%w: bands of %d and %d rows cannot hold a %d-row descriptor block",
			ErrGridTooSmall, l.HeaderBandRows, l.FooterBandRows, DescriptorRows)
	}
	payloadRows := l.GridHeight - l.HeaderBandRows - l.FooterBandRows
	if payloadRows < 1 {
		return fmt.Errorf("%w: height %d leaves %d payload rows after a %d-row header band and a %d-row footer band",
			ErrGridTooSmall, l.GridHeight, payloadRows, l.HeaderBandRows, l.FooterBandRows)
	}
	if l.BandRepeat%2 == 0 {
		return fmt.Errorf("%w: band repeat %d must be odd", ErrGridBounds, l.BandRepeat)
	}
	return nil
}

// ImageWidth is the rendered frame width in pixels, including the quiet zone.
func (l Layout) ImageWidth() int { return (l.GridWidth + 2*l.QuietZone) * l.CellPixels }

// ImageHeight is the rendered frame height in pixels, including the quiet zone.
func (l Layout) ImageHeight() int { return (l.GridHeight + 2*l.QuietZone) * l.CellPixels }

// Bounds is the rendered frame rectangle.
func (l Layout) Bounds() image.Rectangle {
	return image.Rect(0, 0, l.ImageWidth(), l.ImageHeight())
}

// CellRect is the pixel rectangle covering one grid cell.
func (l Layout) CellRect(c Cell) image.Rectangle {
	x0 := (c.X + l.QuietZone) * l.CellPixels
	y0 := (c.Y + l.QuietZone) * l.CellPixels
	return image.Rect(x0, y0, x0+l.CellPixels, y0+l.CellPixels)
}

// CellCenter is the pixel centre of a cell, in fractional coordinates.
func (l Layout) CellCenter(c Cell) (x, y float64) {
	return float64((c.X+l.QuietZone))*float64(l.CellPixels) + float64(l.CellPixels)/2,
		float64((c.Y+l.QuietZone))*float64(l.CellPixels) + float64(l.CellPixels)/2
}

// finderOrigin returns the top-left cell of finder box i, ordered
// top-left, top-right, bottom-left, bottom-right.
func (l Layout) finderOrigin(i int) Cell {
	switch i {
	case 0:
		return Cell{0, 0}
	case 1:
		return Cell{l.GridWidth - FinderBox, 0}
	case 2:
		return Cell{0, l.GridHeight - FinderBox}
	default:
		return Cell{l.GridWidth - FinderBox, l.GridHeight - FinderBox}
	}
}

// FinderOrigins returns the top-left cell of each finder's 7x7 pattern, ordered
// top-left, top-right, bottom-left, bottom-right.
//
// The pattern sits at the outer corner of its box and the separator column or
// row falls on the inner side, which is why the top-right pattern starts one
// cell further right than its box.
func (l Layout) FinderOrigins() [4]Cell {
	return [4]Cell{
		{0, 0},
		{l.GridWidth - FinderPattern, 0},
		{0, l.GridHeight - FinderPattern},
		{l.GridWidth - FinderPattern, l.GridHeight - FinderPattern},
	}
}

// FinderCenters returns the pattern centres in cell coordinates, in the same
// order as FinderOrigins. These four points are the correspondences the decoder
// feeds into its homography.
func (l Layout) FinderCenters() [4][2]float64 {
	const half = FinderPattern / 2.0
	o := l.FinderOrigins()
	var out [4][2]float64
	for i, c := range o {
		out[i] = [2]float64{float64(c.X) + half, float64(c.Y) + half}
	}
	return out
}

// inFinderBox reports whether a cell falls inside any reserved finder box.
func (l Layout) inFinderBox(c Cell) bool {
	for i := 0; i < 4; i++ {
		o := l.finderOrigin(i)
		if c.X >= o.X && c.X < o.X+FinderBox && c.Y >= o.Y && c.Y < o.Y+FinderBox {
			return true
		}
	}
	return false
}

// DescriptorOrigin returns the top-left cell of corner i's descriptor block.
//
// Each block sits immediately inward of its fiducial, hard against the grid edge.
// Keeping it within thirteen cells of the fiducial centre is what makes it
// readable from that fiducial's local frame: over so short a span, the difference
// between the true projective mapping and a straight affine one stays well under
// a single cell even on a sharply angled capture.
func (l Layout) DescriptorOrigin(corner int) Cell {
	switch corner {
	case 0: // top-left, extending right
		return Cell{FinderBox, 0}
	case 1: // top-right, extending left
		return Cell{l.GridWidth - FinderBox - DescriptorCols, 0}
	case 2: // bottom-left
		return Cell{FinderBox, l.GridHeight - DescriptorRows}
	default: // bottom-right
		return Cell{l.GridWidth - FinderBox - DescriptorCols, l.GridHeight - DescriptorRows}
	}
}

// DescriptorCellsAt lists corner i's descriptor cells in write order.
//
// The order is defined in the corner's *own* frame — first axis running inward
// horizontally, second running inward vertically — not in grid coordinates. That
// is what lets a decoder read the block: it has located the fiducial and knows
// which way is inward, but it does not yet know the grid width, so it cannot
// convert to grid coordinates at all. Ordering grid-major instead would reverse
// the bit sequence at three of the four corners.
func (l Layout) DescriptorCellsAt(corner int) []Cell {
	right := corner == 1 || corner == 3
	bottom := corner == 2 || corner == 3

	out := make([]Cell, 0, DescriptorCells)
	for b := 0; b < DescriptorRows; b++ {
		for a := 0; a < DescriptorCols; a++ {
			x := FinderBox + a
			if right {
				x = l.GridWidth - 1 - FinderBox - a
			}
			y := b
			if bottom {
				y = l.GridHeight - 1 - b
			}
			out = append(out, Cell{x, y})
		}
	}
	return out
}

// inDescriptorBlock reports whether a cell belongs to any descriptor block.
func (l Layout) inDescriptorBlock(c Cell) bool {
	for i := 0; i < 4; i++ {
		o := l.DescriptorOrigin(i)
		if c.X >= o.X && c.X < o.X+DescriptorCols && c.Y >= o.Y && c.Y < o.Y+DescriptorRows {
			return true
		}
	}
	return false
}

// reserved reports whether a cell is claimed by a fiducial or a descriptor block,
// and so unavailable to the header and footer records.
func (l Layout) reserved(c Cell) bool {
	return l.inFinderBox(c) || l.inDescriptorBlock(c)
}

// HeaderCells lists the cells carrying the header band, in write order.
func (l Layout) HeaderCells() []Cell {
	out := make([]Cell, 0, l.HeaderBandRows*l.GridWidth)
	for y := 0; y < l.HeaderBandRows; y++ {
		for x := 0; x < l.GridWidth; x++ {
			c := Cell{x, y}
			if l.reserved(c) {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// FooterCells lists the cells carrying the footer band, in write order.
func (l Layout) FooterCells() []Cell {
	out := make([]Cell, 0, l.FooterBandRows*l.GridWidth)
	for y := l.GridHeight - l.FooterBandRows; y < l.GridHeight; y++ {
		for x := 0; x < l.GridWidth; x++ {
			c := Cell{x, y}
			if l.reserved(c) {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// PayloadRect is the half-open cell rectangle available to the payload. The
// first and last columns are excluded because they carry the timing pattern.
func (l Layout) PayloadRect() image.Rectangle {
	return image.Rect(1, l.HeaderBandRows, l.GridWidth-1, l.GridHeight-l.FooterBandRows)
}

// PayloadRows is the number of cell rows available to the payload.
func (l Layout) PayloadRows() int { return l.GridHeight - l.HeaderBandRows - l.FooterBandRows }

// PayloadCols is the number of cell columns available to the payload.
func (l Layout) PayloadCols() int { return l.GridWidth - 2 }

// PayloadCellCount is how many cells the payload region holds.
func (l Layout) PayloadCellCount() int { return l.PayloadRows() * l.PayloadCols() }

// PayloadCells lists the payload cells in row-major order. Modulators that want
// a different traversal — the rolling-shutter encoder interleaves across bands —
// permute this slice rather than recomputing the region.
func (l Layout) PayloadCells() []Cell {
	r := l.PayloadRect()
	out := make([]Cell, 0, l.PayloadCellCount())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			out = append(out, Cell{x, y})
		}
	}
	return out
}

// TimingCells lists the alternating cells in the first and last payload-band
// columns, paired with the value each should carry. The decoder scores them to
// confirm its homography before trusting the payload: a homography that is off
// by a cell scores near zero here, which is a far cheaper check than discovering
// the error through a failed payload CRC.
func (l Layout) TimingCells() []struct {
	Cell Cell
	On   bool
} {
	r := l.PayloadRect()
	out := make([]struct {
		Cell Cell
		On   bool
	}, 0, 2*l.PayloadRows())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		on := y%2 == 0
		out = append(out,
			struct {
				Cell Cell
				On   bool
			}{Cell{0, y}, on},
			struct {
				Cell Cell
				On   bool
			}{Cell{l.GridWidth - 1, y}, !on},
		)
	}
	return out
}

// HeaderBandCapacityBits is how many bits the header band can carry.
func (l Layout) HeaderBandCapacityBits() int { return len(l.HeaderCells()) }

// FooterBandCapacityBits is how many bits the footer band can carry.
func (l Layout) FooterBandCapacityBits() int { return len(l.FooterCells()) }

// oddCopies returns the largest odd copy count that fits, never below one.
func oddCopies(capacityBits, recordBytes int) int {
	n := capacityBits / (recordBytes * 8)
	if n < 1 {
		return 1
	}
	if n%2 == 0 {
		n--
	}
	return n
}

// HeaderCopies is how many copies of the header this layout writes.
//
// A band can never be shorter than FinderBox rows, since it has to contain two
// finder boxes. On a wide grid that floor leaves capacity spare, and spending it
// on extra copies costs nothing: a 512-cell-wide grid fits five copies rather
// than three, so the header survives two independently corrupted copies instead
// of one. The count is derived purely from the geometry, so an encoder and a
// decoder that agree on the grid agree on the copy count without negotiating.
func (l Layout) HeaderCopies() int {
	return oddCopies(l.HeaderBandCapacityBits(), HeaderSize)
}

// FooterCopies is how many copies of the footer this layout writes.
func (l Layout) FooterCopies() int {
	return oddCopies(l.FooterBandCapacityBits(), FooterSize)
}

// String renders the geometry for logs and the diagnostics UI.
func (l Layout) String() string {
	return fmt.Sprintf("grid %dx%d cells @%dpx (%dx%dpx) header %d rows, footer %d rows, payload %dx%d = %d cells",
		l.GridWidth, l.GridHeight, l.CellPixels, l.ImageWidth(), l.ImageHeight(),
		l.HeaderBandRows, l.FooterBandRows, l.PayloadCols(), l.PayloadRows(), l.PayloadCellCount())
}
