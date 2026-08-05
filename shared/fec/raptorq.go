package fec

import (
	"fmt"
	"sort"
)

// RaptorQ is the fountain code of RFC 6330.
//
// It differs from Reed-Solomon in the property that matters most on this channel:
// the number of repair symbols is not fixed at encoding time. A fountain code can
// produce as many distinct repair symbols as are ever asked for, and a decoder
// recovers the block from *any* K of them plus a couple more, without caring which.
// On an optical link where the sender is still displaying frames while the receiver
// is still reporting what it missed, that is the difference between negotiating
// which parity to send and simply sending more.
//
// The implementation follows RFC 6330 exactly, including the systematic indices of
// its Table 2, so a block encoded here is the block the specification defines. The
// one deliberate departure is the decoder: the RFC describes an inactivation
// decoding schedule chosen for large source blocks, and this solves the same
// constraint system by Gaussian elimination over GF(256) instead. The result is
// identical — the system has one solution and both methods find it — but the work
// grows with the square of the block size, which is why MaxDataShards is set where
// it is.
var RaptorQ Codec = &rqCodec{}

type rqCodec struct{}

func (*rqCodec) ID() uint8    { return IDRaptorQ }
func (*rqCodec) Name() string { return "raptorq" }
func (*rqCodec) Description() string {
	return "RFC 6330 fountain code: unlimited repair symbols, and any K of them plus two rebuild the block."
}

// rqMaxDataShards bounds a source block.
//
// The specification's own tables reach 56403 source symbols, and the code would be
// correct there. The limit here is the decoder: solving the constraint system
// directly costs on the order of L^2 symbol operations, so a block of a thousand
// shards decodes in a fraction of a second while a block of fifty thousand would
// take hours. RFC 6330 anticipates this — a file is split into source blocks, and
// the maximum block size is a parameter of the scheme — so the cap is a deployment
// choice rather than a gap. Implementing the RFC's inactivation schedule would
// raise it.
const rqMaxDataShards = 1024

func (*rqCodec) MaxDataShards() int { return rqMaxDataShards }

func (c *rqCodec) Validate(dataShards, parityShards int) error {
	if dataShards < 1 || dataShards > rqMaxDataShards {
		return fmt.Errorf("%w: %d data shards, the limit is %d",
			ErrShardGeometry, dataShards, rqMaxDataShards)
	}
	if parityShards < 0 {
		return fmt.Errorf("%w: %d parity shards", ErrShardGeometry, parityShards)
	}
	// A fountain code has no ceiling on repair symbols of its own; the bound here is
	// only that an identifier has to fit the field that carries it.
	if dataShards+parityShards > 65535 {
		return fmt.Errorf("%w: %d shards exceeds what a chunk number can address",
			ErrShardGeometry, dataShards+parityShards)
	}
	return nil
}

// ShardsNeeded reports K+2.
//
// RFC 6330's design target is that K+2 symbols recover a block with probability
// better than 99.9999%, and K alone with probability about 99%. The receiver uses
// this to decide when to try: attempting at K wastes a solve on a system that
// usually does not yet have full rank, and waiting longer than K+2 wastes channel.
func (*rqCodec) ShardsNeeded(dataShards int) int { return dataShards + 2 }

// rqBlock holds the derived parameters for one extended source block, as RFC 6330
// Section 5.3.3.3 defines them.
type rqBlock struct {
	// K is the caller's source-shard count and Kp is K', the padded size drawn from
	// the specification's Table 2.
	K, Kp int

	// J is the systematic index; S, H, and W are the LDPC, HDPC, and LT symbol
	// counts.
	J, S, H, W int

	// L is the number of intermediate symbols, P the number of permanently inactive
	// ones, P1 the smallest prime at least P, U the inactive symbols that are not
	// HDPC, and B the LT symbols that are not LDPC.
	L, P, P1, U, B int
}

// newRQBlock resolves the parameters for K source symbols.
func newRQBlock(K int) (rqBlock, error) {
	// K' is the smallest supported size at least K. Padding up to it is what lets one
	// table of systematic indices serve every block size: the padding symbols are
	// zero, and both ends know they are zero, so they cost nothing on the wire.
	i := sort.Search(len(rqParams), func(i int) bool { return rqParams[i].K >= K })
	if i == len(rqParams) {
		return rqBlock{}, fmt.Errorf("%w: %d source shards exceeds RFC 6330's largest block",
			ErrShardGeometry, K)
	}
	p := rqParams[i]

	b := rqBlock{K: K, Kp: p.K, J: p.J, S: p.S, H: p.H, W: p.W}
	b.L = b.Kp + b.S + b.H
	b.P = b.L - b.W
	b.U = b.P - b.H
	b.B = b.W - b.S
	b.P1 = nextPrime(b.P)
	return b, nil
}

