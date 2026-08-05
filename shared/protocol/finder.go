package protocol

import (
	"math"
	"sort"
)

// finderRows is the 7x7 corner fiducial: a solid outer ring, a dark separating
// ring, and a solid 3x3 core.
//
// All four corners carry the identical pattern. Making one corner distinctive
// would identify orientation directly, but only under good optics — telling an
// eight-cell core from a nine-cell one is exactly the distinction blur destroys
// first. Instead the decoder resolves orientation by trying each candidate
// rotation and letting the header checksum settle it, which costs a few hundred
// extra samples and never guesses wrong.
var finderRows = [FinderPattern]string{
	"#######",
	"#     #",
	"# ### #",
	"# ### #",
	"# ### #",
	"#     #",
	"#######",
}

// FinderBits returns the fiducial as a grid of bright flags.
func FinderBits() [FinderPattern][FinderPattern]bool {
	var out [FinderPattern][FinderPattern]bool
	for y, row := range finderRows {
		for x := 0; x < FinderPattern; x++ {
			out[y][x] = row[x] == '#'
		}
	}
	return out
}

// finderRingFill is the fraction of a finder's bounding box occupied by its outer
// ring: 24 bright cells out of 49.
const finderRingFill = 24.0 / 49.0

// finderCoreFill is the fraction occupied by the 3x3 core: 9 cells out of 49.
const finderCoreFill = 9.0 / 49.0

// DrawFinder writes one fiducial and its separator through the supplied setter.
// The separator is written dark so the ring stays isolated from neighbouring
// header or footer data, which is what lets connected-component analysis find it.
func DrawFinder(l Layout, corner int, set func(Cell, bool)) {
	bits := FinderBits()
	origin := l.FinderOrigins()[corner]
	for dy := 0; dy < FinderPattern; dy++ {
		for dx := 0; dx < FinderPattern; dx++ {
			set(Cell{origin.X + dx, origin.Y + dy}, bits[dy][dx])
		}
	}

	// Clear the separator: the eighth column and row on whichever side faces the
	// grid interior.
	box := l.finderOrigin(corner)
	for d := 0; d < FinderBox; d++ {
		if origin.X == box.X {
			set(Cell{box.X + FinderBox - 1, box.Y + d}, false) // separator on the right
		} else {
			set(Cell{box.X, box.Y + d}, false) // separator on the left
		}
		if origin.Y == box.Y {
			set(Cell{box.X + d, box.Y + FinderBox - 1}, false) // separator below
		} else {
			set(Cell{box.X + d, box.Y}, false) // separator above
		}
	}
}

// DrawAllFinders writes all four fiducials.
func DrawAllFinders(l Layout, set func(Cell, bool)) {
	for i := 0; i < 4; i++ {
		DrawFinder(l, i, set)
	}
}

// FinderCandidate is a located fiducial.
type FinderCandidate struct {
	// Center is the core's centroid, the most stable estimate of the fiducial's
	// middle. The ring's own centroid drifts when blur eats one edge harder than
	// the opposite one; the core is solid and stays symmetric.
	Center Point

	// ModuleSize is the apparent width of one cell, in pixels, estimated from the
	// fiducial's bounding box.
	ModuleSize float64

	// ModuleArea is the same estimate derived from the ring's pixel area rather
	// than its bounding box.
	//
	// Area is rotation-invariant where a bounding box is not: rotate a fiducial
	// by 45 degrees and its box grows by a factor of root two while its area is
	// unchanged. Since grid-size estimation multiplies this figure by up to a
	// thousand cells, that difference dominates the result, so this is the
	// estimate the geometry search uses.
	ModuleArea float64

	// Confidence scores how closely the candidate matched, in 0..1.
	Confidence float64

	ring Component
	core Component
}

