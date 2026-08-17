package api

import (
	"image"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Printing several frames to a sheet.
//
// The arithmetic that matters here is not the layout — it is that a sheet is the same tiling a display
// already does, so a photograph of one is read by the same LocateAll the capture loop uses. These tests
// pin the two things that would silently break that: the arrangement being portrait, and the gap between
// frames being the measured one rather than zero.

// laneLayoutFor is the geometry the tests below tile: small, so the composed images stay cheap.
func testLane(t *testing.T) protocol.Layout {
	t.Helper()
	l, err := protocol.NewLayoutQuiet(64, 64, 2, 2)
	require.NoError(t, err)
	return l
}

// frameImages returns n images the size one lane renders at.
func frameImages(l protocol.Layout, n int) []image.Image {
	out := make([]image.Image, 0, n)
	for range n {
		out = append(out, image.NewRGBA(image.Rect(0, 0, l.ImageWidth(), l.ImageHeight())))
	}
	return out
}

// TestSheetArrangementsArePortrait is the difference between a page and a display, and it is the whole
// reason the display's own laneGrid could not be reused.
//
// A display is wider than it is tall, so two lanes go side by side. A4 is the other way round, and two
// frames side by side on a portrait page are each limited to half the page's *width* — 3.6 inches, when
// stacking them would give 5.2. Same two frames, same paper, half the cell size, for no reason but the
// arrangement.
func TestSheetArrangementsArePortrait(t *testing.T) {
	for _, tc := range []struct {
		perPage    int
		cols, rows int
	}{
		{1, 1, 1},
		{2, 1, 2},
		{4, 2, 2},
		{6, 2, 3},
	} {
		cols, rows, err := sheetArrangement(tc.perPage)
		require.NoError(t, err, "per_page=%d", tc.perPage)
		assert.Equal(t, tc.cols, cols, "per_page=%d columns", tc.perPage)
		assert.Equal(t, tc.rows, rows, "per_page=%d rows", tc.perPage)
		assert.GreaterOrEqual(t, rows, cols, "per_page=%d must not be wider than it is tall on portrait paper", tc.perPage)
	}
}

// A per-page count nobody laid out is refused rather than guessed at. Three frames to a page has no
// arrangement that is not either wasteful or wrong, and inventing one at request time would produce a
// sheet whose geometry no test has ever looked at.
func TestUnsupportedSheetArrangementIsRefused(t *testing.T) {
	for _, perPage := range []int{0, -1, 3, 5, 7, 100} {
		_, _, err := sheetArrangement(perPage)
		assert.Error(t, err, "per_page=%d", perPage)
	}
}

// TestSheetsLeaveTheMeasuredGapBetweenFrames is the one that stops this being decorative.
//
// LocateAll reads a frame by cropping around its fiducials, and that crop reaches ten cells past each
// fiducial centre — into the neighbour, if the neighbour starts there. Composed flush, two frames read
// perfectly from the encoder's own pixels and fail completely through a camera, which is exactly the
// shape of bug that ships: it passes every test that does not involve a lens.
func TestSheetsLeaveTheMeasuredGapBetweenFrames(t *testing.T) {
	lane := testLane(t)

	sheets, err := composeSheets(frameImages(lane, 2), lane, 2)
	require.NoError(t, err)
	require.Len(t, sheets, 1)

	// Two lanes stacked, so the sheet is two frames tall plus exactly one gap.
	gapPixels := protocol.DefaultLaneGapCells * lane.CellPixels
	assert.Equal(t, 2*lane.ImageHeight()+gapPixels, sheets[0].Bounds().Dy(),
		"a stacked pair must carry one measured gap between them")
	assert.Equal(t, lane.ImageWidth(), sheets[0].Bounds().Dx())
	assert.Positive(t, gapPixels, "a zero gap is the failure this test exists for")
}

// Frames divide across sheets, and the last sheet is allowed to be short. A transfer whose frame count
// is not a multiple of the page's capacity is the ordinary case, not an error.
func TestFramesDivideAcrossSheetsWithAShortLast(t *testing.T) {
	lane := testLane(t)

	for _, tc := range []struct {
		frames, perPage, sheets int
	}{
		{1, 1, 1},
		{5, 1, 5},
		{4, 2, 2},
		{5, 2, 3}, // the last sheet carries one
		{9, 4, 3}, // and here, one again
		{12, 6, 2},
	} {
		sheets, err := composeSheets(frameImages(lane, tc.frames), lane, tc.perPage)
		require.NoError(t, err, "%d frames %d-up", tc.frames, tc.perPage)
		assert.Len(t, sheets, tc.sheets, "%d frames %d-up", tc.frames, tc.perPage)
	}
}

// Every sheet is the same size, including a short final one.
//
// The empty lanes are left as background rather than the sheet being cropped to what it carries. A
// cropped last sheet would print its frames at a different scale from every sheet before it — the PDF
// fits each page's image to the page — so the one sheet holding the last chunk of a transfer would be
// the one sheet with a different cell size, and the operator would have no reason to expect it.
func TestAShortFinalSheetIsStillAFullSizedSheet(t *testing.T) {
	lane := testLane(t)

	sheets, err := composeSheets(frameImages(lane, 3), lane, 2)
	require.NoError(t, err)
	require.Len(t, sheets, 2)
	assert.Equal(t, sheets[0].Bounds().Dx(), sheets[1].Bounds().Dx())
	assert.Equal(t, sheets[0].Bounds().Dy(), sheets[1].Bounds().Dy(),
		"the last sheet keeps its empty lane rather than shrinking")
}

// TestAComposedSheetReadsBackAsEveryFrameOnIt is the test the rest of this file exists to support.
//
// Everything above measures pixels. This one asks the decoder: compose four real frames onto one sheet
// the way the printable document does, then hand that sheet to the same LocateAll the receiver runs on a
// photograph, and require it to find all four. A layout that tiles beautifully and cannot be read back is
// worth nothing, and the failure would otherwise surface at the end of the paper path — after printing,
// after photographing — where it is slowest and most expensive to diagnose.
//
// Composed pixels, not a photograph, so this proves the geometry rather than the optics. The camera's
// share of the problem is the px/cell floor in shared/readable, which is a different question and not one
// a unit test can settle.
func TestAComposedSheetReadsBackAsEveryFrameOnIt(t *testing.T) {
	// Eight pixels a cell: a printed cell is read from paper, and the sheet has to survive being located
	// at a size a scanner actually resolves rather than at the encoder's cheapest.
	lane, err := protocol.NewLayoutQuiet(64, 64, 8, 2)
	require.NoError(t, err)

	enc, err := encoding.ByName("binary")
	require.NoError(t, err)
	depth := enc.DefaultBitDepth()

	txID := uuid.New()
	frames := make([]image.Image, 0, 4)
	for i := range 4 {
		f := protocol.NewFrame(protocol.Header{
			TransmissionID: txID,
			FrameNumber:    uint32(i),
			ChunkNumber:    uint32(i),
			TotalChunks:    4,
		}, []byte{byte(i), byte(i + 1), byte(i + 2)})
		img, err := enc.Encode(f, lane, depth)
		require.NoError(t, err)
		frames = append(frames, img)
	}

	sheets, err := composeSheets(frames, lane, 4)
	require.NoError(t, err)
	require.Len(t, sheets, 1)

	found := protocol.LocateAll(sheets[0], protocol.LocateOptions{}, 16)
	require.Len(t, found, 4, "every frame printed on the sheet must be locatable in it")

	// Located is not read. Decode each one and require the frame numbers back, so a layout that put four
	// findable fiducial sets on a page while sampling the wrong cells cannot pass.
	numbers := map[int]bool{}
	for _, g := range found {
		frame, err := encoding.DecodeAt(g, sheets[0], protocol.LocateOptions{})
		require.NoError(t, err)
		numbers[int(frame.Header.FrameNumber)] = true
	}
	require.Equal(t, map[int]bool{0: true, 1: true, 2: true, 3: true}, numbers,
		"all four frames must decode, each as itself")
}

// One-up composes to exactly the frame, with no gap and no wrapper — the behaviour every existing
// printed sheet has, which must not change now that the path runs through the tiler.
func TestOneUpIsTheFrameItself(t *testing.T) {
	lane := testLane(t)

	sheets, err := composeSheets(frameImages(lane, 1), lane, 1)
	require.NoError(t, err)
	require.Len(t, sheets, 1)
	assert.Equal(t, lane.ImageWidth(), sheets[0].Bounds().Dx())
	assert.Equal(t, lane.ImageHeight(), sheets[0].Bounds().Dy())
}