func nextPrime(n int) int {
	for p := n; ; p++ {
		if isPrime(p) {
			return p
		}
	}
}

func isPrime(n int) bool {
	if n < 2 {
		return false
	}
	for d := 2; d*d <= n; d++ {
		if n%d == 0 {
			return false
		}
	}
	return true
}

// rqRand is the random number generator of RFC 6330 Section 5.3.5.1.
func rqRand(y uint32, i uint32, m uint32) uint32 {
	x0 := (y + i) % 256
	x1 := (y/256 + i) % 256
	x2 := (y/65536 + i) % 256
	x3 := (y/16777216 + i) % 256
	return (rqV0[x0] ^ rqV1[x1] ^ rqV2[x2] ^ rqV3[x3]) % m
}

// rqDeg is the degree generator of RFC 6330 Section 5.3.5.2.
func (b rqBlock) rqDeg(v uint32) int {
	for d := 1; d < len(degreeThresholds); d++ {
		if v < degreeThresholds[d] {
			if d < b.W-2 {
				return d
			}
			return b.W - 2
		}
	}
	return b.W - 2
}

// rqTuple is one source tuple, from RFC 6330 Section 5.3.5.4.
type rqTuple struct {
	d, a, b, d1, a1, b1 int
}

// tuple derives the tuple for an internal symbol identifier.
func (b rqBlock) tuple(X int) rqTuple {
	A := 53591 + b.J*997
	if A%2 == 0 {
		A++
	}
	B := 10267 * (b.J + 1)
	y := uint32(uint64(B) + uint64(X)*uint64(A)) // deliberately modulo 2^32

	v := rqRand(y, 0, 1<<20)
	t := rqTuple{d: b.rqDeg(v)}
	t.a = 1 + int(rqRand(y, 1, uint32(b.W-1)))
	t.b = int(rqRand(y, 2, uint32(b.W)))
	if t.d < 4 {
		t.d1 = 2 + int(rqRand(uint32(X), 3, 2))
	} else {
		t.d1 = 2
	}
	t.a1 = 1 + int(rqRand(uint32(X), 4, uint32(b.P1-1)))
	t.b1 = int(rqRand(uint32(X), 5, uint32(b.P1)))
	return t
}

