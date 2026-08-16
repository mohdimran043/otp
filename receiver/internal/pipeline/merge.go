package pipeline

import (
	"image/color"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/opticaltransport/otp/shared/encoding"
)

// Combining several photographs of one displayed frame into a reading none of them could give alone.
//
// A camera photographing a display gets each cell slightly wrong, and the errors are largely
// independent between shots: sensor noise, the sub-pixel phase the cell happened to land on, and
// whatever the hand did between exposures are all different each time. So the mean of several
// readings is closer to the truth than any one of them, by the usual square root — four shots halve
// the error, nine cut it to a third.
//
// Measured on real captures before any of this was written: a session where 7 frames of 64 decoded on
// their own read 12 after combining, from two or three photographs each. Five frames that no single
// photograph could read came out of the arithmetic.
//
// What it cannot do is worth stating as plainly, because it is the question everyone asks. Below the
// resolution where cells are separated at all, every shot makes the *same* error — the blur that
// merges a cell with its neighbour is optical and identical in each photograph — and averaging
// correlated errors changes nothing. Measured: at 5.4 pixels a cell, combining rescued none of 29
// frames. This raises a marginal geometry to a working one; it does not rescue an unreadable one.
//
// It deliberately involves no model. Alignment is free, because each photograph carries its own
// homography from the fiducials, and photometry is free, because each carries its own reference
// fitted from the same fiducials. What remains is a mean. The learned parts of this system sit
// downstream, in recovery, where they now work on a merged reading instead of a single noisy one.

// mergeWindow is how long a frame's photographs are collected before the accumulator is abandoned.
//
// Bounded by time rather than by count because the display moves on whether or not enough shots
// arrived: a frame that was on screen for half a second and got three photographs is finished, and
// waiting for a fourth that will never come would hold the entry for ever. Generous enough to span
// the slowest frame rate the sender chooses, short enough that an abandoned transmission does not
// leave megabytes of sums behind.
const mergeWindow = 8 * time.Second

// mergeCapacity is how many frames may be accumulating at once.
//
// Small on purpose. The sender shows one frame at a time, so in ordinary operation there is one live
// entry and perhaps a second mid-change; anything beyond that means shots are arriving for frames the
// display has long left behind, and holding them would be spending memory to average photographs of
// different things. A dense grid costs a few megabytes per entry, so this is a real bound and not a
// formality.
const mergeCapacity = 4

// merged is the running total of one displayed frame's photographs, in cell space.
//
// Cell space rather than pixel space, which is what makes this affordable: a 512 grid is a quarter of
// a million cells against twelve million pixels, and the cells are already geometry-corrected and
// already on the palette's scale, so shots taken from different distances and angles add together
// without any resampling at all.
type merged struct {
	// sum accumulates each channel, and n counts the photographs that contributed. Held per cell
	// rather than per frame because a shot may be short — a truncated reading contributes what it has
	// rather than being discarded whole.
	sum []struct{ r, g, b float64 }
	n   []float64

	// shots counts the photographs merged, for the operator's panel and the logs.
	shots int

	// first is the reading the merged symbols are verified against. Any shot of one frame carries the
	// same footer, so the first is as good as any, and keeping one avoids re-reading the image.
	first *encoding.SoftReading

	at time.Time
}

// shotKey identifies the displayed frame a photograph is of.
//
// The transmission belongs in the key and its absence was a real fault. Every transfer numbers its
// frames from zero, so with two files on the display at once, frame 5 of one and frame 5 of the other
// landed in the same accumulator and were averaged together as though they were two photographs of one
// picture. They are not: the mean of two unrelated frames reads as neither.
//
// It could not corrupt a file — a merged reading is still checked against the footer's CRC32 and
// SHA-256, so the nonsense simply failed to verify. What it did was disable merging outright whenever
// two transfers overlapped, and silently: each transfer poisoned the other's accumulator, so one of
// the two mechanisms that rescue a marginal frame stopped working exactly when the channel was busiest.
//
// The header carries the transmission, it is covered by the header's own CRC32, and the header band is
// written several times over and majority-voted — so it survives photographs whose payload does not,
// which is the same property that made the frame number usable. The earlier comment here claimed the
// frame number was "the only identifier available"; it was simply the only one being read.
type shotKey struct {
	transmission uuid.UUID
	frame        uint64
}

