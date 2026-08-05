package fec_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/fec"
)

// shardSize is small enough to keep the tests quick and large enough that a codec
// which confused a shard for a symbol would produce visibly wrong output.
const shardSize = 256

// makeBlock builds k source shards of pseudo-random bytes.
func makeBlock(t *testing.T, k int, seed int64) [][]byte {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	out := make([][]byte, k)
	for i := range out {
		out[i] = make([]byte, shardSize)
		r.Read(out[i])
	}
	return out
}

// shardsOf pairs source and parity shards with the identifiers they travel under.
func shardsOf(source, parity [][]byte) []fec.Shard {
	out := make([]fec.Shard, 0, len(source)+len(parity))
	for i, s := range source {
		out = append(out, fec.Shard{ESI: uint32(i), Data: s})
	}
	for i, p := range parity {
		out = append(out, fec.Shard{ESI: uint32(len(source) + i), Data: p})
	}
	return out
}

// drop removes shards at the given positions in the combined list.
func drop(all []fec.Shard, positions ...int) []fec.Shard {
	gone := map[int]bool{}
	for _, p := range positions {
		gone[p] = true
	}
	out := make([]fec.Shard, 0, len(all))
	for i, s := range all {
		if !gone[i] {
			out = append(out, s)
		}
	}
	return out
}

func requireBlockEqual(t *testing.T, want, got [][]byte) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		require.True(t, bytes.Equal(want[i], got[i]), "shard %d differs", i)
	}
}

func TestRegistryHoldsEveryCodec(t *testing.T) {
	require.Equal(t, []string{"none", "reed-solomon", "raptorq", "ldpc"}, fec.Names())

	for _, c := range fec.All() {
		byID, err := fec.ByID(c.ID())
		require.NoError(t, err)
		require.Same(t, c, byID)

		byName, err := fec.ByName(c.Name())
		require.NoError(t, err)
		require.Same(t, c, byName)

		require.NotEmpty(t, c.Description())
		require.Positive(t, c.MaxDataShards())
		require.GreaterOrEqual(t, c.ShardsNeeded(16), 16)
	}

	_, err := fec.ByID(200)
	require.ErrorIs(t, err, fec.ErrUnknownCodec)
	_, err = fec.ByName("turbo")
	require.ErrorIs(t, err, fec.ErrUnknownCodec)
}

// TestRecoversFromLossEveryCodec is the property the whole package exists for: with
// parity present, a block survives losing shards.
//
// Each codec is held to its own contract rather than to one shared figure, because
// the three codes differ in exactly this: ShardsNeeded says how many shards a decode
// requires, so a block of k+m shards must survive losing however many that leaves
// over. Reed-Solomon survives losing all of its parity's worth; the sparse code
// survives less, and says so. Deriving the loss count from ShardsNeeded means a codec
// cannot pass this test by promising something it does not deliver.
//
// Source shards are dropped as well as parity ones, since a code that could only
// rebuild parity would be useless — the chunks are what the receiver actually needs.
func TestRecoversFromLossEveryCodec(t *testing.T) {
	const k, m = 32, 32

	for _, c := range fec.All() {
		if c.ID() == fec.IDNone {
			continue // Covered separately: it recovers nothing, by design.
		}
		t.Run(c.Name(), func(t *testing.T) {
			require.NoError(t, c.Validate(k, m))

			source := makeBlock(t, k, 1)
			parity, err := c.Encode(source, m)
			require.NoError(t, err)
			require.Len(t, parity, m)

			budget := k + m - c.ShardsNeeded(k)
			require.Positive(t, budget, "%s cannot lose anything at this geometry", c.Name())

			patterns := map[string][]int{
				"one source shard": {0},
				"one parity shard": {k},
			}
			// The shapes that stress a code differently: a burst, a scatter, all from the
			// source half, all from the parity half. Each is taken at the full budget, so
			// the codec is tested at exactly the limit it claims.
			burst := make([]int, budget)
			scatter := make([]int, budget)
			sourceOnly := make([]int, budget)
			parityOnly := make([]int, budget)
			for i := 0; i < budget; i++ {
				burst[i] = i
				scatter[i] = (i * 7) % (k + m)
				sourceOnly[i] = i % k
				parityOnly[i] = k + i%m
			}
			patterns["burst at the start"] = burst
			patterns["scattered"] = dedupe(scatter)
			patterns["all from the source half"] = dedupe(sourceOnly)
			patterns["all from the parity half"] = dedupe(parityOnly)

			for name, lost := range patterns {
				t.Run(name, func(t *testing.T) {
					got, err := c.Decode(drop(shardsOf(source, parity), lost...), k, m)
					require.NoError(t, err, "%d shards lost of %d, budget %d",
						len(lost), k+m, budget)
					requireBlockEqual(t, source, got)
				})
			}
		})
	}
}

