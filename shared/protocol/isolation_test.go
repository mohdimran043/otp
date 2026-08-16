package protocol_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

// One file's chunk cannot become another file's chunk.
//
// With several transfers displaying at once the question is not rhetorical: every frame on the screen
// carries the transmission it belongs to, and the receiver files chunks under whatever that field
// says. If a corrupted read could turn transfer A's identifier into transfer B's, a chunk of one file
// would be written into the middle of another and the damage would survive every later check — the
// payload is intact, its checksums pass, and it is simply in the wrong file. It would surface as a
// corrupt download long after the transfer reported success.
//
// It cannot happen, and this is the layer that stops it: the transmission identifier lives inside the
// header, the header carries its own CRC32 over every byte of itself, and a header that fails is a
// frame the decoder refuses outright. The tests below hold that property still, because it is the
// kind of guarantee that is quietly lost when a field is added or the CRC's coverage is narrowed.

func chunkHeader(transmission uuid.UUID, chunk uint32) protocol.Header {
	return protocol.Header{
		Version:        protocol.Current,
		EncoderID:      1,
		BitDepth:       1,
		CompressionID:  2,
		FECID:          1,
		CellPixels:     protocol.DefaultCellPixels,
		GridWidth:      protocol.DefaultGridWidth,
		GridHeight:     protocol.DefaultGridHeight,
		TransmissionID: transmission,
		SessionID:      uuid.New(),
		FrameNumber:    7,
		TotalFrames:    64,
		ChunkNumber:    chunk,
		TotalChunks:    64,
		PayloadLength:  16,
		TimestampMS:    1754300000000,
	}
}

// Corrupting the transmission identifier fails the header rather than misfiling the chunk.
//
// Every bit of the identifier is tried, not a sampled few. The property has to hold for all of them:
// one unprotected bit is one way for a chunk to land in the wrong file, and a spot check would very
// likely miss it.
func TestACorruptedTransmissionIDFailsTheHeader(t *testing.T) {
	fileA := uuid.MustParse("aaaaaaaa-1111-2222-3333-444444444444")
	h := chunkHeader(fileA, 12)

	encoded, err := h.MarshalBinary()
	require.NoError(t, err)

	// Where the identifier sits, found by encoding a header that differs only in that field.
	other := chunkHeader(uuid.MustParse("bbbbbbbb-1111-2222-3333-444444444444"), 12)
	otherEncoded, err := other.MarshalBinary()
	require.NoError(t, err)

	var idBytes []int
	for i := range encoded {
		if encoded[i] != otherEncoded[i] {
			idBytes = append(idBytes, i)
		}
	}
	require.NotEmpty(t, idBytes, "the identifier must appear in the serialised header")

	for _, at := range idBytes {
		for bit := range 8 {
			corrupted := append([]byte(nil), encoded...)
			corrupted[at] ^= 1 << bit

			var got protocol.Header
			err := got.UnmarshalBinary(corrupted)
			require.Error(t, err,
				"a header with byte %d bit %d flipped was accepted; that chunk would be filed under transmission %s",
				at, bit, got.TransmissionID)
			assert.ErrorIs(t, err, protocol.ErrHeaderCRC)
		}
	}
}

// An intact header round-trips its identifier exactly, which is the other half of the guarantee:
// refusing everything would also pass the test above.
func TestAnIntactHeaderKeepsItsTransmissionAndChunk(t *testing.T) {
	fileA := uuid.MustParse("aaaaaaaa-1111-2222-3333-444444444444")
	h := chunkHeader(fileA, 12)

	encoded, err := h.MarshalBinary()
	require.NoError(t, err)

	var got protocol.Header
	require.NoError(t, got.UnmarshalBinary(encoded))
	assert.Equal(t, fileA, got.TransmissionID)
	assert.Equal(t, uint32(12), got.ChunkNumber)
}

// Two files' chunk 12 are different frames, and stay distinguishable through a round trip.
//
// The receiver keys chunks on (transmission, chunk number), so the number alone is deliberately not
// unique — chunk 12 exists in every transfer. This is what makes the identifier load-bearing rather
// than merely informative.
func TestTheSameChunkNumberInTwoFilesStaysApart(t *testing.T) {
	fileA := uuid.MustParse("aaaaaaaa-1111-2222-3333-444444444444")
	fileB := uuid.MustParse("bbbbbbbb-5555-6666-7777-888888888888")

	a, err := chunkHeader(fileA, 12).MarshalBinary()
	require.NoError(t, err)
	b, err := chunkHeader(fileB, 12).MarshalBinary()
	require.NoError(t, err)

	var gotA, gotB protocol.Header
	require.NoError(t, gotA.UnmarshalBinary(a))
	require.NoError(t, gotB.UnmarshalBinary(b))

	assert.Equal(t, gotA.ChunkNumber, gotB.ChunkNumber, "the same chunk number in both files")
	assert.NotEqual(t, gotA.TransmissionID, gotB.TransmissionID,
		"and the identifier is the only thing keeping them apart")
}