// merger holds the frames currently being accumulated.
type merger struct {
	mu sync.Mutex
	by map[shotKey]*merged
}

func newMerger() *merger { return &merger{by: map[shotKey]*merged{}} }

// mergeResult is what adding one photograph produced.
type mergeResult struct {
	// Symbols is the payload as the merged evidence reads it, or nil when there is nothing to add to.
	Symbols []uint32

	// Reading verifies Symbols. Same frame, so any contributing shot's footer serves.
	Reading *encoding.SoftReading

	// Shots is how many photographs stand behind this reading, including the one just added.
	Shots int
}

// Add folds one photograph of a displayed frame into that frame's running mean and returns the
// combined reading.
//
// The caller decides what to do with it. This does not verify, because verification belongs where the
// decision about a frame is made, and because a merged reading that fails is not an error — it is one
// more photograph short.
func (m *merger) Add(key shotKey, r *encoding.SoftReading) mergeResult {
	if r == nil || len(r.Cells) == 0 {
		return mergeResult{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.evictLocked()

	e := m.by[key]
	if e == nil || len(e.n) != len(r.Cells) {
		// A different cell count means the sender's geometry changed under us. Start again rather than
		// average across two shapes, which would be arithmetic on unrelated things.
		e = &merged{
			sum:   make([]struct{ r, g, b float64 }, len(r.Cells)),
			n:     make([]float64, len(r.Cells)),
			first: r,
		}
		if len(m.by) >= mergeCapacity {
			m.dropOldestLocked()
		}
		m.by[key] = e
	}
	e.at = time.Now()
	e.shots++

	for i, c := range r.Cells {
		// A plain mean, deliberately, and this was got wrong once on the way here.
		//
		// The obvious refinement is to weight each photograph by how far its reading sat from the
		// runner-up, on the reasoning that a cell read at a margin of sixty is worth more than one read
		// at two. It is circular. That margin is large precisely when the measured colour landed near a
		// palette entry, so weighting by it means believing a photograph *because* it agrees with a
		// conclusion, and it drags the mean toward whichever entry the noise happened to favour. A test
		// of three photographs each pulled toward a different neighbour — whose true mean is plainly the
		// original colour — came out as a fourth colour entirely under that weighting.
		//
		// These are independent measurements of one quantity, taken by one camera under conditions that
		// do not change between them, so they have equal variance and the best estimate is their
		// unweighted mean. Confidence is not discarded by this: it is expressed in the colours
		// themselves, since a photograph that read a cell poorly says so by returning a colour further
		// from any entry, which moves the mean less than a clean reading of the same cell would.
		e.sum[i].r += float64(c.Normalised.R)
		e.sum[i].g += float64(c.Normalised.G)
		e.sum[i].b += float64(c.Normalised.B)
		e.n[i]++
	}

	symbols := make([]uint32, len(e.n))
	for i := range e.n {
		if e.n[i] == 0 {
			symbols[i] = r.Symbols[i]
			continue
		}
		mean := color.RGBA{
			R: clamp8(e.sum[i].r / e.n[i]),
			G: clamp8(e.sum[i].g / e.n[i]),
			B: clamp8(e.sum[i].b / e.n[i]),
			A: 255,
		}
		symbols[i], _, _ = r.Palette.ValueWithMargin(mean)
	}
	return mergeResult{Symbols: symbols, Reading: e.first, Shots: e.shots}
}

// Forget drops a frame's accumulator, once it has been read and there is nothing left to improve.
func (m *merger) Forget(key shotKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.by, key)
}

// Reset abandons everything, for a new capture session.
func (m *merger) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.by = map[shotKey]*merged{}
}

// evictLocked removes entries the display has long moved past.
func (m *merger) evictLocked() {
	cutoff := time.Now().Add(-mergeWindow)
	for k, e := range m.by {
		if e.at.Before(cutoff) {
			delete(m.by, k)
		}
	}
}

// dropOldestLocked makes room, preferring to lose the frame least likely to still be on screen.
func (m *merger) dropOldestLocked() {
	var oldest shotKey
	var at time.Time
	for k, e := range m.by {
		if at.IsZero() || e.at.Before(at) {
			oldest, at = k, e.at
		}
	}
	delete(m.by, oldest)
}

func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}
