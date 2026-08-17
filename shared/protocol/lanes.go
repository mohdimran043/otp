package protocol

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"math"
	"sort"
)

// Several independent frames shown at once, tiled across one display.
//
// The obvious reason to do this is wrong, and it is worth stating first because it is what everyone
// reaches for. Tiling does *not* buy pixels per cell. Area is area: four 64-cell lanes and one
// 128-cell grid carry the same 16,384 cells and span the same display, so a camera resolves the same
// pixels per cell either way — measured, 0.92 to 0.96 of it, slightly worse, because each lane pays
// for its own quiet zone and the gaps between them. Anyone tiling in the hope of resolving finer
// cells should stop and make the grid smaller instead.
//
// What tiling actually buys is independence, and on this channel that is worth more than the few
// percent it costs.
//
// A single grid is one indivisible bet. A reflection across one corner of the panel, a hand passing
// through the shot, glare from a window, or a fiducial lost to a rolling-shutter tear costs the whole
// frame — every cell of it, including the thousands that were photographed perfectly. Tiled, the same
// blemish costs the lane it falls on. The rest still carry their symbols, and under a fountain code,
// where any sufficient set of symbols rebuilds the block and no particular symbol is needed, those
// surviving lanes advance the transfer exactly as much as if the spoiled one had never been sent.
// That is why this pairs with RaptorQ specifically and would be far less useful beside a scheme that
// needs particular symbols back.
//
// There is a second, quieter gain. Each lane fits its own homography and its own photometric
// reference from its own fiducials, so lens distortion, a panel that is not quite flat, and uneven
// backlight are corrected per region rather than globally. A single grid spanning the display has to
// describe all of that with one transform and one reference fitted from four corners; four lanes
// describe it with four, each over a quarter of the area.
//
// This deliberately does not touch Layout. A lane *is* a Layout — the same fiducials, bands, timing
// columns and descriptor — and the decoder that reads one reads any of them without knowing it was
// tiled. What is added here is only where each lane sits.

// Lane count bounds. One is the degenerate case and is allowed, because it makes a single-frame
// display the same code path as a tiled one rather than a special case beside it.
const (
	MinLanes = 1
	MaxLanes = 16
)

// ErrLaneCount means a lane count outside what can be tiled.
var ErrLaneCount = errors.New("protocol: unsupported lane count")

// LaneLayout is a tiling of identical frames across one display.
type LaneLayout struct {
	// Lane is the geometry of a single frame. Every lane is identical, which is what lets one
	// decoder read all of them and one calibration serve all of them.
	Lane Layout

	// Columns and Rows are the arrangement. Four lanes are two by two; two lanes are side by side,
	// because a display is wider than it is tall and stacking them would waste the width.
	Columns, Rows int

	// Gap is the blank space between lanes, in cells of the lane's own grid.
	//
	// Not decoration. Two frames flush against each other put one's quiet zone against the other's,
	// and a fiducial search that finds eight candidates in a row has to work out which four belong
	// together — a problem that does not arise if there is visibly nothing between them. It is
	// measured in cells rather than pixels so that it scales with the lane and does not have to be
	// re-tuned when the grid changes.
	Gap int
}

// laneGrid picks the arrangement for a lane count.
//
// Wider than tall wherever there is a choice, because a display is, and because a lane's own frame is
// square: laying four out as one row of four would leave the display's height mostly empty and shrink
// every cell to fit the width.
func laneGrid(lanes int) (columns, rows int, err error) {
	switch lanes {
	case 1:
		return 1, 1, nil
	case 2:
		return 2, 1, nil
	case 4:
		return 2, 2, nil
	case 6:
		return 3, 2, nil
	case 8:
		return 4, 2, nil
	case 9:
		return 3, 3, nil
	case 12:
		return 4, 3, nil
	case 16:
		return 4, 4, nil
	default:
		return 0, 0, fmt.Errorf("%w: %d lanes cannot be tiled evenly; use 1, 2, 4, 6, 8, 9, 12 or 16",
			ErrLaneCount, lanes)
	}
}

