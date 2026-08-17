package protocol_test

import (
	"image"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

// LocateAll loses lanes on a stacked tiling once the grid passes 128, from the encoder's own pixels.
//
// Found while measuring the printed-sheet channel, and it is not a paper problem: there is no print and no
// scan here. Two frames composed one above the other with the measured six-cell gap, handed straight to
// LocateAll, and at grid 192 it returns one geometry instead of two — a quad built from lane 0's top
// fiducials and lane 1's top fiducials, spanning both frames.
//
// Why the existing guards do not catch it:
//
//   - plausibleLaneShape rejects a quad more than 1.6x from square, which is what stops a side-by-side
//     pair being read as one frame. The quad here is 1480 wide by 1616 tall — a ratio of 1.09, squarer
//     than plenty of legitimate captures — because a stacked pair's *outer* corners happen to describe an
//     almost-square region once the gap between them is included. The guard is aimed at the horizontal
//     case and the vertical one walks past it.
//   - The descriptor CRC, which LocateAll's own comment relies on to reject mixed quads ("a quad
//     assembled from two neighbouring lanes' fiducials simply does not decode"), passes here: the quad's
//     top-left corner is a genuine lane-0 corner carrying a genuine descriptor.
//
// So the spanning quad is accepted, consumes lane 0's corners, and the real frames can no longer be
// assembled. Grid 128 and below escape only because the true quads happen to be reached first — the
// smallest-first ordering *ties* at these sizes (a spanning quad and a real one both span 2093 px at grid
// 192), so which is tried first is incidental rather than decided.
//
// This affects a tiled display as well as a printed sheet: any grid above 128 tiled 1xN. It has gone
// unnoticed because the camera work runs at 80 to 96 cells, below where it starts.
func TestLocateAllFindsEveryLaneOnAStackedTiling(t *testing.T) {
	for _, grid := range []int{64, 96, 128, 192, 256} {
		lane, err := protocol.NewLayout(grid, grid, 8)
		require.NoError(t, err)

		var images []image.Image
		for i := range 2 {
			h := sampleHeader()
			h.CellPixels = uint16(lane.CellPixels)
			h.GridWidth, h.GridHeight = uint16(lane.GridWidth), uint16(lane.GridHeight)
			h.FrameNumber = uint32(200 + i)
			images = append(images, renderBands(t, lane, h))
		}

		sheet, err := protocol.LaneLayout{
			Lane: lane, Columns: 1, Rows: 2, Gap: protocol.DefaultLaneGapCells,
		}.Compose(images)
		require.NoError(t, err)

		found := protocol.LocateAll(sheet, protocol.LocateOptions{CellPixelsHint: lane.CellPixels}, 16)
		require.Len(t, found, 2,
			"grid %d: both stacked lanes must be located in the encoder's own pixels", grid)
	}
}