// dedupe removes repeated positions, so a generated pattern loses as many distinct
// shards as it looks like it does.
func dedupe(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// TestSourceShardsAreNotModified matters because the sender still needs the chunks
// after encoding: they become the frames it has yet to render. A codec that
// scribbled on its input would corrupt the transmission it was protecting.
func TestSourceShardsAreNotModified(t *testing.T) {
	const k, m = 32, 16

	for _, c := range fec.All() {
		t.Run(c.Name(), func(t *testing.T) {
			source := makeBlock(t, k, 2)
			before := makeBlock(t, k, 2)

			parity := m
			if c.ID() == fec.IDNone {
				parity = 0
			}
			_, err := c.Encode(source, parity)
			require.NoError(t, err)
			requireBlockEqual(t, before, source)
		})
	}
}

// TestDecodeIsOrderIndependent checks shards may arrive in any order, which on this
// channel they routinely do: a retransmitted frame arrives long after its neighbours.
func TestDecodeIsOrderIndependent(t *testing.T) {
	const k, m = 32, 32

	for _, c := range fec.All() {
		if c.ID() == fec.IDNone {
			continue
		}
		t.Run(c.Name(), func(t *testing.T) {
			source := makeBlock(t, k, 3)
			parity, err := c.Encode(source, m)
			require.NoError(t, err)

			all := drop(shardsOf(source, parity), 2, 5, 8, 11, 14)
			shuffled := append([]fec.Shard(nil), all...)
			rand.New(rand.NewSource(9)).Shuffle(len(shuffled), func(i, j int) {
				shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
			})

			got, err := c.Decode(shuffled, k, m)
			require.NoError(t, err)
			requireBlockEqual(t, source, got)
		})
	}
}

// TestTooFewShardsIsReportedNotGuessed checks a block that cannot be recovered says
// so. The receiver answers ErrTooFewShards by asking for retransmission, so a codec
// that returned plausible-looking wrong data instead would corrupt the file silently.
func TestTooFewShardsIsReportedNotGuessed(t *testing.T) {
	const k, m = 32, 16

	for _, c := range fec.All() {
		if c.ID() == fec.IDNone {
			continue
		}
		t.Run(c.Name(), func(t *testing.T) {
			source := makeBlock(t, k, 4)
			parity, err := c.Encode(source, m)
			require.NoError(t, err)

			// More lost than the parity could ever replace: no code recovers k shards
			// from fewer than k, so this is the one loss level every codec must refuse.
			tooMany := make([]int, m+1)
			for i := range tooMany {
				tooMany[i] = i
			}
			_, err = c.Decode(drop(shardsOf(source, parity), tooMany...), k, m)
			require.ErrorIs(t, err, fec.ErrTooFewShards)

			_, err = c.Decode(nil, k, m)
			require.ErrorIs(t, err, fec.ErrTooFewShards)
		})
	}
}

// TestNoneCodecCarriesNoParity pins the deliberate behaviour of the codec that does
// nothing: it passes a complete block through and refuses an incomplete one.
func TestNoneCodecCarriesNoParity(t *testing.T) {
	source := makeBlock(t, 8, 5)

	parity, err := fec.None.Encode(source, 0)
	require.NoError(t, err)
	require.Empty(t, parity)

	got, err := fec.None.Decode(shardsOf(source, nil), len(source), 0)
	require.NoError(t, err)
	requireBlockEqual(t, source, got)

	_, err = fec.None.Decode(drop(shardsOf(source, nil), 3), len(source), 0)
	require.ErrorIs(t, err, fec.ErrTooFewShards)

	_, err = fec.None.Encode(source, 2)
	require.ErrorIs(t, err, fec.ErrShardGeometry)
}

// TestReedSolomonIsOptimal checks the property that distinguishes it: *any* k of the
// n shards suffice, with no exceptions and no margin. It is checked exhaustively over
// which shards are lost rather than on a sample, because "any k" is the entire claim.
func TestReedSolomonIsOptimal(t *testing.T) {
	const k, m = 6, 4
	source := makeBlock(t, k, 6)
	parity, err := fec.ReedSolomon.Encode(source, m)
	require.NoError(t, err)
	all := shardsOf(source, parity)

	// Every subset of exactly k shards out of k+m.
	tested := 0
	for mask := 0; mask < 1<<(k+m); mask++ {
		if popcount(mask) != k {
			continue
		}
		var kept []fec.Shard
		for i := 0; i < k+m; i++ {
			if mask&(1<<i) != 0 {
				kept = append(kept, all[i])
			}
		}
		got, err := fec.ReedSolomon.Decode(kept, k, m)
		require.NoError(t, err, "subset %b of %d shards should decode", mask, k)
		requireBlockEqual(t, source, got)
		tested++
	}
	require.Equal(t, 210, tested, "there are C(10,6) subsets to check")
}

func popcount(v int) int {
	n := 0
	for ; v != 0; v &= v - 1 {
		n++
	}
	return n
}

// TestRecoveryOverhead measures how many shards past the source count each codec
// actually needs, over many random loss patterns.
//
// This is the figure that distinguishes the three codes in practice, and it is what
// ShardsNeeded promises, so it is measured rather than asserted from theory. The
// table it prints is the one an operator needs to choose between them.
func TestRecoveryOverhead(t *testing.T) {
	const k, m, trials = 32, 32, 60

	var table strings.Builder
	table.WriteString("\nrecovery from random loss, 32 source and 32 parity shards, 60 trials each:\n")
	table.WriteString("  codec          shards received: ")
	counts := []int{32, 33, 34, 36, 40, 44, 48}
	for _, n := range counts {
		fmt.Fprintf(&table, " %5d", n)
	}
	table.WriteString("\n")

	rng := rand.New(rand.NewSource(11))
	for _, c := range fec.All() {
		if c.ID() == fec.IDNone {
			continue
		}
		source := makeBlock(t, k, 7)
		parity, err := c.Encode(source, m)
		require.NoError(t, err)
		all := shardsOf(source, parity)

		fmt.Fprintf(&table, "  %-32s", c.Name())
		successes := map[int]int{}
		for _, n := range counts {
			ok := 0
			for trial := 0; trial < trials; trial++ {
				kept := append([]fec.Shard(nil), all...)
				rng.Shuffle(len(kept), func(i, j int) { kept[i], kept[j] = kept[j], kept[i] })
				kept = kept[:n]

				got, err := c.Decode(kept, k, m)
				if err == nil {
					requireBlockEqual(t, source, got)
					ok++
				}
			}
			successes[n] = ok
			fmt.Fprintf(&table, " %4d%%", ok*100/trials)
		}
		table.WriteString("\n")

		// Whatever ShardsNeeded promises has to actually hold, for every codec, or the
		// receiver's decision about when to attempt a decode is wrong.
		need := c.ShardsNeeded(k)
		require.LessOrEqual(t, need, 48, "%s: ShardsNeeded is outside the measured range", c.Name())
		for _, n := range counts {
			if n >= need {
				require.Equal(t, trials, successes[n],
					"%s promises recovery at %d shards but failed at %d", c.Name(), need, n)
			}
		}
	}
	t.Log(table.String())
}

// TestConflictingShardsAreRefused covers a shard arriving twice with different
// contents. Every shard reaching this layer already passed CRC32 and SHA-256, so a
// conflict means something upstream is broken in a way those checks did not catch,
// and choosing one at random would let the block rebuild into plausible nonsense.
func TestConflictingShardsAreRefused(t *testing.T) {
	source := makeBlock(t, 8, 8)

	for _, c := range fec.All() {
		t.Run(c.Name(), func(t *testing.T) {
			all := shardsOf(source, nil)
			conflicting := append([]fec.Shard(nil), all...)
			tampered := append([]byte(nil), source[2]...)
			tampered[0] ^= 0xFF
			conflicting = append(conflicting, fec.Shard{ESI: 2, Data: tampered})

			_, err := c.Decode(conflicting, len(source), 0)
			require.ErrorIs(t, err, fec.ErrDuplicateShard)

			// The same shard twice with the same contents is a retransmission, which is
			// ordinary and must be accepted.
			duplicated := append(append([]fec.Shard(nil), all...), all[2])
			_, err = c.Decode(duplicated, len(source), 0)
			require.NoError(t, err)
		})
	}
}

func TestRejectsMalformedBlocks(t *testing.T) {
	for _, c := range fec.All() {
		t.Run(c.Name(), func(t *testing.T) {
			_, err := c.Encode(nil, 2)
			require.ErrorIs(t, err, fec.ErrShardGeometry)

			// Parity is computed position by position across the shards, so shards of
			// different lengths are not a block.
			ragged := [][]byte{make([]byte, 64), make([]byte, 32)}
			_, err = c.Encode(ragged, 2)
			require.ErrorIs(t, err, fec.ErrShardSize)

			_, err = c.Encode([][]byte{{}, {}}, 2)
			require.ErrorIs(t, err, fec.ErrShardSize)

			// A source block larger than the codec supports must be refused when the
			// profile is validated, not when the first block is encoded.
			require.ErrorIs(t, c.Validate(c.MaxDataShards()+1, 2), fec.ErrShardGeometry)
		})
	}
}

// TestRaptorQRecoversFromRepairSymbolsAlone is the fountain property, in its
// strongest form: the block is rebuilt from repair symbols only, with not one source
// shard among them.
//
// No block code can do this. Reed-Solomon with k data and m parity shards cannot
// recover if more than m go missing, however many parity shards arrive, because there
// are only m of them in existence. A fountain code has no such limit — repair symbols
// are generated on demand and any K of them carry the block — which is what lets a
// sender answer "I missed a lot" with "here is more" rather than with a negotiation
// over which specific shards to resend.
func TestRaptorQRecoversFromRepairSymbolsAlone(t *testing.T) {
	const k = 32
	source := makeBlock(t, k, 21)

	// Twice as many repair symbols as source shards, then decode from a window of them
	// that contains no source shard at all.
	parity, err := fec.RaptorQ.Encode(source, 2*k)
	require.NoError(t, err)
	require.Len(t, parity, 2*k)

	var repairOnly []fec.Shard
	for i, p := range parity {
		if i >= k+4 {
			break
		}
		repairOnly = append(repairOnly, fec.Shard{ESI: uint32(k + i), Data: p})
	}

	got, err := fec.RaptorQ.Decode(repairOnly, k, 2*k)
	require.NoError(t, err, "a fountain code must rebuild a block from repair symbols alone")
	requireBlockEqual(t, source, got)
}

// TestRaptorQRepairSymbolsAreDistinct checks the generator produces a different
// symbol for every identifier. Repeated repair symbols would look like protection
// while adding no information, and the failure would appear as a decode that cannot
// reach full rank no matter how many shards arrive.
func TestRaptorQRepairSymbolsAreDistinct(t *testing.T) {
	const k, m = 20, 100
	parity, err := fec.RaptorQ.Encode(makeBlock(t, k, 22), m)
	require.NoError(t, err)

	seen := map[string]int{}
	for i, p := range parity {
		key := string(p)
		if prev, dup := seen[key]; dup {
			t.Fatalf("repair symbols %d and %d are identical", prev, i)
		}
		seen[key] = i
	}
	require.Len(t, seen, m)
}

// TestRaptorQPaddingIsInvisible checks block sizes that are not in RFC 6330's table.
// Most real sizes are not: the table jumps from 10 to 12 to 18, so a block of 13
// chunks is padded to 18 internally. The padding is never transmitted, so a caller
// should see no sign of it.
func TestRaptorQPaddingIsInvisible(t *testing.T) {
	for _, k := range []int{1, 5, 11, 13, 17, 100, 255} {
		t.Run(fmt.Sprint(k), func(t *testing.T) {
			source := makeBlock(t, k, int64(k))
			m := k + 4
			parity, err := fec.RaptorQ.Encode(source, m)
			require.NoError(t, err)

			// Lose every source shard the parity could conceivably replace.
			lost := make([]int, 0, k)
			for i := 0; i < k; i++ {
				lost = append(lost, i)
			}
			got, err := fec.RaptorQ.Decode(drop(shardsOf(source, parity), lost...), k, m)
			require.NoError(t, err)
			requireBlockEqual(t, source, got)
		})
	}
}

// TestNoneCodecAcceptsAnUnsetBlockSize covers how a configuration turns error correction off.
// Requiring a block size for a codec that has no blocks would make "none" fail on a field the
// operator had no reason to set.
func TestNoneCodecAcceptsAnUnsetBlockSize(t *testing.T) {
	require.NoError(t, fec.None.Validate(0, 0), "a zero block size means no error correction")
	require.Error(t, fec.None.Validate(0, 4), "the none codec cannot produce parity")

	// Decoding is the other case: it has to say how many shards it is returning, so a zero
	// count there is a caller mistake rather than a configuration choice.
	_, err := fec.None.Decode([]fec.Shard{{ESI: 0, Data: []byte("x")}}, 0, 0)
	require.ErrorIs(t, err, fec.ErrShardGeometry)

	// And the other codecs still require a real block size, since for them it describes the
	// code itself.
	for _, c := range fec.All() {
		if c.ID() == fec.IDNone {
			continue
		}
		require.Error(t, c.Validate(0, 4), "%s needs a block size", c.Name())
	}
}

// TestBlockingTranslatesBothWays covers the arithmetic a sender and a receiver have to agree on
// without being able to see each other's data: which block a transmission-wide chunk number
// belongs to, and what its number is inside that block.
//
// Two implementations of this would eventually disagree, and the symptom would not be an error —
// it would be a block that reconstructs into the wrong bytes.
func TestBlockingTranslatesBothWays(t *testing.T) {
	// Twenty-six source chunks in blocks of eight: three full blocks and a final block of two.
	b := fec.NewBlocking(26, 8, 4)

	require.True(t, b.Enabled())
	require.Equal(t, 4, b.Blocks())
	require.Equal(t, 8, b.BlockSize(0))
	require.Equal(t, 8, b.BlockSize(2))
	require.Equal(t, 2, b.BlockSize(3), "the last block holds the remainder")
	require.Zero(t, b.BlockSize(4), "there is no fifth block")

	for _, c := range []struct{ chunk, block, inBlock int }{
		{0, 0, 0}, {7, 0, 7}, {8, 1, 0}, {23, 2, 7}, {24, 3, 0}, {25, 3, 1},
	} {
		block, inBlock, err := b.SourceShard(c.chunk)
		require.NoError(t, err)
		require.Equal(t, c.block, block, "chunk %d", c.chunk)
		require.Equal(t, c.inBlock, inBlock, "chunk %d", c.chunk)

		back, err := b.SourceChunk(block, inBlock)
		require.NoError(t, err)
		require.Equal(t, c.chunk, back, "the translation must reverse")
	}

	// Parity chunks come after every source chunk, block by block, and their number inside a
	// block starts at that block's source count — which for the short final block is not
	// DataShards.
	for _, c := range []struct{ chunk, block, inBlock int }{
		{26, 0, 8}, {29, 0, 11}, {30, 1, 8}, {38, 3, 2}, {41, 3, 5},
	} {
		block, inBlock, err := b.ParityShard(c.chunk)
		require.NoError(t, err)
		require.Equal(t, c.block, block, "parity chunk %d", c.chunk)
		require.Equal(t, c.inBlock, inBlock, "parity chunk %d", c.chunk)
	}

	first, err := b.ParityChunk(0, 0)
	require.NoError(t, err)
	require.Equal(t, 26, first)
	last, err := b.ParityChunk(3, 3)
	require.NoError(t, err)
	require.Equal(t, 41, last)

	// Out of range in either direction is an error rather than a plausible-looking answer.
	_, _, err = b.SourceShard(26)
	require.ErrorIs(t, err, fec.ErrBadESI)
	_, _, err = b.SourceShard(-1)
	require.ErrorIs(t, err, fec.ErrBadESI)
	_, _, err = b.ParityShard(25)
	require.ErrorIs(t, err, fec.ErrBadESI)
	_, _, err = b.ParityShard(42)
	require.ErrorIs(t, err, fec.ErrBadESI)
	_, err = b.ParityChunk(4, 0)
	require.ErrorIs(t, err, fec.ErrBadESI)

	// And a transmission with no error correction reports so rather than dividing by zero.
	off := fec.NewBlocking(26, 0, 0)
	require.False(t, off.Enabled())
	require.Zero(t, off.Blocks())
	_, _, err = off.SourceShard(0)
	require.ErrorIs(t, err, fec.ErrShardGeometry)
}

// TestBlockingRoundTripsThroughACodec puts the arithmetic to work: shards are labelled by
// transmission-wide chunk number, translated into block coordinates, and decoded — which is
// exactly the path a receiver takes.
func TestBlockingRoundTripsThroughACodec(t *testing.T) {
	const sourceCount, dataShards, parityShards = 26, 8, 4
	b := fec.NewBlocking(sourceCount, dataShards, parityShards)

	source := makeBlock(t, sourceCount, 31)
	parityByChunk := map[int][]byte{}

	for block := 0; block < b.Blocks(); block++ {
		size := b.BlockSize(block)
		shards := make([][]byte, size)
		for i := 0; i < size; i++ {
			chunk, err := b.SourceChunk(block, i)
			require.NoError(t, err)
			shards[i] = source[chunk]
		}

		repair, err := fec.ReedSolomon.Encode(shards, parityShards)
		require.NoError(t, err)
		for i, shard := range repair {
			chunk, err := b.ParityChunk(block, i)
			require.NoError(t, err)
			parityByChunk[chunk] = shard
		}
	}

	// Lose two source chunks from the first block and two from the short final one, then rebuild
	// each block from whatever is left plus its own parity.
	lost := map[int]bool{1: true, 4: true, 24: true, 25: true}

	for block := 0; block < b.Blocks(); block++ {
		size := b.BlockSize(block)

		var received []fec.Shard
		for i := 0; i < size; i++ {
			chunk, err := b.SourceChunk(block, i)
			require.NoError(t, err)
			if lost[chunk] {
				continue
			}
			received = append(received, fec.Shard{ESI: uint32(i), Data: source[chunk]})
		}
		for i := 0; i < parityShards; i++ {
			chunk, err := b.ParityChunk(block, i)
			require.NoError(t, err)
			_, inBlock, err := b.ParityShard(chunk)
			require.NoError(t, err)
			received = append(received, fec.Shard{ESI: uint32(inBlock), Data: parityByChunk[chunk]})
		}

		rebuilt, err := fec.ReedSolomon.Decode(received, size, parityShards)
		require.NoError(t, err, "block %d of %d shards should rebuild", block, size)

		for i := 0; i < size; i++ {
			chunk, err := b.SourceChunk(block, i)
			require.NoError(t, err)
			require.Equal(t, source[chunk], rebuilt[i],
				"block %d shard %d (chunk %d)", block, i, chunk)
		}
	}
}
