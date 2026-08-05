package fec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGF256MatchesRFC checks the generated field tables against the values RFC 6330
// publishes in Sections 5.7.3 and 5.7.4.
//
// The tables are computed rather than transcribed, which removes the risk of a typo
// and introduces a different one: the wrong primitive polynomial would produce a
// perfectly self-consistent field that is not the field the specification means, and
// every round-trip test in the package would still pass while nothing this code
// produced could be decoded by any other RaptorQ implementation. These are the
// published values that pin it.
func TestGF256MatchesRFC(t *testing.T) {
	// The opening and closing entries of OCT_EXP, and the wrap point at 255 where
	// alpha^255 returns to one.
	require.Equal(t, uint8(1), octExp[0])
	require.Equal(t, uint8(2), octExp[1])
	require.Equal(t, uint8(4), octExp[2])
	require.Equal(t, uint8(8), octExp[3])
	require.Equal(t, uint8(16), octExp[4])
	require.Equal(t, uint8(1), octExp[255])
	require.Equal(t, uint8(142), octExp[509])

	// OCT_LOG is the inverse of OCT_EXP over the non-zero elements.
	require.Equal(t, uint8(0), octLog[1])
	require.Equal(t, uint8(1), octLog[2])
	for u := 1; u < 256; u++ {
		require.Equal(t, uint8(u), octExp[octLog[u]], "log and exp disagree at %d", u)
	}

	// The field axioms the solver depends on: every non-zero element has an inverse,
	// and multiplication is associative and commutative.
	for u := 1; u < 256; u++ {
		require.Equal(t, uint8(1), gfMul(uint8(u), gfDiv(1, uint8(u))),
			"%d has no inverse", u)
		for _, v := range []uint8{1, 2, 3, 17, 128, 255} {
			require.Equal(t, gfMul(uint8(u), v), gfMul(v, uint8(u)))
			require.Equal(t, gfMul(gfMul(uint8(u), v), 7), gfMul(uint8(u), gfMul(v, 7)))
		}
	}

	require.Equal(t, uint8(0), gfMul(0, 5))
	require.Equal(t, uint8(0), gfMul(5, 0))
	require.Equal(t, uint8(0), gfDiv(0, 5))
}

// TestRQTablesHoldTheirStatedProperties checks the transcribed parameter table
// against the invariants RFC 6330 states about it.
//
// The table cannot be derived, so a transcription error cannot be caught by
// recomputing it. What can be caught is a value that contradicts something the RFC
// says must be true — and since the errors transcription actually makes are digit
// slips and dropped rows, checks on primality, ordering, and range catch most of
// them.
func TestRQTablesHoldTheirStatedProperties(t *testing.T) {
	require.Len(t, rqParams, 477, "RFC 6330's Table 2 has 477 rows")
	require.Equal(t, 10, rqParams[0].K, "the smallest supported block is 10 symbols")
	require.Equal(t, 56403, rqParams[len(rqParams)-1].K, "the largest is 56403")

	for i, p := range rqParams {
		if i > 0 {
			require.Greater(t, p.K, rqParams[i-1].K, "row %d: K' must ascend", i)
		}

		// "For each value of K', the corresponding values of S(K') and W(K') are prime
		// numbers." — Section 5.6.
		require.True(t, isPrime(p.S), "row %d: S=%d must be prime", i, p.S)
		require.True(t, isPrime(p.W), "row %d: W=%d must be prime", i, p.W)

		// The systematic index is a value found by search over a bounded range, and the
		// HDPC symbol count runs from 10 to 16 across the table.
		require.GreaterOrEqual(t, p.J, 1, "row %d", i)
		require.Less(t, p.J, 1<<16, "row %d", i)
		require.GreaterOrEqual(t, p.H, 10, "row %d", i)
		require.LessOrEqual(t, p.H, 16, "row %d", i)

		// W counts the LT symbols and cannot exceed the intermediate symbols there are.
		require.Less(t, p.W, p.K+p.S+p.H, "row %d: W must leave room for the PI symbols", i)
	}

	// The degree distribution is cumulative over a 2^20 range and must be strictly
	// increasing, or some degree would be unreachable.
	require.Equal(t, uint32(0), degreeThresholds[0])
	require.Equal(t, uint32(1<<20), degreeThresholds[30])
	for d := 1; d < len(degreeThresholds); d++ {
		require.Greater(t, degreeThresholds[d], degreeThresholds[d-1], "threshold %d", d)
	}

	for _, v := range [][256]uint32{rqV0, rqV1, rqV2, rqV3} {
		require.Len(t, v, 256)
	}
	// The first entries of each array, as published in Sections 5.5.1 to 5.5.4.
	require.Equal(t, uint32(251291136), rqV0[0])
	require.Equal(t, uint32(807385413), rqV1[0])
	require.Equal(t, uint32(1629829892), rqV2[0])
	require.Equal(t, uint32(1191369816), rqV3[0])
}

