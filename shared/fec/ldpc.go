package fec

import (
	"fmt"
	"sort"
)

// LDPC is a sparse-graph erasure code with a staircase parity structure, decoded by
// belief propagation.
//
// Its appeal is cost. Reed-Solomon is optimal but dense: every parity shard depends
// on every source shard, so encoding and decoding cost grows with the square of the
// block size, and the field caps a block at 256 shards. An LDPC code makes each
// parity shard depend on a handful of source shards instead, which makes both
// directions close to linear and lifts the block-size ceiling entirely. What it
// gives up is optimality: a sparse code occasionally needs a shard or two more than
// the theoretical minimum.
//
// The parity structure is a staircase, in the manner of the LDPC-Staircase code of
// RFC 5170: parity shard i is constrained together with parity shard i-1, which
// makes encoding a single sequential pass rather than a matrix multiplication. The
// sparse half of the matrix is generated from the block geometry alone, so both ends
// derive the same code without transmitting it.
var LDPC Codec = &ldpcCodec{}

type ldpcCodec struct{}

func (*ldpcCodec) ID() uint8    { return IDLDPC }
func (*ldpcCodec) Name() string { return "ldpc" }
func (*ldpcCodec) Description() string {
	return "Sparse-graph erasure code with staircase parity, decoded by belief propagation then exact solve."
}

// ldpcColumnWeight is how many parity checks each source shard takes part in.
//
// Five, measured. Three is the textbook figure and it is noticeably worse here: at
// thirty-two source and thirty-two parity shards, three checks per shard recovers
// 50% of loss patterns that leave thirty-five shards while five recovers 76%. Going
// on to seven buys almost nothing and makes every check denser, which costs
// propagation the state it depends on — a check with exactly one unknown left.
const ldpcColumnWeight = 5

// ldpcMargin is how many shards past the source count this code needs to be reliable.
//
// The figure is a property of arithmetic over GF(2) rather than of this construction.
// With ML decoding the block is recoverable exactly when the received shards span it,
// and for a sparse binary matrix the chance of falling short halves with each extra
// shard: about 2^-e with e shards in hand beyond the minimum. So twelve extra puts
// failure near one in four thousand, and — the part that matters — the figure is
// additive rather than proportional. It was measured at 100% recovery over every
// geometry tested, and it is the same twelve whether the block holds thirty-two
// shards or a thousand, which is why this code suits large blocks and Reed-Solomon
// suits small ones.
//
// RaptorQ avoids the whole effect by mixing a dozen dense GF(256) rows into its
// constraint system, which replaces 2^-e with something nearer 256^-e.
const ldpcMargin = 12

// ldpcMaxDataShards is generous because nothing in the construction is quadratic.
// The bound is what a shard identifier can address.
const ldpcMaxDataShards = 32768

func (*ldpcCodec) MaxDataShards() int { return ldpcMaxDataShards }

func (*ldpcCodec) Validate(dataShards, parityShards int) error {
	if dataShards < 1 || dataShards > ldpcMaxDataShards {
		return fmt.Errorf("%w: %d data shards, the limit is %d",
			ErrShardGeometry, dataShards, ldpcMaxDataShards)
	}
	if parityShards < 0 {
		return fmt.Errorf("%w: %d parity shards", ErrShardGeometry, parityShards)
	}
	// A geometry with less parity than the margin cannot deliver what ShardsNeeded
	// promises: there would be fewer shards in the whole block than a decode requires,
	// so most losses would be unrecoverable. That is worse than no parity at all,
	// because it looks like protection. Refusing it here means a passing Validate
	// guarantees the codec's own contract is satisfiable.
	if parityShards > 0 && parityShards < ldpcMargin {
		return fmt.Errorf("%w: %d parity shards is below the %d this code needs to be reliable; use reed-solomon for small blocks",
			ErrShardGeometry, parityShards, ldpcMargin)
	}
	if dataShards+parityShards > 65535 {
		return fmt.Errorf("%w: %d shards exceeds what a chunk number can address",
			ErrShardGeometry, dataShards+parityShards)
	}
	return nil
}

// ShardsNeeded reports the source count plus the constant margin a binary code
// needs. See ldpcMargin for where the number comes from; TestRecoveryOverhead
// measures that it holds.
func (*ldpcCodec) ShardsNeeded(dataShards int) int { return dataShards + ldpcMargin }