// NewLaneLayout tiles the given number of copies of a lane geometry.
func NewLaneLayout(lane Layout, lanes, gap int) (LaneLayout, error) {
	if lanes < MinLanes || lanes > MaxLanes {
		return LaneLayout{}, fmt.Errorf("%w: %d is outside %d..%d", ErrLaneCount, lanes, MinLanes, MaxLanes)
	}
	columns, rows, err := laneGrid(lanes)
	if err != nil {
		return LaneLayout{}, err
	}
	if gap < 0 {
		return LaneLayout{}, fmt.Errorf("protocol: a negative lane gap (%d) is not a gap", gap)
	}
	return LaneLayout{Lane: lane, Columns: columns, Rows: rows, Gap: gap}, nil
}

// Lanes is how many frames this layout carries.
func (l LaneLayout) Lanes() int { return l.Columns * l.Rows }

// laneEdgePixels is one lane's rendered width, including its quiet zone.
func (l LaneLayout) laneEdgePixels() int {
	return (l.Lane.GridWidth + 2*l.Lane.QuietZone) * l.Lane.CellPixels
}

func (l LaneLayout) laneEdgePixelsY() int {
	return (l.Lane.GridHeight + 2*l.Lane.QuietZone) * l.Lane.CellPixels
}

// gapPixels is the space between neighbouring lanes.
func (l LaneLayout) gapPixels() int { return l.Gap * l.Lane.CellPixels }

// ImageSize is the whole tiled display, in pixels.
func (l LaneLayout) ImageSize() image.Point {
	w := l.Columns*l.laneEdgePixels() + (l.Columns-1)*l.gapPixels()
	h := l.Rows*l.laneEdgePixelsY() + (l.Rows-1)*l.gapPixels()
	return image.Point{X: w, Y: h}
}

// LaneOrigin is where lane i's own image begins, in pixels of the tiled display.
//
// Lanes are numbered left to right then top to bottom — reading order — because that is the order an
// operator looking at the screen will assume, and a diagnostic that numbered them otherwise would
// have to be read with a diagram beside it.
func (l LaneLayout) LaneOrigin(lane int) (image.Point, error) {
	if lane < 0 || lane >= l.Lanes() {
		return image.Point{}, fmt.Errorf("%w: lane %d of %d", ErrLaneCount, lane, l.Lanes())
	}
	col := lane % l.Columns
	row := lane / l.Columns
	return image.Point{
		X: col * (l.laneEdgePixels() + l.gapPixels()),
		Y: row * (l.laneEdgePixelsY() + l.gapPixels()),
	}, nil
}

// LaneBounds is lane i's rectangle within the tiled display.
func (l LaneLayout) LaneBounds(lane int) (image.Rectangle, error) {
	origin, err := l.LaneOrigin(lane)
	if err != nil {
		return image.Rectangle{}, err
	}
	return image.Rect(origin.X, origin.Y,
		origin.X+l.laneEdgePixels(), origin.Y+l.laneEdgePixelsY()), nil
}

// LaneAt reports which lane contains a point of the tiled display, and whether any does.
//
// A point in the gap belongs to no lane, and saying so is the useful answer: it is how a receiver
// that has found a fiducial in the space between lanes learns that its geometry is wrong, rather than
// attributing it to whichever lane is nearest.
func (l LaneLayout) LaneAt(p image.Point) (int, bool) {
	for lane := 0; lane < l.Lanes(); lane++ {
		b, err := l.LaneBounds(lane)
		if err != nil {
			return 0, false
		}
		if p.In(b) {
			return lane, true
		}
	}
	return 0, false
}

// CellsPerFrame is how many payload cells the whole display carries, across every lane.
func (l LaneLayout) CellsPerFrame() int { return l.Lanes() * l.Lane.PayloadCellCount() }

