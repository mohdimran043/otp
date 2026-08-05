package fec

import "fmt"

// Blocking describes how a transmission's chunks are grouped into error-correction blocks.
//
// It exists because the sender and the receiver have to agree on something neither can see in
// the other's data. A transmission of ten thousand chunks is not coded as one ten-thousand-shard
// block — no codec here would accept that, and the work would be quadratic — so the chunks are
// cut into blocks of DataShards and each is coded on its own. Every codec then numbers the
// shards of a block from zero: source shards 0 to k-1, repair shards k upward.
//
// But the chunks on the wire are numbered across the whole transmission, because a frame header
// carries one chunk number. So a receiver holding chunk 5312 has to work out which block it
// belongs to and what its number is *inside* that block before it can hand it to a decoder, and
// it has to reach the same answer the sender did. That translation is this type, written once
// and used from both sides, because two implementations of it would eventually disagree and the
// symptom would be a block that silently reconstructs into the wrong bytes.
type Blocking struct {
	// SourceShards is how many source chunks the whole transmission has.
	SourceShards int

	// DataShards is the source-shard count of a full block, and ParityShards how many repair
	// shards each block carries.
	DataShards   int
	ParityShards int
}

// NewBlocking builds a Blocking from the figures a manifest carries.
func NewBlocking(sourceShards, dataShards, parityShards int) Blocking {
	return Blocking{
		SourceShards: sourceShards,
		DataShards:   dataShards,
		ParityShards: parityShards,
	}
}

// Enabled reports whether there is any error correction to apply.
func (b Blocking) Enabled() bool {
	return b.DataShards > 0 && b.ParityShards > 0 && b.SourceShards > 0
}

// Blocks is how many blocks the transmission is cut into.
func (b Blocking) Blocks() int {
	if b.DataShards <= 0 || b.SourceShards <= 0 {
		return 0
	}
	return (b.SourceShards + b.DataShards - 1) / b.DataShards
}

// BlockSize is how many source shards a block holds.
//
// Every block but the last holds DataShards. The last holds the remainder, and it is coded at
// that shorter size rather than being padded up to a full block — so a decoder must use this
// figure rather than DataShards, or it will ask a codec to reconstruct shards that were never
// part of the block.
func (b Blocking) BlockSize(block int) int {
	if block < 0 || block >= b.Blocks() {
		return 0
	}
	start := block * b.DataShards
	if remaining := b.SourceShards - start; remaining < b.DataShards {
		return remaining
	}
	return b.DataShards
}

// SourceShard translates a transmission-wide source chunk number into its block and its number
// inside that block.
func (b Blocking) SourceShard(chunk int) (block, inBlock int, err error) {
	if b.DataShards <= 0 {
		return 0, 0, fmt.Errorf("%w: no block size", ErrShardGeometry)
	}
	if chunk < 0 || chunk >= b.SourceShards {
		return 0, 0, fmt.Errorf("%w: source chunk %d is outside a transmission of %d",
			ErrBadESI, chunk, b.SourceShards)
	}
	return chunk / b.DataShards, chunk % b.DataShards, nil
}

// ParityShard translates a transmission-wide parity chunk number the same way.
//
// Parity chunks are numbered after every source chunk, block by block: the first block's repair
// shards come first, then the second block's, and so on. Their number inside a block starts at
// that block's source-shard count, which is what every codec here expects.
func (b Blocking) ParityShard(chunk int) (block, inBlock int, err error) {
	if b.ParityShards <= 0 {
		return 0, 0, fmt.Errorf("%w: no parity shards", ErrShardGeometry)
	}
	offset := chunk - b.SourceShards
	if offset < 0 || offset >= b.Blocks()*b.ParityShards {
		return 0, 0, fmt.Errorf("%w: parity chunk %d is outside a transmission of %d source shards in %d blocks",
			ErrBadESI, chunk, b.SourceShards, b.Blocks())
	}
	block = offset / b.ParityShards
	return block, b.BlockSize(block) + offset%b.ParityShards, nil
}

// ParityChunk is the reverse: the transmission-wide number of a block's repair shard.
func (b Blocking) ParityChunk(block, index int) (int, error) {
	if block < 0 || block >= b.Blocks() || index < 0 || index >= b.ParityShards {
		return 0, fmt.Errorf("%w: repair shard %d of block %d does not exist",
			ErrBadESI, index, block)
	}
	return b.SourceShards + block*b.ParityShards + index, nil
}

// SourceChunk is the reverse for source shards.
func (b Blocking) SourceChunk(block, index int) (int, error) {
	if block < 0 || block >= b.Blocks() || index < 0 || index >= b.BlockSize(block) {
		return 0, fmt.Errorf("%w: source shard %d of block %d does not exist",
			ErrBadESI, index, block)
	}
	return block*b.DataShards + index, nil
}