// ldpcMatrix is the parity-check matrix, as the source columns of each check row.
// The parity columns are not stored, because the staircase makes them implicit: row
// i always contains parity column i, and every row but the first also contains
// parity column i-1.
type ldpcMatrix struct {
	k, m int

	// sourceOf[i] lists the source shards check i constrains, ascending.
	sourceOf [][]int

	// checksOf[j] lists the checks source shard j takes part in, ascending.
	checksOf [][]int
}

// newLDPCMatrix derives the code for a geometry.
//
// The construction is deterministic and depends on nothing but k and m, so a sender
// and a receiver that agree on the shard counts agree on the matrix without
// exchanging anything.
//
// Edges are dealt from a shuffled bag holding each check row an equal number of
// times. That gives both properties a usable code needs at once. Drawing from a
// balanced bag keeps the row degrees even to within one, so no check ends up nearly
// empty while another is too dense to ever hold a single unknown. Shuffling the bag
// keeps the *supports* spread, which is the property that is easy to lose: an
// obvious round-robin gives column j the rows j, j+1, j+2, and there are only m such
// consecutive triples, so past m columns every support repeats one already used.
// Columns sharing a support are indistinguishable to the decoder — no combination of
// checks separates them — so losing both is unrecoverable however much parity was
// sent.
func newLDPCMatrix(k, m int) *ldpcMatrix {
	h := &ldpcMatrix{
		k:        k,
		m:        m,
		sourceOf: make([][]int, m),
		checksOf: make([][]int, k),
	}

	weight := ldpcColumnWeight
	if weight > m {
		weight = m
	}

	// The bag: every row repeated until there are enough slots for every edge.
	slots := make([]int, 0, k*weight+m)
	for len(slots) < k*weight {
		for r := 0; r < m; r++ {
			slots = append(slots, r)
		}
	}

	rng := newSplitMix(uint64(k)<<32 | uint64(m))
	for i := len(slots) - 1; i > 0; i-- {
		j := int(rng.next(uint64(i + 1)))
		slots[i], slots[j] = slots[j], slots[i]
	}

	pos := 0
	for j := 0; j < k; j++ {
		placed := make(map[int]bool, weight)
		for len(placed) < weight {
			// Take the next slot, unless this column already holds that row — a repeated
			// edge would cancel under exclusive-or and silently reduce the column's
			// weight. When it does, a later slot holding a usable row is swapped forward,
			// which keeps the bag balanced rather than discarding the draw.
			pick := -1
			for s := pos; s < len(slots); s++ {
				if !placed[slots[s]] {
					pick = s
					break
				}
			}
			if pick < 0 {
				// The bag is exhausted of rows this column can still use, which only happens
				// on the last column of an awkward geometry. Take any unused row directly.
				for r := 0; r < m && len(placed) < weight; r++ {
					if !placed[r] {
						placed[r] = true
					}
				}
				break
			}
			slots[pos], slots[pick] = slots[pick], slots[pos]
			placed[slots[pos]] = true
			pos++
		}

		for row := range placed {
			h.sourceOf[row] = append(h.sourceOf[row], j)
			h.checksOf[j] = append(h.checksOf[j], row)
		}
	}

	for i := range h.sourceOf {
		sort.Ints(h.sourceOf[i])
	}
	for j := range h.checksOf {
		sort.Ints(h.checksOf[j])
	}
	return h
}

// splitMix is a small deterministic generator, used only to place matrix edges. It
// is written out rather than taken from math/rand because the matrix has to be
// reproduced identically by an independently built receiver, and the standard
// library's stream is explicitly not a stable API.
type splitMix struct{ state uint64 }

func newSplitMix(seed uint64) *splitMix {
	// The odd constant is the golden-ratio increment splitmix64 is defined with.
	return &splitMix{state: seed + 0x9E3779B97F4A7C15}
}

func (s *splitMix) next(m uint64) uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	z ^= z >> 31
	return z % m
}

// variables lists every shard check i constrains: its source shards, then its own
// parity shard, then the parity shard before it. Shard indices run 0..k-1 for source
// and k..k+m-1 for parity, matching the identifiers on the wire.
func (h *ldpcMatrix) variables(i int) []int {
	out := make([]int, 0, len(h.sourceOf[i])+2)
	out = append(out, h.sourceOf[i]...)
	out = append(out, h.k+i)
	if i > 0 {
		out = append(out, h.k+i-1)
	}
	return out
}