// ModulePixelsAt is how many camera pixels one cell resolves to, given the short side of the picture
// the camera takes and how much of it the display fills.
//
// Note what this is not: an argument for tiling. The figure depends on how many cells span the
// display and nothing else, so a tiling and a single grid of the same capacity give the same answer
// to within the cost of the extra quiet zones. It is here so that a tiling can be checked against
// the same floors a single grid is — see the readable package — not to show it in a better light.
func (l LaneLayout) ModulePixelsAt(captureShortSide int, fill float64) float64 {
	if captureShortSide <= 0 || fill <= 0 {
		return 0
	}
	// The display's own long edge in lane-cells, since that is what spans the capture.
	cellsAcross := l.Columns*(l.Lane.GridWidth+2*l.Lane.QuietZone) + (l.Columns-1)*l.Gap
	if cellsAcross <= 0 {
		return 0
	}
	return float64(captureShortSide) * fill / float64(cellsAcross)
}

// Compose tiles already-rendered frame images into one display image.
//
// Composition happens here, at display time, rather than in the encoder — and that is what makes the
// whole tiling cheap. Each lane is an ordinary frame, rendered by the ordinary encoder, stored and
// checksummed like any other; this only decides where each one is placed. Nothing upstream knows it
// is being tiled, and nothing downstream has to: a decoder handed one lane's region reads it exactly
// as it reads a frame that was shown alone.
//
// Fewer images than lanes is allowed and is the ordinary case at the end of a transmission, when
// there are three chunks left and four lanes to fill. The remaining lanes are left as background,
// which reads as an absence rather than as a corrupt frame: a receiver finds no fiducials there and
// moves on, where a repeated or blank-but-framed lane would cost it a decode attempt to discover the
// same thing.
func (l LaneLayout) Compose(lanes []image.Image) (image.Image, error) {
	if len(lanes) > l.Lanes() {
		return nil, fmt.Errorf("%w: %d images for %d lanes", ErrLaneCount, len(lanes), l.Lanes())
	}

	size := l.ImageSize()
	out := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))

	// The background is the quiet zone's colour, so the gaps between lanes are the same dark field a
	// lane's own margin is. A light gap would put a bright edge against every lane's quiet zone, which
	// is precisely the high-contrast straight line the fiducial search is looking for.
	draw.Draw(out, out.Bounds(), &image.Uniform{CellOff}, image.Point{}, draw.Src)

	for i, src := range lanes {
		if src == nil {
			continue
		}
		origin, err := l.LaneOrigin(i)
		if err != nil {
			return nil, err
		}
		b := src.Bounds()
		at := image.Rect(origin.X, origin.Y, origin.X+b.Dx(), origin.Y+b.Dy())
		draw.Draw(out, at, src, b.Min, draw.Src)
	}
	return out, nil
}

// SelectLaneQuads finds one frame per lane in a rectified capture.
//
// The single-frame search picks the best four fiducials in the picture and stops, which is right when
// there is one frame and wrong when there are several: on a tiled display it returns one lane and the
// rest of the screen goes unread.
//
// Grouping them is harder than it looks, and two obvious approaches were tried and abandoned against
// a real composition. Taking the strongest quad and searching what remains selects a quad spanning
// the whole display — the outer corners of the four lanes — which passes every plausibility test the
// single-frame path applies, because every lane is rendered at the same geometry so the module sizes
// agree, the arrangement is convex, and at a two-by-two tiling it is exactly square. Clustering by
// distance fails for a different reason: within a 96-cell lane the fiducials are 356 pixels apart,
// while the nearest fiducials of two *neighbouring* lanes are 52 apart, so proximity groups them
// backwards.
//
// What does work is the geometry itself. The tiling is known — the sender chose it — so a fiducial's
// position says which lane it belongs to before any grouping is attempted, and a quad is then
// selected within each lane from its own candidates. That removes the ambiguity rather than trying to
// resolve it afterwards.
//
// It requires a *rectified* capture: coordinates in the tiled display's own frame, not the camera's.
// A photograph taken off-axis has to have its screen found and its perspective removed first, and
// that step is the caller's. Handed raw camera coordinates this will assign fiducials to the wrong
// lanes and find nothing, which is the honest failure — better than appearing to work while reading
// two lanes into one.
//
// Lanes with too few candidates are skipped. Returning three quads from a four-lane display is the
// ordinary case, not a shortfall: a lane lost to glare simply is not there to find, and under a
// fountain code the other three still advance the transfer.
func (l LaneLayout) SelectLaneQuads(cands []FinderCandidate) map[int][4]FinderCandidate {
	byLane := map[int][]FinderCandidate{}
	for _, c := range cands {
		lane, ok := l.LaneAt(image.Point{X: int(c.Center.X), Y: int(c.Center.Y)})
		if !ok {
			// In the gap between lanes. Evidence the rectification is off, and deliberately not
			// attributed to the nearest lane — a fiducial that is not inside a lane is not that lane's.
			continue
		}
		byLane[lane] = append(byLane[lane], c)
	}

	out := map[int][4]FinderCandidate{}
	for lane, group := range byLane {
		if len(group) < 4 {
			continue
		}
		quad, err := SelectFinderQuad(group)
		if err != nil {
			continue
		}
		out[lane] = quad
	}
	return out
}

