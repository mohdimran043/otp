package fec

import (
	"fmt"

	"github.com/klauspost/reedsolomon"
)

// None is the codec that adds no parity.
//
// It is the right choice more often than it looks. Parity costs channel time in
// exact proportion to how much of it there is, and on a clean optical path — a
// well-sited camera in controlled light, where the envelope tests show every frame
// decoding — that time buys nothing, because the retransmission it would prevent
// almost never happens. Acknowledgements and retransmission already handle loss
// correctly; error correction only makes it cheaper.
var None Codec = &noneCodec{}

type noneCodec struct{}

func (*noneCodec) ID() uint8    { return IDNone }
func (*noneCodec) Name() string { return "none" }
func (*noneCodec) Description() string {
	return "No error correction. Lost chunks are recovered by retransmission alone."
}

// MaxDataShards is unbounded in practice, since without parity a block is just a
// list of chunks. The figure is the largest block the manifest's shard counts can
// describe.
func (*noneCodec) MaxDataShards() int { return 65535 }

func (*noneCodec) Validate(dataShards, parityShards int) error {
	if dataShards < 1 || dataShards > 65535 {
		return fmt.Errorf("%w: %d data shards", ErrShardGeometry, dataShards)
	}
	if parityShards != 0 {
		return fmt.Errorf("%w: the none codec cannot produce %d parity shards",
			ErrShardGeometry, parityShards)
	}
	return nil
}

func (c *noneCodec) Encode(source [][]byte, parityShards int) ([][]byte, error) {
	if _, err := checkSource(source); err != nil {
		return nil, err
	}
	if err := c.Validate(len(source), parityShards); err != nil {
		return nil, err
	}
	return nil, nil
}

func (c *noneCodec) Decode(received []Shard, dataShards, parityShards int) ([][]byte, error) {
	if err := c.Validate(dataShards, parityShards); err != nil {
		return nil, err
	}
	byESI, size, err := collect(received, dataShards)
	if err != nil {
		return nil, err
	}

	out := make([][]byte, dataShards)
	for i := range out {
		data, ok := byESI[uint32(i)]
		if !ok {
			return nil, fmt.Errorf("%w: shard %d of %d is missing and there is no parity to rebuild it from",
				ErrTooFewShards, i, dataShards)
		}
		out[i] = append(make([]byte, 0, size), data...)
	}
	return out, nil
}

func (*noneCodec) ShardsNeeded(dataShards int) int { return dataShards }

// ReedSolomon is the optimal erasure code: any dataShards of the
// dataShards+parityShards shards reconstruct the block, and no code can do better
// at that redundancy.
//
// The cost of that optimality is its ceiling. The code works over GF(256), so a
// block cannot exceed 256 shards in total, and its decode work grows with the
// square of the block size. For the block sizes a transmission actually uses —
// tens of chunks, sized so one chunk is one frame — that is exactly the right
// trade, and it is the recommended default whenever the channel loses frames at all.
var ReedSolomon Codec = &rsCodec{}

type rsCodec struct{}

func (*rsCodec) ID() uint8    { return IDReedSolomon }
func (*rsCodec) Name() string { return "reed-solomon" }
func (*rsCodec) Description() string {
	return "Optimal erasure code over GF(256): any k of n shards rebuild the block, up to 256 shards."
}

// rsMaxShards is the total shard count GF(256) admits.
const rsMaxShards = 256

func (*rsCodec) MaxDataShards() int { return rsMaxShards - 1 }

func (*rsCodec) Validate(dataShards, parityShards int) error {
	if dataShards < 1 {
		return fmt.Errorf("%w: %d data shards", ErrShardGeometry, dataShards)
	}
	if parityShards < 0 {
		return fmt.Errorf("%w: %d parity shards", ErrShardGeometry, parityShards)
	}
	if total := dataShards + parityShards; total > rsMaxShards {
		return fmt.Errorf("%w: %d data plus %d parity exceeds the %d shards GF(256) admits",
			ErrShardGeometry, dataShards, parityShards, rsMaxShards)
	}
	return nil
}

func (c *rsCodec) Encode(source [][]byte, parityShards int) ([][]byte, error) {
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

	enc, err := reedsolomon.New(len(source), parityShards)
	if err != nil {
		return nil, err
	}

	// The library encodes in place across one slice of data and parity shards, so the
	// source shards are copied in rather than handed over: a codec that modified its
	// caller's chunks would be a trap for the pipeline that reuses them for the
	// frames it still has to render.
	shards := make([][]byte, len(source)+parityShards)
	for i, s := range source {
		shards[i] = append(make([]byte, 0, size), s...)
	}
	for i := len(source); i < len(shards); i++ {
		shards[i] = make([]byte, size)
	}
	if err := enc.Encode(shards); err != nil {
		return nil, err
	}
	return shards[len(source):], nil
}

func (c *rsCodec) Decode(received []Shard, dataShards, parityShards int) ([][]byte, error) {
	if err := c.Validate(dataShards, parityShards); err != nil {
		return nil, err
	}
	total := dataShards + parityShards
	byESI, size, err := collect(received, total)
	if err != nil {
		return nil, err
	}

	shards := make([][]byte, total)
	present := 0
	for esi, data := range byESI {
		shards[esi] = append(make([]byte, 0, size), data...)
		present++
	}
	if present < dataShards {
		return nil, fmt.Errorf("%w: %d of %d shards arrived", ErrTooFewShards, present, dataShards)
	}

	if parityShards == 0 {
		// Nothing to reconstruct from; every source shard must simply be present.
		for i, s := range shards {
			if s == nil {
				return nil, fmt.Errorf("%w: shard %d is missing and no parity was sent",
					ErrTooFewShards, i)
			}
		}
		return shards, nil
	}

	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil, err
	}
	if err := enc.ReconstructData(shards); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrTooFewShards, err)
	}
	return shards[:dataShards], nil
}

func (*rsCodec) ShardsNeeded(dataShards int) int { return dataShards }

func init() {
	for _, c := range []Codec{None, ReedSolomon, RaptorQ, LDPC} {
		Register(c)
	}
}