// TestRQBlockParametersAreConsistent checks the derived constants against the
// relationships Section 5.3.3.3 defines between them, at every supported block size.
func TestRQBlockParametersAreConsistent(t *testing.T) {
	for _, p := range rqParams {
		b, err := newRQBlock(p.K)
		require.NoError(t, err)

		require.Equal(t, p.K, b.Kp)
		require.Equal(t, b.Kp+b.S+b.H, b.L)
		require.Equal(t, b.L-b.W, b.P)
		require.Equal(t, b.P-b.H, b.U)
		require.Equal(t, b.W-b.S, b.B)
		require.GreaterOrEqual(t, b.P1, b.P)
		require.True(t, isPrime(b.P1))
		require.Positive(t, b.B, "K'=%d: there must be LT symbols that are not LDPC", p.K)

		// U may be zero, and is at the smallest block size: K'=10 has ten HDPC symbols
		// and exactly ten permanently inactive ones, so none of them is anything else.
		require.GreaterOrEqual(t, b.U, 0, "K'=%d", p.K)
	}
}

// TestRQPaddingChoosesTheSmallestBlock checks K' is the smallest supported size at
// least K, since padding further than necessary would spend channel on symbols that
// carry nothing.
func TestRQPaddingChoosesTheSmallestBlock(t *testing.T) {
	for _, c := range []struct{ k, want int }{
		{1, 10}, {9, 10}, {10, 10}, {11, 12}, {12, 12}, {13, 18}, {100, 101}, {1024, 1032},
	} {
		b, err := newRQBlock(c.k)
		require.NoError(t, err)
		require.Equal(t, c.want, b.Kp, "K=%d", c.k)
	}

	_, err := newRQBlock(56404)
	require.ErrorIs(t, err, ErrShardGeometry)
}

// TestRQTupleIsInRange checks the tuple generator produces values inside the bounds
// Section 5.3.5.3 states for them. A tuple outside those bounds would index past an
// intermediate symbol, and the failure would be a panic rather than a bad decode.
func TestRQTupleIsInRange(t *testing.T) {
	for _, k := range []int{10, 101, 1032} {
		b, err := newRQBlock(k)
		require.NoError(t, err)

		for X := 0; X < b.Kp+200; X++ {
			tp := b.tuple(X)
			require.GreaterOrEqual(t, tp.d, 1, "K'=%d X=%d", k, X)
			require.GreaterOrEqual(t, tp.a, 1)
			require.LessOrEqual(t, tp.a, b.W-1)
			require.GreaterOrEqual(t, tp.b, 0)
			require.Less(t, tp.b, b.W)
			require.Contains(t, []int{2, 3}, tp.d1)
			require.GreaterOrEqual(t, tp.a1, 1)
			require.LessOrEqual(t, tp.a1, b.P1-1)
			require.GreaterOrEqual(t, tp.b1, 0)
			require.Less(t, tp.b1, b.P1)

			// Every index the encoder touches has to be a real intermediate symbol.
			for _, i := range b.encIndices(tp) {
				require.GreaterOrEqual(t, i, 0)
				require.Less(t, i, b.L)
			}
		}
	}
}

// TestRQConstraintMatrixHasFullRank is the property the systematic indices exist to
// guarantee, checked directly: given all K' source symbols, the constraint system has
// exactly one solution. If a J value were transcribed wrongly, this is what would
// fail, and it would fail for that block size alone.
func TestRQConstraintMatrixHasFullRank(t *testing.T) {
	// Every block size would take too long; these span the table's shape, including
	// the first row, the ones where H changes, and the largest the codec supports.
	for _, k := range []int{10, 12, 18, 101, 256, 500, 1032} {
		b, err := newRQBlock(k)
		require.NoError(t, err)

		known := make(map[int][]byte, b.Kp)
		for i := 0; i < b.Kp; i++ {
			known[i] = []byte{byte(i), byte(i >> 8), 0x5A, 0xA5}
		}
		C, err := b.intermediate(known, 4)
		require.NoError(t, err, "K'=%d: the constraint matrix must be invertible", b.Kp)
		require.Len(t, C, b.L)

		// And the solution must actually reproduce the source symbols, which is what
		// makes the code systematic.
		for i := 0; i < b.Kp; i++ {
			require.Equal(t, known[i], b.enc(C, b.tuple(i)), "K'=%d: source symbol %d", b.Kp, i)
		}
	}
}