// laneAspectTolerance is how far from square a lane's fiducial quad may be.
//
// A frame is square, so the four fiducials sit on a square, and a camera looking at one off-axis
// foreshortens it — but not by much at any angle the rest of the decoder tolerates. A quad built from
// two lanes side by side is a different thing entirely: twice as wide as it is tall, or twice as tall
// as wide. The gap between those two cases is wide enough that the threshold does not need to be
// delicate, which is the only reason a single number is defensible here.
const laneAspectTolerance = 1.6

// plausibleLaneShape reports whether four fiducials could be one frame's corners.
//
// It exists because of a failure that nothing else could catch. On a tiled display every lane is
// rendered at the same geometry, so a quad taking two fiducials from one lane and two from its
// neighbour has fiducials of identical module size arranged convexly — passing every test the
// single-frame path applies — and describes a frame twice as wide as any that was ever displayed. It
// was found by a test that composed four lanes and got a quad centred in the gap between two of them.
func plausibleLaneShape(q [4]FinderCandidate) bool {
	cycle := OrderQuad([4]Point{q[0].Center, q[1].Center, q[2].Center, q[3].Center})
	top := cycle[0].Dist(cycle[1])
	bottom := cycle[2].Dist(cycle[3])
	left := cycle[0].Dist(cycle[2])
	right := cycle[1].Dist(cycle[3])

	width := (top + bottom) / 2
	height := (left + right) / 2
	if width <= 0 || height <= 0 {
		return false
	}
	ratio := width / height
	if ratio < 1 {
		ratio = 1 / ratio
	}
	return ratio <= laneAspectTolerance
}

