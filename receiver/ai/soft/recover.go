// Package soft recovers a frame whose geometry resolved and whose payload did not verify, by
// correcting the cell decisions the sampler was least sure about.
//
// The idea rests on one property of this protocol: the footer carries both a CRC32 and a SHA-256
// of the payload, so a corrected payload can be *confirmed* rather than merely made plausible. A
// candidate that satisfies both is the frame — a false accept would need a simultaneous collision
// in both functions — which is what makes it safe to try many candidates and stop at the first
// that passes.
//
// It is aimed at a specific, measured regime. On this project's captures the mean distance to the
// nearest colour8 palette entry was 53 against a separation of 86: not noise swamping the signal,
// but a channel sitting on the decision boundary where a few cells per frame are coin tosses and
// the rest are comfortable. Those frames fail their payload CRC and are discarded whole today.
// They are also exactly the frames a bounded search over the least confident cells recovers.
//
// What it cannot do is invent information. A frame with two hundred wrong cells is beyond any
// bounded search, and one whose fiducials were never found has no geometry to search at. Both are
// refused rather than approximated.
package soft

import (
	"container/heap"
	"errors"
	"image"
	"math/bits"
	"sort"
	"time"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// ErrNotRecovered means no candidate within the configured bounds satisfied the footer.
var ErrNotRecovered = errors.New("soft: no candidate correction verified")

// sunkCostFloor is how many candidates are tried before the budget is allowed to stop the search.
//
// Sampling the payload region dominates the cost at a large grid — about 118 ms of a 121 ms
// recovery at grid 1024, against roughly 3 ms for the candidates themselves. Once that read is
// done it cannot be un-spent, so honouring a budget that the read alone exhausted would pay the
// full price for nothing. Sixty-four candidates, in cost order, covers every one-cell correction
// and most two-cell ones at the default twelve-cell search set, which is the bulk of what is
// recoverable at all.
//
// The consequence is honest and worth stating: at a large grid the effective floor on a failed
// frame's cost is the read, not the budget. Bounding that would mean not attempting recovery at
// all, or sharing the sampling with the decode that already failed — which is a worthwhile change
// and a larger one than this.
const sunkCostFloor = 64

// Options bounds the search.
type Options struct {
	// MaxCells is how many of the least confident cells may be corrected. The search space is
	// 2^MaxCells, so this is the parameter that decides the ceiling on the work.
	MaxCells int

	// MaxCandidates caps how many corrections are tried, whatever MaxCells allows. It keeps the
	// search deterministic: on a fast machine the budget below may never bite, and a bound that
	// depends on how busy the host was makes two runs incomparable.
	MaxCandidates int

	// Budget is the wall-clock ceiling on one frame's search, and it is the bound that actually
	// makes this work across the whole grid range the sender offers.
	//
	// A candidate costs one unpack and two hashes over the entire payload, so its cost scales
	// with the payload — a couple of kilobytes at grid 80, hundreds of kilobytes at grid 1024. A
	// fixed candidate count therefore means tens of milliseconds at one end of the sender's
	// dropdown and over a second at the other, on the decode path, per failed frame. Counting
	// candidates bounds the search only in a unit nobody cares about; counting time bounds it in
	// the one that matters, and does so identically at every grid.
	//
	// Zero means no time bound, leaving MaxCandidates as the only limit. Useful for a benchmark
	// that wants every grid given the same number of attempts rather than the same number of
	// milliseconds.
	Budget time.Duration
}

// DefaultOptions is twelve cells, four thousand candidates, and fifty milliseconds.
//
// Twelve because 2^12 is 4096 sequences; beyond that the space doubles per cell while the chance
// that exactly so many cells are simultaneously marginal falls faster, so it buys progressively
// less. Fifty milliseconds because a frame that reached this point has already failed and its
// chunk will otherwise be retransmitted, and because the receiver decodes on several workers at
// once — so the budget is per frame, not per second, and a channel failing at ten frames a second
// still leaves the pipeline the majority of its time.
func DefaultOptions() Options {
	return Options{MaxCells: 12, MaxCandidates: 4096, Budget: 50 * time.Millisecond}
}

// Result describes a recovery.
type Result struct {
	// Frame is the verified frame.
	Frame *protocol.Frame

	// Flips is how many cell decisions had to be corrected. Zero means the frame verified as
	// read, which happens when a caller retries something that was never broken.
	Flips int

	// Candidates is how many corrections were tried before this one verified, and Considered how
	// many cells were in the search set. Both are logged per recovery: a rising candidate count
	// across a session is the earliest sign a camera is drifting.
	Candidates int
	Considered int

	// WorstMargin is the smallest margin in the search set, in the palette's weighted units.
	// Compared against the palette separation it says how close to hopeless the frame was.
	WorstMargin float64

	// Elapsed is how long the search took.
	Elapsed time.Duration
}

// Recover attempts to correct a frame at an already-resolved geometry.
//
// The search is ordered by total margin rather than by number of flips. A single correction to a
// cell the sampler was confident about is *less* likely than two corrections to cells it was
// guessing at, so ordering by confidence finds the real answer sooner than ordering by edit
// distance would — and because the first verified candidate is accepted as final, the order
// decides how much work an average recovery costs.
func Recover(g *protocol.Geometry, img image.Image, opts Options) (*Result, error) {
	if opts.MaxCells <= 0 || opts.MaxCandidates <= 0 {
		return nil, ErrNotRecovered
	}

	// The clock starts here, not at the candidate loop, because the budget has to bound what the
	// caller actually waits for. Sampling the payload region and selecting the marginal cells is
	// not free at a large grid — measured at grid 512, a 20 ms budget applied to the loop alone
	// returned after 46 ms, because two thirds of the time had already gone before the loop began.
	// A bound that only covers the part after the expensive setup is not a bound.
	started := time.Now()

	r, err := encoding.SoftRead(g, img)
	if err != nil {
		return nil, err
	}

	// A frame that already verifies is returned at once. The decode path calls this only after a
	// failure, but Recover is also what a benchmark and an operator's replay call directly, and
	// re-searching a good frame would be both wasteful and misleading.
	if frame, err := r.Verify(r.Symbols); err == nil {
		return &Result{Frame: frame, Flips: 0, Candidates: 1, Elapsed: time.Since(started)}, nil
	}

	set := leastConfident(r.Cells, opts.MaxCells)
	k := len(set)
	if k == 0 {
		return nil, ErrNotRecovered
	}

	// No early exit here, even when the read has already spent the whole budget.
	//
	// Measured at grid 1024: sampling the payload region costs about 118 ms and the candidate loop
	// that follows costs about 3 ms. Refusing at this point would pay the entire price and return
	// nothing, which is strictly worse than either trying or not starting. The read is sunk, and
	// what remains is cheap, so the loop below guarantees a floor of candidates before the budget
	// can stop it.

	// Every non-empty subset of the search set, ordered by the total margin it spends. Built in
	// full and sorted rather than explored with a priority queue: 2^12 masks is a few thousand
	// entries, and a sort is easier to be sure of than a heap.
	type candidate struct {
		mask uint32
		cost float64
	}
	masks := make([]candidate, 0, (1<<uint(k))-1)
	for m := uint32(1); m < 1<<uint(k); m++ {
		var cost float64
		for b := 0; b < k; b++ {
			if m&(1<<uint(b)) != 0 {
				cost += set[b].Margin
			}
		}
		masks = append(masks, candidate{mask: m, cost: cost})
	}
	sort.Slice(masks, func(i, j int) bool {
		if masks[i].cost != masks[j].cost {
			return masks[i].cost < masks[j].cost
		}
		// Ties broken toward fewer flips, the more likely of two equally costly explanations.
		return bits.OnesCount32(masks[i].mask) < bits.OnesCount32(masks[j].mask)
	})

	trial := make([]uint32, len(r.Symbols))
	tried := 0

	// The budget is checked every candidate rather than every few, because at a large grid a
	// single candidate is already several hundred microseconds and reading a clock is nanoseconds.
	// At a small grid the check is proportionally more overhead and still negligible against an
	// unpack plus two hashes.
	for _, c := range masks {
		if tried >= opts.MaxCandidates {
			break
		}
		if tried >= sunkCostFloor && opts.Budget > 0 && time.Since(started) >= opts.Budget {
			return nil, ErrNotRecovered
		}
		tried++

		copy(trial, r.Symbols)
		for b := 0; b < k; b++ {
			if c.mask&(1<<uint(b)) != 0 {
				trial[set[b].Index] = set[b].Second
			}
		}
		frame, err := r.Verify(trial)
		if err != nil {
			continue
		}
		return &Result{
			Frame:       frame,
			Flips:       bits.OnesCount32(c.mask),
			Candidates:  tried,
			Considered:  k,
			WorstMargin: set[0].Margin,
			Elapsed:     time.Since(started),
		}, nil
	}
	return nil, ErrNotRecovered
}

// leastConfident returns the k cells with the smallest margins, ascending.
//
// A bounded max-heap rather than a sort, because the grid range this has to cover spans four
// orders of magnitude in cell count. Grid 80 has about six thousand payload cells and grid 1024
// has about a million; sorting a million float64s to read off twelve of them costs on the order of
// a hundred milliseconds, which would be several times the entire search it exists to set up. One
// pass keeping the k smallest is O(n log k) and is unmeasurable at either end.
//
// The returned slice is sorted ascending, because Recover reports set[0].Margin as the worst
// margin and the mask cost ordering reads more naturally against a sorted set.
func leastConfident(cells []encoding.SoftCell, k int) []encoding.SoftCell {
	if k <= 0 || len(cells) == 0 {
		return nil
	}
	if k > len(cells) {
		k = len(cells)
	}

	// A max-heap of the k smallest seen so far: the root is the largest of the keepers, so a new
	// cell either beats it and replaces it or is discarded.
	h := make(worstFirst, 0, k)
	for _, c := range cells {
		if len(h) < k {
			heap.Push(&h, c)
			continue
		}
		if c.Margin < h[0].Margin {
			h[0] = c
			heap.Fix(&h, 0)
		}
	}

	out := make([]encoding.SoftCell, len(h))
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(&h).(encoding.SoftCell)
	}
	return out
}

// worstFirst is a max-heap on margin, used only by leastConfident.
type worstFirst []encoding.SoftCell

func (w worstFirst) Len() int           { return len(w) }
func (w worstFirst) Less(i, j int) bool { return w[i].Margin > w[j].Margin }
func (w worstFirst) Swap(i, j int)      { w[i], w[j] = w[j], w[i] }
func (w *worstFirst) Push(x any)        { *w = append(*w, x.(encoding.SoftCell)) }
func (w *worstFirst) Pop() any {
	old := *w
	n := len(old)
	v := old[n-1]
	*w = old[:n-1]
	return v
}
