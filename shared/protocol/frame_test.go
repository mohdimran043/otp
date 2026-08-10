package protocol

import (
	"bytes"
	"hash/crc32"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testHeader() Header {
	return Header{
		Version:        Current,
		Flags:          FlagManifest | FlagLastChunk,
		EncoderID:      3,
		BitDepth:       4,
		CompressionID:  2,
		FECID:          1,
		CellPixels:     DefaultCellPixels,
		GridWidth:      DefaultGridWidth,
		GridHeight:     DefaultGridHeight,
		TransmissionID: uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"),
		SessionID:      uuid.MustParse("6ba7b811-9dad-11d1-80b4-00c04fd430c8"),
		FrameNumber:    17,
		TotalFrames:    4096,
		ChunkNumber:    16,
		TotalChunks:    4095,
		PayloadLength:  1234,
		TimestampMS:    1_754_300_000_000,
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	in := testHeader()
	b, err := in.MarshalBinary()
	require.NoError(t, err)
	require.Len(t, b, HeaderSize, "HeaderSize constant must match the marshalled size")

	var out Header
	require.NoError(t, out.UnmarshalBinary(b))
	assert.Equal(t, in, out)
}

func TestFooterRoundTrip(t *testing.T) {
	in := Footer{PayloadCRC32: 0xDEADBEEF}
	for i := range in.PayloadSHA256 {
		in.PayloadSHA256[i] = byte(i * 7)
	}
	b, err := in.MarshalBinary()
	require.NoError(t, err)
	require.Len(t, b, FooterSize, "FooterSize constant must match the marshalled size")

	var out Footer
	require.NoError(t, out.UnmarshalBinary(b))
	assert.Equal(t, in, out)
}

func TestHeaderRejectsCorruption(t *testing.T) {
	good, err := testHeader().MarshalBinary()
	require.NoError(t, err)

	t.Run("bad magic", func(t *testing.T) {
		b := bytes.Clone(good)
		b[0] = 'X'
		var h Header
		assert.ErrorIs(t, h.UnmarshalBinary(b), ErrBadMagic)
	})

	t.Run("flipped payload bit", func(t *testing.T) {
		b := bytes.Clone(good)
		b[60] ^= 0x01 // inside ChunkNumber
		var h Header
		assert.ErrorIs(t, h.UnmarshalBinary(b), ErrHeaderCRC)
	})

	t.Run("truncated", func(t *testing.T) {
		var h Header
		assert.ErrorIs(t, h.UnmarshalBinary(good[:HeaderSize-1]), ErrShortBuffer)
	})

	t.Run("unsupported version", func(t *testing.T) {
		h := testHeader()
		h.Version = Current + 1
		b, err := h.MarshalBinary()
		require.NoError(t, err)
		var out Header
		assert.ErrorIs(t, out.UnmarshalBinary(b), ErrUnsupportedVersion)
	})
}

func TestFooterRejectsCorruption(t *testing.T) {
	good, err := Footer{PayloadCRC32: 1}.MarshalBinary()
	require.NoError(t, err)

	t.Run("bad magic", func(t *testing.T) {
		b := bytes.Clone(good)
		b[44] = 'Z'
		var f Footer
		assert.ErrorIs(t, f.UnmarshalBinary(b), ErrBadMagic)
	})

	t.Run("flipped hash bit", func(t *testing.T) {
		b := bytes.Clone(good)
		b[10] ^= 0x80
		var f Footer
		assert.ErrorIs(t, f.UnmarshalBinary(b), ErrFooterCRC)
	})
}

func TestNewFrameDerivesIntegrityFields(t *testing.T) {
	payload := []byte("the quick brown fox jumps over the lazy dog")
	f := NewFrame(Header{FrameNumber: 9}, payload)

	assert.Equal(t, Current, f.Header.Version)
	assert.Equal(t, uint32(len(payload)), f.Header.PayloadLength)
	assert.NotZero(t, f.Header.TimestampMS, "timestamp should be stamped when unset")
	assert.Equal(t, crc32.ChecksumIEEE(payload), f.Footer.PayloadCRC32)
	require.NoError(t, f.Verify())
}

func TestFrameVerifyDetectsTamper(t *testing.T) {
	f := NewFrame(Header{}, []byte("original payload contents"))
	require.NoError(t, f.Verify())

	t.Run("payload byte changed", func(t *testing.T) {
		g := NewFrame(Header{}, []byte("original payload contents"))
		g.Payload[3] ^= 0xFF
		assert.ErrorIs(t, g.Verify(), ErrPayloadCRC)
	})

	t.Run("length disagrees with header", func(t *testing.T) {
		g := NewFrame(Header{}, []byte("original payload contents"))
		g.Payload = g.Payload[:5]
		assert.ErrorIs(t, g.Verify(), ErrPayloadCRC)
	})

	t.Run("crc collides but hash does not", func(t *testing.T) {
		g := NewFrame(Header{}, []byte("original payload contents"))
		// Force the CRC to agree while the bytes differ, isolating the SHA-256
		// check. This is the case CRC32 alone cannot catch.
		g.Payload = bytes.Repeat([]byte{0x41}, len(g.Payload))
		g.Footer.PayloadCRC32 = crc32.ChecksumIEEE(g.Payload)
		assert.ErrorIs(t, g.Verify(), ErrPayloadHash)
	})
}

func TestFlagsString(t *testing.T) {
	assert.Equal(t, "none", Flags(0).String())
	assert.Equal(t, "manifest", FlagManifest.String())
	assert.Equal(t, "manifest|retransmit", (FlagManifest | FlagRetransmit).String())
}

func TestVersionSupport(t *testing.T) {
	assert.True(t, SupportsVersion(Version1))
	assert.False(t, SupportsVersion(0))
	assert.False(t, SupportsVersion(Current+1))
	assert.NoError(t, CheckVersion(Current))
	assert.ErrorIs(t, CheckVersion(Current+1), ErrUnsupportedVersion)
	require.NotEmpty(t, Versions())
}

func TestHeaderEncryptionIDRoundTrip(t *testing.T) {
	h := Header{
		Version:        Current,
		Flags:          FlagEncrypted,
		EncryptionID:   2,
		TransmissionID: uuid.New(),
	}
	b, err := h.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, byte(2), b[80], "the encryption id is wire byte 80, the first former reserved byte")

	var got Header
	require.NoError(t, got.UnmarshalBinary(b))
	require.Equal(t, uint8(2), got.EncryptionID)
}

func TestHeaderEncryptionIDZeroIsLegacyLayout(t *testing.T) {
	// A header written before the field existed carries zeroes at byte 80; it must
	// unmarshal to EncryptionID 0 with nothing else disturbed.
	var h Header
	h.Version = Current
	b, err := h.MarshalBinary()
	require.NoError(t, err)
	var got Header
	require.NoError(t, got.UnmarshalBinary(b))
	require.Equal(t, uint8(0), got.EncryptionID)
	require.Equal(t, [7]byte{}, got.Reserved)
}