// LocateAll finds every frame present in one capture, in the capture's own coordinates.
//
// This is the receiver's counterpart to Compose, and it deliberately does not need the photograph
// rectified first. An earlier version did: it grouped fiducials by which lane's rectangle they fell
// in, which is exact but requires the display to have been found and its perspective removed before
// any frame can be read — a large piece of machinery to build before the first lane decodes.
//
// The observation that removes that requirement is that a frame already proves itself. Locate reads
// the grid descriptor at each candidate geometry and checks its CRC before returning, so a quad
// assembled from two neighbouring lanes' fiducials — the failure that defeats every cheap grouping
// rule, because on a tiled display the module sizes agree and the arrangement is convex and square —
// simply does not decode. There is no need to reason about which fiducials belong together when the
// checksum will say.
//
// So this proposes quads, crops around each, and lets Locate accept or reject it. Cropping rather
// than masking because Locate searches the whole image it is given: handed the full picture it would
// find the same strongest quad every time, and handed a crop it finds what is inside the crop.
//
// Returned geometries carry coordinates in the original capture, not the crop, so a caller can sample
// the image it already has. Frames are returned in the order they were found, which is by fiducial
// strength and not by lane: a caller wanting lane numbers should read them from the headers, which
// carry the frame numbers the sender assigned.
func LocateAll(img image.Image, opts LocateOptions, maxFrames int) []*Geometry {
	if maxFrames < 1 {
		return nil
	}

	cands := FindFinderCandidates(Binarize(Grayscale(img)))
	if len(cands) < 4 {
		return nil
	}
	// Bounded before enumerating: the count below grows as the fourth power, and a noisy capture can
	// produce dozens of candidates that are not fiducials at all.
	if len(cands) > maxLaneCandidates {
		ranked := make([]FinderCandidate, len(cands))
		copy(ranked, cands)
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].Confidence > ranked[j].Confidence })
		cands = ranked[:maxLaneCandidates]
	}

	// Every four candidates that could be one frame, smallest first.
	//
	// Order is the whole trick, and getting it wrong is what made an earlier version return nothing at
	// all. Taking the strongest quad and searching what remains selects the four *outermost* fiducials
	// of a tiled display — a quad spanning every lane, which is square, whose module sizes agree, and
	// which passes every plausibility test there is. It then consumes four real lanes' corners on its
	// way to failing, and the frames that were actually there can no longer be assembled.
	//
	// Smallest first inverts that. On a tiling the real frames are the smallest repeating unit, so any
	// quad spanning two lanes is strictly larger than the lanes it spans, and the true ones are
	// reached before anything can eat their corners.
	type sized struct {
		quad [4]FinderCandidate
		span float64
	}
	var quads []sized
	for a := 0; a < len(cands); a++ {
		for b := a + 1; b < len(cands); b++ {
			for c := b + 1; c < len(cands); c++ {
				for d := c + 1; d < len(cands); d++ {
					q := [4]FinderCandidate{cands[a], cands[b], cands[c], cands[d]}
					if !similarModules(q) || !plausibleLaneShape(q) || !plausibleLaneSize(q) {
						continue
					}
					quads = append(quads, sized{quad: q, span: quadSpan(q)})
				}
			}
		}
	}
	sort.Slice(quads, func(i, j int) bool { return quads[i].span < quads[j].span })

	bounds := img.Bounds()
	var found []*Geometry
	consumed := map[Point]bool{}

	// Bounded attempts, and the bound matters more than it looks.
	//
	// Filtering leaves hundreds of plausible quads on a tiled capture, and each rejected one costs a
	// crop and a full descriptor read. Unbounded, a capture where nothing decodes spends that on every
	// candidate — which is how a receiver came to drop 61 of 172 posted frames for being too slow,
	// throwing away a third of the photographs a merge was going to be built from.
	//
	// A few attempts per lane is enough. The real frames are the smallest quads, so they are reached
	// early; anything still being tried after that is a combination that spans lanes.
	attempts := 0
	maxAttempts := maxFrames * laneAttemptsEach

	// Once one frame has decoded, every other lane is the same size — they are copies of one geometry.
	// Quads far from that span are spanning lanes rather than describing one, and can be skipped
	// without a crop.
	var knownSpan float64

	for _, s := range quads {
		if len(found) >= maxFrames || attempts >= maxAttempts {
			break
		}
		if knownSpan > 0 {
			ratio := s.span / knownSpan
			if ratio < 0.75 || ratio > 1.25 {
				continue
			}
		}
		// Skip anything built from a fiducial already claimed by a frame that decoded. Only decoded
		// frames claim their corners — a quad that failed proves nothing about the fiducials it used,
		// and removing them was exactly the mistake above.
		overlaps := false
		for _, c := range s.quad {
			if consumed[c.Center] {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}

		crop := cropAround(bounds, s.quad)
		sub, ok := subImage(img, crop)
		if !ok {
			continue
		}
		attempts++
		g, err := Locate(sub, opts)
		if err != nil {
			// Not a frame. The commonest outcome by far, and not worth reporting: rejecting quads that
			// span two lanes is what this loop is for, and the descriptor checksum is what does it.
			continue
		}
		if g.TimingScore < minLaneTimingScore {
			// Located, checksummed, and still not this quad's frame.
			//
			// The comment above this loop says the descriptor checksum is what rejects a quad spanning two
			// lanes. That is true of lanes side by side and false of lanes stacked, which is a gap nothing
			// caught until a printed sheet went through it. A stacked pair's outer corners describe an
			// almost-square region once the gap is counted — 1480 by 1616 at grid 192, a ratio of 1.09 —
			// so plausibleLaneShape's 1.6 tolerance passes it, and the descriptor read at its top-left
			// corner is a real lane's real descriptor, so the CRC passes too. The quad is then accepted,
			// consumes the corners of the lane it borrowed them from, and both frames are lost.
			//
			// The timing pattern is what actually knows. It alternates down the first and last payload
			// columns, so a homography stretched to span two lanes walks off it within a few rows: measured
			// on exactly this failure, a true lane scores 1.000 and the spanning quad scores 0.488 — chance,
			// for a two-valued pattern. There is no overlap to trade off against, which is why this is a
			// gate rather than a weighting.
			//
			// Cheaper than it looks, too: the score is already computed by Locate for the receiver's decode
			// quality, so this costs a comparison.
			continue
		}
		if knownSpan == 0 {
			knownSpan = s.span
		}
		// Back into the capture's own coordinates, transform included.
		//
		// The corners are what a caller prints and the homography is what a caller samples with, and
		// for a long time only the corners were moved. That geometry passes every inspection — the
		// corners land on the right lane, the header reads, the frame number is correct — and then
		// samples the payload at crop-relative coordinates against the full picture, which lands in a
		// neighbouring lane or off the display entirely. It is why decoding at a located lane was
		// believed impossible and every caller fell back to searching the whole image again.
		g.Homography = g.Homography.Translate(float64(crop.Min.X), float64(crop.Min.Y))
		for i := range g.Corners {
			g.Corners[i].X += float64(crop.Min.X)
			g.Corners[i].Y += float64(crop.Min.Y)
		}
		for _, c := range s.quad {
			consumed[c.Center] = true
		}
		found = append(found, g)
	}
	return found
}

