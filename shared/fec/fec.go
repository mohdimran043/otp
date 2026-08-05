// Package fec implements the error-correcting codes a transmission may use,
// behind a plug-in interface.
//
// The optical channel loses whole frames, not individual bits. A frame either
// passes its CRC32 and SHA-256 and yields a chunk, or it fails and yields nothing
// at all — a tear across a fiducial, a hand passing in front of the camera, a
// refresh caught mid-scan. So every codec here is an *erasure* code: it recovers
// shards that went missing, and never has to repair a shard that arrived wrong.
//
// That is what error correction buys on this channel. Without it, a lost chunk
// costs a full round trip: the receiver has to notice the gap, write an
// acknowledgement to shared storage, the sender has to see it and schedule a
// retransmission, and the frame has to come round again. With parity shards, the
// receiver reconstructs the chunk from frames it already has and the round trip
// never happens. On a channel whose latency is measured in frames displayed, that
// is the difference between a transmission that finishes and one that thrashes.
package fec

import (
	"errors"
	"fmt"

	"github.com/opticaltransport/otp/shared/internal/registry"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Codec identifiers, carried in the frame header and the manifest. They are wire
// values: never renumber them.
const (
	IDNone        uint8 = 0
	IDReedSolomon uint8 = 1
	IDRaptorQ     uint8 = 2
	IDLDPC        uint8 = 3
)

// Errors returned by codecs.
var (
	// ErrUnknownCodec means no codec is registered under that name or id.
	ErrUnknownCodec = errors.New("fec: unknown codec")

	// ErrShardGeometry means the requested data and parity counts are not something
	// the codec can encode.
	ErrShardGeometry = errors.New("fec: unsupported shard geometry")

	// ErrShardSize means the shards were not all the same non-zero length. Every
	// code here works on a block of equal-length shards, because parity is computed
	// position by position across them.
	ErrShardSize = errors.New("fec: shards must all be the same non-zero length")

	// ErrTooFewShards means not enough shards arrived to reconstruct the block. It
	// is the ordinary outcome of a bad stretch of channel, and the receiver answers
	// it by asking for retransmission rather than by failing the transmission.
	ErrTooFewShards = errors.New("fec: too few shards to reconstruct the block")

	// ErrDuplicateShard means the same shard arrived twice with different contents,
	// which means one of them is corrupt in a way its own checksum did not catch.
	ErrDuplicateShard = errors.New("fec: conflicting copies of the same shard")

	// ErrBadESI means a shard's identifier is outside the block it claims to be part
	// of.
	ErrBadESI = errors.New("fec: shard identifier outside the block")
)

// Shard is one unit of an error-coded block, as it travels and as it arrives.
//
// ESI is the Encoding Symbol Identifier, borrowed from RFC 6330 because the
// fountain code needs it and the others are happy with it: identifiers below the
// source-shard count name source shards, and identifiers at or above it name repair
// shards. It travels in the frame header's chunk number, and the parity flag says
// which side of the line it falls on.
type Shard struct {
	ESI  uint32
	Data []byte
}

// Codec is the plug-in interface every error-correcting code implements.
type Codec interface {
	// ID is the wire identifier written into the frame header and the manifest.
	ID() uint8

	// Name is the stable configuration name, such as "raptorq".
	Name() string

	// Description is one line for the profile UI and generated documentation.
	Description() string

	// MaxDataShards is the largest source block this codec can encode. It differs
	// sharply between codes — Reed-Solomon is bounded by its field, the fountain
	// code by its parameter table — and the sender needs it to decide how to split a
	// file into blocks.
	MaxDataShards() int

	// Validate reports whether the codec can work at this geometry, so a profile can
	// be rejected when it is configured rather than when it is first used.
	Validate(dataShards, parityShards int) error

	// Encode returns the repair shards for a block of equal-length source shards.
	// The source shards are not modified.
	Encode(source [][]byte, parityShards int) ([][]byte, error)

	// Decode reconstructs the dataShards source shards from whatever arrived.
	//
	// Shards may arrive in any order, with any subset missing, and the source shards
	// themselves may be among the missing — that is the whole point. It returns
	// ErrTooFewShards if the block cannot be recovered, which is a signal to ask for
	// retransmission rather than a failure of the transmission.
	//
	// Both shard counts are required, and neither is inferred from what arrived. A
	// code is defined by the geometry it was encoded at: infer a smaller parity count
	// because the last parity shard went missing, and the decoder reconstructs a
	// different code than the encoder used — which does not fail, it silently returns
	// the wrong bytes. The receiver has the real figures in the manifest, so there is
	// no reason to guess at them.
	Decode(received []Shard, dataShards, parityShards int) ([][]byte, error)

	// ShardsNeeded is how many shards of a dataShards-shard block a decoder needs
	// before it is worth attempting recovery.
	//
	// For the optimal codes this is exactly dataShards. The sparse-graph and
	// fountain codes need a small number more, and the receiver uses this figure to
	// decide when a block is worth decoding — attempting it earlier wastes work on a
	// system that cannot yet have full rank.
	ShardsNeeded(dataShards int) int
}

var codecs = registry.New[Codec]("fec codec", ErrUnknownCodec)

// Register adds a codec to the registry. It panics on a duplicate id or name,
// because a collision would mean blocks decoded by the wrong code.
func Register(c Codec) { codecs.Register(c) }

// ByID returns the codec with that wire identifier.
func ByID(id uint8) (Codec, error) { return codecs.ByID(id) }

// ByName returns the codec with that configuration name.
func ByName(name string) (Codec, error) { return codecs.ByName(name) }

// All returns every registered codec, ordered by id.
func All() []Codec { return codecs.All() }

// Names returns every registered codec name, ordered by id.
func Names() []string { return codecs.Names() }

// Params describes a block geometry as it travels in the manifest.
func Params(c Codec, dataShards, parityShards, shardSize int) (protocol.FECParams, error) {
	if err := c.Validate(dataShards, parityShards); err != nil {
		return protocol.FECParams{}, err
	}
	if shardSize <= 0 {
		return protocol.FECParams{}, fmt.Errorf("%w: shard size %d", ErrShardSize, shardSize)
	}
	return protocol.FECParams{
		ID:           c.ID(),
		DataShards:   uint16(dataShards),
		ParityShards: uint16(parityShards),
		ShardSize:    uint32(shardSize),
	}, nil
}

// checkSource validates a block of source shards and returns the shard length.
func checkSource(source [][]byte) (int, error) {
	if len(source) == 0 {
		return 0, fmt.Errorf("%w: no source shards", ErrShardGeometry)
	}
	size := len(source[0])
	if size == 0 {
		return 0, ErrShardSize
	}
	for _, s := range source {
		if len(s) != size {
			return 0, fmt.Errorf("%w: %d and %d", ErrShardSize, size, len(s))
		}
	}
	return size, nil
}

// collect sorts received shards into a slice indexed by ESI, and returns the shard
// length.
//
// It rejects two copies of one shard that disagree. A shard only reaches this layer
// after its frame passed CRC32 and SHA-256, so two different payloads under one
// identifier means something upstream is broken in a way those checksums did not
// catch — and quietly picking one would let the block reconstruct into plausible
// nonsense.
func collect(received []Shard, span int) (map[uint32][]byte, int, error) {
	if len(received) == 0 {
		return nil, 0, ErrTooFewShards
	}
	out := make(map[uint32][]byte, len(received))
	size := 0
	for _, s := range received {
		if len(s.Data) == 0 {
			return nil, 0, ErrShardSize
		}
		if size == 0 {
			size = len(s.Data)
		} else if len(s.Data) != size {
			return nil, 0, fmt.Errorf("%w: %d and %d", ErrShardSize, size, len(s.Data))
		}
		if span > 0 && int(s.ESI) >= span {
			return nil, 0, fmt.Errorf("%w: esi %d in a block of %d", ErrBadESI, s.ESI, span)
		}
		if prev, dup := out[s.ESI]; dup {
			if !equalBytes(prev, s.Data) {
				return nil, 0, fmt.Errorf("%w: esi %d", ErrDuplicateShard, s.ESI)
			}
			continue
		}
		out[s.ESI] = s.Data
	}
	return out, size, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