// encIndices lists the intermediate symbols an encoding symbol is the sum of,
// following the encoding symbol generator of RFC 6330 Section 5.3.5.3.
//
// The generator walks two strides — one over the LT symbols, one over the
// permanently inactive ones — and may land on the same symbol twice. Since addition
// here is exclusive-or, a symbol visited twice cancels itself, so the result is
// accumulated as a parity per index rather than as a list of visits. Treating those
// repeats as two separate additions would produce a row that is not the row the
// specification defines.
func (b rqBlock) encIndices(t rqTuple) []int {
	touched := make(map[int]bool, t.d+t.d1)
	flip := func(i int) {
		if touched[i] {
			delete(touched, i)
			return
		}
		touched[i] = true
	}

	bb := t.b
	flip(bb)
	for j := 1; j < t.d; j++ {
		bb = (bb + t.a) % b.W
		flip(bb)
	}

	b1 := t.b1
	for b1 >= b.P {
		b1 = (b1 + t.a1) % b.P1
	}
	flip(b.W + b1)
	for j := 1; j < t.d1; j++ {
		b1 = (b1 + t.a1) % b.P1
		for b1 >= b.P {
			b1 = (b1 + t.a1) % b.P1
		}
		flip(b.W + b1)
	}

	out := make([]int, 0, len(touched))
	for i := range touched {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

// enc builds one encoding symbol from the intermediate symbols.
func (b rqBlock) enc(C [][]byte, t rqTuple) []byte {
	out := make([]byte, len(C[0]))
	for _, i := range b.encIndices(t) {
		xorInto(out, C[i])
	}
	return out
}

// constraintRows returns the S+H pre-coding rows of the matrix A, from RFC 6330
// Section 5.3.3.3. Each row is a full-width vector of GF(256) coefficients.
//
// These rows are what make RaptorQ a *pre-coded* fountain code rather than a plain
// one. The LDPC rows are sparse and cheap; the HDPC rows are dense and expensive,
// but there are only ten to twelve of them, and together they raise the probability
// that any K+2 symbols suffice from around 99% to better than one failure in a
// million.
func (b rqBlock) constraintRows() [][]uint8 {
	rows := make([][]uint8, b.S+b.H)
	for i := range rows {
		rows[i] = make([]uint8, b.L)
	}

	// The LDPC rows: each of the S LDPC symbols, plus the three LT symbols and two
	// permanently inactive symbols that must sum with it to zero.
	for i := 0; i < b.S; i++ {
		rows[i][b.B+i] ^= 1
	}
	for i := 0; i < b.B; i++ {
		a := 1 + i/b.S
		bb := i % b.S
		rows[bb][i] ^= 1
		bb = (bb + a) % b.S
		rows[bb][i] ^= 1
		bb = (bb + a) % b.S
		rows[bb][i] ^= 1
	}
	for i := 0; i < b.S; i++ {
		rows[i][b.W+i%b.P] ^= 1
		rows[i][b.W+(i+1)%b.P] ^= 1
	}

	// The HDPC rows are G_HDPC = MT * GAMMA, followed by the identity on the H HDPC
	// symbols. GAMMA is lower triangular with GAMMA[i][j] = alpha^(i-j), so the
	// product is accumulated right to left by the recurrence
	//
	//	G[j] = MT[j] + alpha * G[j+1]
	//
	// which costs one multiplication per entry instead of summing a column at a time.
	width := b.Kp + b.S
	for i := 0; i < b.H; i++ {
		row := rows[b.S+i]
		var carry uint8
		for j := width - 1; j >= 0; j-- {
			var mt uint8
			if j == width-1 {
				mt = gfPow(i)
			} else {
				r1 := int(rqRand(uint32(j+1), 6, uint32(b.H)))
				r2 := (r1 + int(rqRand(uint32(j+1), 7, uint32(b.H-1))) + 1) % b.H
				if i == r1 || i == r2 {
					mt = 1
				}
			}
			carry = mt ^ gfMul(2, carry)
			row[j] = carry
		}
		row[b.Kp+b.S+i] ^= 1
	}
	return rows
}

// encRow builds the coefficient row for the encoding symbol with internal
// identifier X.
func (b rqBlock) encRow(X int) []uint8 {
	row := make([]uint8, b.L)
	for _, i := range b.encIndices(b.tuple(X)) {
		row[i] ^= 1
	}
	return row
}

// intermediate solves A*C = D for the intermediate symbols, given the symbols that
// are known and which internal identifier each belongs to.
//
// This is the one operation both encoding and decoding rest on. Encoding knows all
// K' source symbols and solves for C so that repair symbols can be generated from
// it; decoding knows some arbitrary subset of source and repair symbols and solves
// the same system. That the two are the same computation is the reason a fountain
// code needs no separate decoder for the case where nothing was lost.
func (b rqBlock) intermediate(known map[int][]byte, symbolSize int) ([][]byte, error) {
	rows := b.constraintRows()

	// The constraint rows sum to zero, so their right-hand sides are zero symbols.
	rhs := make([][]byte, len(rows))
	for i := range rhs {
		rhs[i] = make([]byte, symbolSize)
	}

	ids := make([]int, 0, len(known))
	for X := range known {
		ids = append(ids, X)
	}
	sort.Ints(ids)
	for _, X := range ids {
		rows = append(rows, b.encRow(X))
		rhs = append(rhs, append(make([]byte, 0, symbolSize), known[X]...))
	}

	C, err := solveGF256(rows, rhs, b.L)
	if err != nil {
		return nil, err
	}
	return C, nil
}

// solveGF256 solves an over-determined linear system over GF(256) for unknowns
// unknowns, by Gaussian elimination with the symbol vectors carried along.
//
// The rows are consumed destructively, which is why callers build them fresh. The
// search for a pivot walks every remaining row rather than only the diagonal,
// because the rows arrive in whatever order symbols did and there is no reason for
// the useful ones to be in position.
func solveGF256(rows [][]uint8, rhs [][]byte, unknowns int) ([][]byte, error) {
	if len(rows) < unknowns {
		return nil, fmt.Errorf("%w: %d equations for %d unknowns",
			ErrTooFewShards, len(rows), unknowns)
	}

	for col := 0; col < unknowns; col++ {
		pivot := -1
		for r := col; r < len(rows); r++ {
			if rows[r][col] != 0 {
				pivot = r
				break
			}
		}
		if pivot < 0 {
			// No remaining equation constrains this unknown, so the system is
			// rank-deficient: the symbols that arrived, however many, do not determine
			// the block. More symbols will fix it, which is what retransmission is for.
			return nil, fmt.Errorf("%w: the constraint system is rank-deficient at symbol %d",
				ErrTooFewShards, col)
		}
		rows[col], rows[pivot] = rows[pivot], rows[col]
		rhs[col], rhs[pivot] = rhs[pivot], rhs[col]

		if inv := rows[col][col]; inv != 1 {
			scale := gfDiv(1, inv)
			for c := col; c < unknowns; c++ {
				rows[col][c] = gfMul(rows[col][c], scale)
			}
			mulInto(rhs[col], scale)
		}

		for r := col + 1; r < len(rows); r++ {
			f := rows[r][col]
			if f == 0 {
				continue
			}
			for c := col; c < unknowns; c++ {
				rows[r][c] ^= gfMul(f, rows[col][c])
			}
			mulAddInto(rhs[r], rhs[col], f)
		}
	}

	// Back-substitute. Only the first `unknowns` rows matter now; any extra equations
	// were redundant, which is the normal case for a fountain code given more symbols
	// than it strictly needed.
	for col := unknowns - 1; col >= 0; col-- {
		for r := 0; r < col; r++ {
			if f := rows[r][col]; f != 0 {
				mulAddInto(rhs[r], rhs[col], f)
				rows[r][col] = 0
			}
		}
	}
	return rhs[:unknowns], nil
}

func (c *rqCodec) Encode(source [][]byte, parityShards int) ([][]byte, error) {
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

	b, err := newRQBlock(len(source))
	if err != nil {
		return nil, err
	}

	// The extended source block is the caller's shards followed by K'-K zero symbols.
	// Those padding symbols are never transmitted: the receiver knows they are zero
	// because it knows K and K', so they cost nothing and are free constraints on the
	// decode.
	known := make(map[int][]byte, b.Kp)
	for i, s := range source {
		known[i] = s
	}
	zero := make([]byte, size)
	for i := len(source); i < b.Kp; i++ {
		known[i] = zero
	}

	C, err := b.intermediate(known, size)
	if err != nil {
		return nil, err
	}

	// Repair symbols take the internal identifiers immediately after the extended
	// block, which is what makes them distinct from every source symbol and from each
	// other.
	out := make([][]byte, parityShards)
	for i := range out {
		out[i] = b.enc(C, b.tuple(b.Kp+i))
	}
	return out, nil
}

func (c *rqCodec) Decode(received []Shard, dataShards, parityShards int) ([][]byte, error) {
	if err := c.Validate(dataShards, parityShards); err != nil {
		return nil, err
	}
	byESI, size, err := collect(received, dataShards+parityShards)
	if err != nil {
		return nil, err
	}

	b, err := newRQBlock(dataShards)
	if err != nil {
		return nil, err
	}

	// Encoding symbol identifiers become internal ones: source symbols keep their
	// number, and repair symbols shift past the padding. This mapping is the only
	// place the padding is visible on the receiving side.
	known := make(map[int][]byte, len(byESI)+b.Kp-b.K)
	for esi, data := range byESI {
		X := int(esi)
		if X >= b.K {
			X += b.Kp - b.K
		}
		known[X] = data
	}
	zero := make([]byte, size)
	for i := b.K; i < b.Kp; i++ {
		known[i] = zero
	}

	if len(byESI) < dataShards {
		return nil, fmt.Errorf("%w: %d of at least %d shards arrived",
			ErrTooFewShards, len(byESI), dataShards)
	}

	C, err := b.intermediate(known, size)
	if err != nil {
		return nil, err
	}

	// The source symbols are recovered the same way any other symbol is. That is what
	// "systematic" means here: source symbol i is the encoding symbol for identifier
	// i, so the decoder needs no special case for the ones that arrived intact.
	out := make([][]byte, dataShards)
	for i := range out {
		out[i] = b.enc(C, b.tuple(i))
	}
	return out, nil
}