// maxLaneCandidates bounds the combination search. Four nested loops over n candidates is n^4/24, so
// twenty is already fifteen thousand quads to filter — cheap, since the filter is arithmetic on four
// points, but not something to leave unbounded on a noisy capture.
const maxLaneCandidates = 20

// minLaneTimingScore is how well a located quad must match the timing pattern to be believed.
//
// Set against what the two populations actually score rather than by taste, because they do not overlap:
// a real lane composed and read back scores 1.000, and a quad spanning two stacked lanes scores 0.488 —
// which is chance, since the pattern has two values. Anything in between is a capture degrading, and 0.65
// sits clear of chance while leaving a wide margin for one.
//
// It is deliberately not tighter. A soft or skewed capture loses timing cells honestly, and a threshold
// close to 1.0 would reject frames that decode perfectly well — trading a bug that loses lanes for a
// gate that does the same thing on purpose.
const minLaneTimingScore = 0.65

// quadSpan is the largest distance between any two of a quad's fiducials.
func quadSpan(q [4]FinderCandidate) float64 {
	var span float64
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			span = math.Max(span, q[i].Center.Dist(q[j].Center))
		}
	}
	return span
}

// DefaultLaneGapCells is the blank space a sender should leave between lanes, in cells.
//
// Measured against cropMarginCells below, which is what makes a gap necessary at all. LocateAll reads
// a lane by cropping around its fiducials and handing the crop to Locate, and that crop reaches ten
// cells past each fiducial centre. A fiducial sits three and a half cells inside the frame's corner
// with two more cells of quiet zone outside it, so the crop already extends four and a half cells
// beyond the lane's own edge — into the neighbour, if the neighbour starts there. Locate then finds
// the neighbour's fiducials inside the crop and is back to the ambiguity the crop existed to remove.
//
// The numbers, composing two lanes and reading them back through shared/simulate:
//
//	gap  pristine  clean  typical  harsh
//	  0     2/2      2/2     0/2     0/2
//	  2     2/2      2/2     2/2     0/2
//	  4     2/2      2/2     2/2     1/2
//	  6     2/2      2/2     2/2     2/2
//
// Flush lanes read perfectly from the encoder's own pixels and fail completely through any camera,
// which is exactly the shape of failure that gets a tiling shipped and then found broken on a rig.
// Six is the first gap that holds at Harsh, and it costs about three percent of the display's width.
const DefaultLaneGapCells = 6

