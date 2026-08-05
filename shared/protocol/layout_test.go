package protocol

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultLayout(t *testing.T) {
	l, err := NewLayout(DefaultGridWidth, DefaultGridHeight, DefaultCellPixels)
	require.NoError(t, err)

	assert.Equal(t, (DefaultGridWidth+2*DefaultQuietZone)*DefaultCellPixels, l.ImageWidth())
	assert.Equal(t, (DefaultGridHeight+2*DefaultQuietZone)*DefaultCellPixels, l.ImageHeight())
	assert.LessOrEqual(t, l.ImageWidth(), 1920, "default layout should fit a 1080p display")
	assert.LessOrEqual(t, l.ImageHeight(), 1080, "default layout should fit a 1080p display")
	assert.Positive(t, l.PayloadCellCount())
	t.Log(l.String())
}

// The bands must hold BandRepeat copies of their records, because decoding
// majority-votes across those copies to repair damaged bits.
func TestBandsHoldEveryRepeat(t *testing.T) {
	for _, w := range []int{48, 64, 96, 128, 192, 256, 512, 1024} {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			l, err := NewLayout(w, 512, 4)
			require.NoError(t, err)

			assert.GreaterOrEqual(t, l.HeaderBandCapacityBits(), HeaderSize*8*BandRepeat,
				"header band must fit at least %d copies", BandRepeat)
			assert.GreaterOrEqual(t, l.FooterBandCapacityBits(), FooterSize*8*BandRepeat,
				"footer band must fit at least %d copies", BandRepeat)

			// Voting needs an odd count, and the guaranteed minimum must hold.
			assert.Equal(t, 1, l.HeaderCopies()%2, "header copy count must be odd")
			assert.Equal(t, 1, l.FooterCopies()%2, "footer copy count must be odd")
			assert.GreaterOrEqual(t, l.HeaderCopies(), BandRepeat)
			assert.GreaterOrEqual(t, l.FooterCopies(), BandRepeat)

			// Every copy the layout claims must actually fit.
			assert.LessOrEqual(t, l.HeaderCopies()*HeaderSize*8, l.HeaderBandCapacityBits())
			assert.LessOrEqual(t, l.FooterCopies()*FooterSize*8, l.FooterBandCapacityBits())

			// The band must be no taller than necessary, or every frame wastes
			// payload capacity on redundant header space. Bands bottom out at
			// FinderBox rows because they have to contain two finder boxes.
			if l.HeaderBandRows > FinderBox {
				assert.Less(t, (l.HeaderBandRows-1)*w-bandExcludedCells, HeaderSize*8*BandRepeat,
					"header band is one row taller than it needs to be")
			}
			if l.FooterBandRows > FinderBox {
				assert.Less(t, (l.FooterBandRows-1)*w-bandExcludedCells, FooterSize*8*BandRepeat,
					"footer band is one row taller than it needs to be")
			}
		})
	}
}

// Wide grids hit the eight-row band floor with capacity to spare, and that spare
// room should buy extra redundancy rather than go unused.
func TestWideGridsEarnExtraHeaderCopies(t *testing.T) {
	narrow, err := NewLayout(192, 512, 4)
	require.NoError(t, err)
	wide, err := NewLayout(1024, 512, 4)
	require.NoError(t, err)

	assert.Equal(t, FinderBox, wide.HeaderBandRows, "a wide grid should sit at the band floor")
	assert.Greater(t, wide.HeaderCopies(), narrow.HeaderCopies(),
		"spare band capacity on a wide grid should become extra header copies")
}

func TestBandRepeatIsOdd(t *testing.T) {
	assert.Equal(t, 1, BandRepeat%2, "majority voting requires an odd repeat count")
}

func TestLayoutRejectsBadGeometry(t *testing.T) {
	cases := []struct {
		name              string
		w, h, cell, quiet int
	}{
		{"width below minimum", MinGridDimension - 1, 128, 8, 2},
		{"height below minimum", 128, MinGridDimension - 1, 8, 2},
		{"width above maximum", MaxGridDimension + 1, 128, 8, 2},
		{"zero cell size", 128, 128, 0, 2},
		{"cell size above maximum", 128, 128, MaxCellPixels + 1, 2},
		{"negative quiet zone", 128, 128, 8, -1},
		// A 48-wide grid needs 45 header rows and 26 footer rows, which is more
		// than a 48-tall grid has.
		{"bands crowd out payload", 48, 48, 8, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLayoutQuiet(tc.w, tc.h, tc.cell, tc.quiet)
			assert.Error(t, err)
		})
	}
}