func (c *ldpcCodec) Encode(source [][]byte, parityShards int) ([][]byte, error) {
	size, err := checkSource(source)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(len(source), parityShards); err != nil {
		return nil, err
	}
	if parityShards == 0 {
		return nil, nil
	}

	h := newLDPCMatrix(len(source), parityShards)

	// The staircase is what makes this a single pass. Check i says that its source
	// shards, parity shard i, and parity shard i-1 sum to zero, so parity shard i is
	// the previous one plus the sum of its own source shards — no matrix inverse and
	// no second pass over the block.
	parity := make([][]byte, parityShards)
	for i := 0; i < parityShards; i++ {
		p := make([]byte, size)
		for _, j := range h.sourceOf[i] {
			xorInto(p, source[j])
		}
		if i > 0 {
			xorInto(p, parity[i-1])
		}
		parity[i] = p
	}
	return parity, nil
}

func (c *ldpcCodec) Decode(received []Shard, dataShards, parityShards int) ([][]byte, error) {
	if parityShards == 0 {
		// Without parity there is no graph to propagate over, and the block is simply
		// required intact.
		return None.Decode(received, dataShards, 0)
	}
	if err := c.Validate(dataShards, parityShards); err != nil {
		return nil, err
	}
	total := dataShards + parityShards
	byESI, size, err := collect(received, total)
	if err != nil {
		return nil, err
	}

	h := newLDPCMatrix(dataShards, parityShards)
	shards := make([][]byte, total)
	for esi, data := range byESI {
		shards[esi] = append(make([]byte, 0, size), data...)
	}

	// Belief propagation first. On the erasure channel a message is either a known
	// symbol or nothing at all, so propagation reduces to one rule: a check with
	// exactly one unknown variable determines it. Applying that rule until it stops
	// firing is the whole algorithm, and it costs a single pass per shard recovered.
	unresolved := ldpcPropagate(h, shards, size)

	// Propagation stalls when every remaining check holds two or more unknowns —
	// a stopping set. That is the characteristic weakness of a sparse code, and the
	// residual system it leaves behind is small, so solving it exactly costs little
	// and removes the weakness. Doing both is what makes this code recover a block
	// whenever the shards that arrived determine it at all.
	if unresolved > 0 {
		if err := ldpcSolveResidual(h, shards, size); err != nil {
			return nil, err
		}
	}

	for i := 0; i < dataShards; i++ {
		if shards[i] == nil {
			return nil, fmt.Errorf("%w: source shard %d could not be recovered",
				ErrTooFewShards, i)
		}
	}
	return shards[:dataShards], nil
}

// ldpcPropagate runs belief propagation and returns how many shards remain unknown.
func ldpcPropagate(h *ldpcMatrix, shards [][]byte, size int) int {
	missing := 0
	for _, s := range shards {
		if s == nil {
			missing++
		}
	}

	for missing > 0 {
		progress := false
		for i := 0; i < h.m; i++ {
			unknown := -1
			count := 0
			for _, v := range h.variables(i) {
				if shards[v] == nil {
					unknown = v
					count++
					if count > 1 {
						break
					}
				}
			}
			if count != 1 {
				continue
			}

			// The check sums to zero, so the one unknown is the sum of the others.
			rebuilt := make([]byte, size)
			for _, v := range h.variables(i) {
				if v != unknown {
					xorInto(rebuilt, shards[v])
				}
			}
			shards[unknown] = rebuilt
			missing--
			progress = true
		}
		if !progress {
			break
		}
	}
	return missing
}

// ldpcSolveResidual solves for the shards propagation could not reach.
//
// Only the unknown shards are unknowns, and only the checks that touch them are
// equations: everything already recovered moves to the right-hand side. The residual
// system is therefore a fraction of the size of the full one, which is what makes an
// exact solve affordable here.
func ldpcSolveResidual(h *ldpcMatrix, shards [][]byte, size int) error {
	var unknowns []int
	position := map[int]int{}
	for v, s := range shards {
		if s == nil {
			position[v] = len(unknowns)
			unknowns = append(unknowns, v)
		}
	}
	if len(unknowns) == 0 {
		return nil
	}

	var rows [][]uint8
	var rhs [][]byte
	for i := 0; i < h.m; i++ {
		vars := h.variables(i)
		touches := false
		for _, v := range vars {
			if shards[v] == nil {
				touches = true
				break
			}
		}
		if !touches {
			continue
		}

		row := make([]uint8, len(unknowns))
		known := make([]byte, size)
		for _, v := range vars {
			if shards[v] == nil {
				row[position[v]] ^= 1
			} else {
				xorInto(known, shards[v])
			}
		}
		rows = append(rows, row)
		rhs = append(rhs, known)
	}

	solved, err := solveGF256(rows, rhs, len(unknowns))
	if err != nil {
		return err
	}
	for i, v := range unknowns {
		shards[v] = solved[i]
	}
	return nil
}