// cropMarginCells is how much room is left around a quad when cropping, in cells.
//
// A fiducial's centre sits three and a half cells inside the frame's corner, and the quiet zone is
// outside that again, so a crop taken exactly at the fiducial centres would cut through the frame's
// own margin — and the fiducial search needs unbroken background around a ring to find it. Ten cells
// is comfortably more than either, and costs only pixels.
const cropMarginCells = 10

// cropAround is the region of the capture that should contain one frame.
func cropAround(bounds image.Rectangle, quad [4]FinderCandidate) image.Rectangle {
	minX, minY := quad[0].Center.X, quad[0].Center.Y
	maxX, maxY := minX, minY
	for _, c := range quad[1:] {
		minX = math.Min(minX, c.Center.X)
		maxX = math.Max(maxX, c.Center.X)
		minY = math.Min(minY, c.Center.Y)
		maxY = math.Max(maxY, c.Center.Y)
	}
	margin := MeanModuleSize(quad) * cropMarginCells
	r := image.Rect(
		int(minX-margin), int(minY-margin),
		int(maxX+margin)+1, int(maxY+margin)+1,
	)
	return r.Intersect(bounds)
}

// subImage crops to an image whose bounds begin at the origin.
//
// The obvious implementation uses SubImage, which shares pixels and copies nothing — and it does not
// work here, which cost a debugging pass to discover even though this repository already documents
// it. A SubImage keeps its parent's offset, so a crop taken from the middle of a capture has bounds
// starting at that offset, and the geometry search underneath Locate assumes bounds beginning at
// zero. Handed such a crop it samples the wrong pixels and finds nothing, silently: not an error, no
// frames.
//
// So the pixels are copied into an origin-anchored image. It is a real cost paid several times a
// frame, and it is bounded by the crop rather than the capture — a lane's region is a fraction of a
// megapixel picture, which is the whole reason for cropping in the first place.
func subImage(img image.Image, r image.Rectangle) (image.Image, bool) {
	if r.Empty() {
		return nil, false
	}
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), img, r.Min, draw.Src)
	return out, true
}

// laneAttemptsEach is how many candidate quads may be tried per lane before giving up.
//
// Measured rather than chosen. Four per lane was tried first and found nothing at all on a four-lane
// composition: the filters leave enough near-square, correctly-scaled combinations spanning pairs of
// lanes that the real frames are not reached within sixteen attempts, and the search gave up short.
// Twenty per lane finds all four reliably.
//
// The cost is bounded where it matters. Only the attempts before the *first* success are spent
// blindly — after that every lane is known to be the same size, so quads far from that span are
// skipped without a crop, and the fiducials of a decoded frame are claimed so nothing rebuilds it.
const laneAttemptsEach = 20

// plausibleLaneSize rejects quads whose span makes no sense for their own cell size.
//
// The fiducial centres of a frame sit three and a half cells inside each corner, so the distance
// between them is the grid less seven cells — and the cell size is measured independently, from the
// fiducials themselves. Dividing one by the other gives the grid the quad implies, and a quad that
// implies a grid the protocol cannot render is not a frame.
//
// It exists because ordering by span alone is not enough. On a two-by-two tiling the four fiducials
// nearest the centre — one inner corner from each of the four lanes — form a small, square, uniform
// quad that passes every other test and is *smaller* than any real lane, so a smallest-first search
// reaches it before the frames it is looking for. Measured on a composed display: span 73 against a
// real lane's 356, implying a 25-cell grid where the protocol's minimum is 48. Four of those between
// them exhausted a bounded search and it found nothing at all.
func plausibleLaneSize(q [4]FinderCandidate) bool {
	module := MeanModuleSize(q)
	if module <= 0 {
		return false
	}
	cycle := OrderQuad([4]Point{q[0].Center, q[1].Center, q[2].Center, q[3].Center})
	side := (cycle[0].Dist(cycle[1]) + cycle[0].Dist(cycle[2])) / 2
	grid := side/module + FinderPattern
	return grid >= MinGridDimension && grid <= MaxGridDimension
}