// Every cell must belong to exactly one region. An overlap would mean two
// writers fighting over a cell; a gap would mean wasted capacity.
func TestRegionsPartitionTheGrid(t *testing.T) {
	l, err := NewLayout(DefaultGridWidth, DefaultGridHeight, DefaultCellPixels)
	require.NoError(t, err)

	owner := make(map[Cell]string)
	claim := func(c Cell, who string) {
		if prev, dup := owner[c]; dup {
			t.Fatalf("cell %v claimed by both %s and %s", c, prev, who)
		}
		owner[c] = who
	}

	for _, c := range l.HeaderCells() {
		claim(c, "header")
	}
	for _, c := range l.FooterCells() {
		claim(c, "footer")
	}
	for _, c := range l.PayloadCells() {
		claim(c, "payload")
	}
	for _, tc := range l.TimingCells() {
		claim(tc.Cell, "timing")
	}
	for corner := 0; corner < 4; corner++ {
		for _, c := range l.DescriptorCellsAt(corner) {
			claim(c, fmt.Sprintf("descriptor%d", corner))
		}
	}
	for i := 0; i < 4; i++ {
		o := l.finderOrigin(i)
		for dy := 0; dy < FinderBox; dy++ {
			for dx := 0; dx < FinderBox; dx++ {
				c := Cell{o.X + dx, o.Y + dy}
				if _, taken := owner[c]; !taken {
					owner[c] = "finder"
				}
			}
		}
	}

	// Header and footer bands skip finder boxes, and the middle band's finder
	// boxes overlap the timing columns, so account for both.
	var unclaimed []Cell
	for y := 0; y < l.GridHeight; y++ {
		for x := 0; x < l.GridWidth; x++ {
			c := Cell{x, y}
			if _, ok := owner[c]; !ok {
				unclaimed = append(unclaimed, c)
			}
		}
	}
	assert.Empty(t, unclaimed, "every grid cell should belong to a region")
}

func TestPayloadRegionExcludesTimingColumns(t *testing.T) {
	l, err := NewLayout(DefaultGridWidth, DefaultGridHeight, DefaultCellPixels)
	require.NoError(t, err)

	r := l.PayloadRect()
	assert.Equal(t, 1, r.Min.X, "column 0 carries the timing pattern")
	assert.Equal(t, l.GridWidth-1, r.Max.X, "the last column carries the timing pattern")
	assert.Equal(t, l.HeaderBandRows, r.Min.Y)
	assert.Equal(t, l.GridHeight-l.FooterBandRows, r.Max.Y)
	assert.Equal(t, l.PayloadCellCount(), len(l.PayloadCells()))
	assert.Equal(t, l.PayloadRows()*l.PayloadCols(), l.PayloadCellCount())
}

func TestHeaderAndFooterCellsAvoidFinders(t *testing.T) {
	l, err := NewLayout(256, 256, 4)
	require.NoError(t, err)

	for _, c := range append(l.HeaderCells(), l.FooterCells()...) {
		assert.False(t, l.inFinderBox(c), "band cell %v overlaps a finder box", c)
	}
}

func TestTimingPatternAlternates(t *testing.T) {
	l, err := NewLayout(DefaultGridWidth, DefaultGridHeight, DefaultCellPixels)
	require.NoError(t, err)

	timing := l.TimingCells()
	require.Equal(t, 2*l.PayloadRows(), len(timing))

	// The two columns run in opposite phase, so a homography off by one row
	// scores near zero on both rather than passing on one by luck.
	for i := 0; i < len(timing); i += 2 {
		left, right := timing[i], timing[i+1]
		require.Equal(t, left.Cell.Y, right.Cell.Y)
		assert.NotEqual(t, left.On, right.On, "columns must be in opposite phase at row %d", left.Cell.Y)
	}
}

func TestFinderCentersMatchOrigins(t *testing.T) {
	l, err := NewLayout(DefaultGridWidth, DefaultGridHeight, DefaultCellPixels)
	require.NoError(t, err)

	origins := l.FinderOrigins()
	centers := l.FinderCenters()
	for i := range origins {
		assert.InDelta(t, float64(origins[i].X)+FinderPattern/2.0, centers[i][0], 1e-9)
		assert.InDelta(t, float64(origins[i].Y)+FinderPattern/2.0, centers[i][1], 1e-9)
	}

	// Corner ordering is load-bearing: the homography pairs these points with
	// detected image corners in exactly this order.
	assert.Equal(t, Cell{0, 0}, origins[0])
	assert.Equal(t, Cell{l.GridWidth - FinderPattern, 0}, origins[1])
	assert.Equal(t, Cell{0, l.GridHeight - FinderPattern}, origins[2])
	assert.Equal(t, Cell{l.GridWidth - FinderPattern, l.GridHeight - FinderPattern}, origins[3])
}

// A decoder rebuilds geometry from the header alone, so LayoutFor must agree
// with the layout the encoder used.
func TestLayoutForReproducesEncoderGeometry(t *testing.T) {
	want, err := NewLayout(320, 180, 6)
	require.NoError(t, err)

	got, err := LayoutFor(Header{
		GridWidth:  uint16(want.GridWidth),
		GridHeight: uint16(want.GridHeight),
		CellPixels: uint16(want.CellPixels),
	})
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestCellRectAndCenterAgree(t *testing.T) {
	l, err := NewLayout(DefaultGridWidth, DefaultGridHeight, DefaultCellPixels)
	require.NoError(t, err)

	for _, c := range []Cell{{0, 0}, {5, 9}, {l.GridWidth - 1, l.GridHeight - 1}} {
		r := l.CellRect(c)
		cx, cy := l.CellCenter(c)
		assert.InDelta(t, float64(r.Min.X+r.Max.X)/2, cx, 1e-9)
		assert.InDelta(t, float64(r.Min.Y+r.Max.Y)/2, cy, 1e-9)
		assert.True(t, r.In(l.Bounds()), "cell %v must fall inside the image", c)
	}
}