// FindFinderCandidates locates fiducials in a binarised capture.
//
// The test that carries the weight is structural rather than statistical: a
// finder is a bright ring whose bounding-box centre is *also* bright but belongs
// to a different connected component. Only a ring-inside-a-ring produces that,
// so ordinary bright blobs in a photograph are rejected without tuning any
// threshold.
func FindFinderCandidates(bm *Bitmap) []FinderCandidate {
	lab := LabelComponents(bm)
	out := make([]FinderCandidate, 0, 8)

	for i := range lab.Components {
		ring := lab.Components[i]
		w, h := ring.Width(), ring.Height()

		// A fiducial is seven cells across, so anything under seven pixels wide
		// cannot resolve one.
		if w < FinderPattern || h < FinderPattern || ring.Area < 3*FinderPattern {
			continue
		}
		if aspect := float64(w) / float64(h); aspect < 0.6 || aspect > 1.7 {
			continue
		}
		// The ring should fill a good fraction of its box: 24 cells of 49, or
		// about half, when the capture is square-on.
		//
		// The lower bound has to allow for rotation. A square rotated 45 degrees
		// has a bounding box root-two wider on each side, so the same ring fills
		// only 24/98 of it — under a quarter. A bound set from the square-on
		// figure rejects every rotated capture outright, which is precisely the
		// bug an earlier revision had.
		if fill := float64(ring.Area) / float64(w*h); fill < 0.18 || fill > 0.92 {
			continue
		}

		box := ring.BoxCenter()
		core := lab.ComponentAt(int(box.X+0.5), int(box.Y+0.5))
		if core == nil || core.Label == ring.Label {
			continue
		}
		// The core must sit strictly inside the ring, not merely overlap its box.
		if core.MinX <= ring.MinX || core.MaxX >= ring.MaxX ||
			core.MinY <= ring.MinY || core.MaxY >= ring.MaxY {
			continue
		}
		// The core covers 9 cells of 49 square-on, and as little as 9 of 98 when
		// rotated 45 degrees, so the same allowance applies here.
		coreFill := float64(core.Area) / float64(w*h)
		if coreFill < 0.06 || coreFill > 0.42 {
			continue
		}

		centroid := core.Centroid()
		offset := math.Hypot(centroid.X-box.X, centroid.Y-box.Y)
		if offset > 0.20*float64(w) {
			continue
		}

		module := (float64(w) + float64(h)) / (2 * FinderPattern)

		// The ring covers 24 of the pattern's 49 cells, and the core a further 9.
		// Both together give a larger, so less noisy, area to divide by than the
		// ring alone.
		moduleArea := math.Sqrt(float64(ring.Area+core.Area) / (24 + 9))

		fillErr := math.Abs(float64(ring.Area)/float64(w*h)-finderRingFill) / finderRingFill
		coreErr := math.Abs(coreFill-finderCoreFill) / finderCoreFill
		centreErr := offset / (0.20 * float64(w))
		confidence := 1 - clamp01((fillErr+coreErr+centreErr)/3)

		out = append(out, FinderCandidate{
			Center:     centroid,
			ModuleSize: module,
			ModuleArea: moduleArea,
			Confidence: confidence,
			ring:       ring,
			core:       *core,
		})
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// SelectFinderQuad picks the four candidates most likely to be one frame's
// fiducials.
//
// Candidates are first grouped by apparent cell size, since the four fiducials of
// a single frame are the same size while stray reflections rarely match. Within
// the best group the combination enclosing the greatest area wins, which rejects
// clusters of nearby false positives in favour of the frame's actual corners.
func SelectFinderQuad(cands []FinderCandidate) ([4]FinderCandidate, error) {
	if len(cands) < 4 {
		return [4]FinderCandidate{}, ErrFindersNotFound
	}
	if len(cands) == 4 {
		return [4]FinderCandidate{cands[0], cands[1], cands[2], cands[3]}, nil
	}

	// Strongest candidates first, capped so the combination search stays bounded.
	ranked := make([]FinderCandidate, len(cands))
	copy(ranked, cands)
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Confidence != ranked[j].Confidence {
			return ranked[i].Confidence > ranked[j].Confidence
		}
		return ranked[i].ring.Area > ranked[j].ring.Area
	})
	const maxConsidered = 14
	if len(ranked) > maxConsidered {
		ranked = ranked[:maxConsidered]
	}

	best := -1.0
	var bestQuad [4]FinderCandidate
	found := false
	n := len(ranked)
	for a := 0; a < n-3; a++ {
		for b := a + 1; b < n-2; b++ {
			for c := b + 1; c < n-1; c++ {
				for d := c + 1; d < n; d++ {
					group := [4]FinderCandidate{ranked[a], ranked[b], ranked[c], ranked[d]}
					if !similarModules(group) {
						continue
					}
					pts := [4]Point{group[0].Center, group[1].Center, group[2].Center, group[3].Center}
					area := QuadArea(OrderQuad(pts))
					if area > best {
						best, bestQuad, found = area, group, true
					}
				}
			}
		}
	}
	if !found {
		return [4]FinderCandidate{}, ErrFindersNotFound
	}
	return bestQuad, nil
}

// similarModules reports whether four candidates share an apparent cell size.
// Perspective makes the far corners of a tilted display smaller, so the tolerance
// has to accommodate genuine foreshortening while still excluding unrelated blobs.
func similarModules(g [4]FinderCandidate) bool {
	lo, hi := g[0].ModuleSize, g[0].ModuleSize
	for _, c := range g[1:] {
		lo = math.Min(lo, c.ModuleSize)
		hi = math.Max(hi, c.ModuleSize)
	}
	return lo > 0 && hi/lo <= 2.0
}

// MeanModuleSize is the average apparent cell size across four fiducials.
func MeanModuleSize(q [4]FinderCandidate) float64 {
	var s float64
	for _, c := range q {
		s += c.ModuleSize
	}
	return s / 4
}
