package protocol_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

// Golden vectors for the wire records.
//
// These are the protocol's compatibility contract, and they are pinned as literal
// bytes rather than round-tripped because a round-trip test cannot fail the way
// this one can. Marshal and Unmarshal will always agree with each other, even if
// both drift — swap two adjacent fields, change an endianness, resize a reserved
// area, and every round-trip test in the package still passes while every already
// deployed receiver stops being able to read the frames.
//
// A change that breaks one of these vectors is a wire-format change. That is
// sometimes correct, but it is never incidental: it means the protocol version has
// to be raised and both applications redeployed together, and this test failing is
// how that decision gets made deliberately.
//
// Regenerate with care, never with -update.
const (
	goldenHeaderHex = "" +
		"4f545031" + // magic OTP1
		"0001" + // version 1
		"005c" + // header length 92
		"0009" + // flags: manifest|last-chunk
		"03" + // encoder id: color8
		"03" + // bit depth 3
		"04" + // compression id: brotli
		"02" + // fec id: raptorq
		"0008" + // cell pixels 8
		"0060" + // grid width 96
		"0060" + // grid height 96
		"6f9619ff8b86d011b42d00cf4fc964ff" + // transmission id
		"9c1e5f420d3a4b218f772b1d5a6c9e01" + // session id
		"00000007" + // frame number 7
		"00000040" + // total frames 64
		"00000006" + // chunk number 6
		"0000003f" + // total chunks 63
		"00000200" + // payload length 512
		"00000198746da700" + // timestamp 1754300000000 ms
		"0000000000000000" + // reserved
		"92e3c0ba" // header crc32

	goldenFooterHex = "" +
		"d3bfbc35" + // payload crc32
		"c0d58d4473e23043996ddcdbe9b175816b96acd7dd9486df41b9de3b7097bd84" + // payload sha256
		"0000000000000000" + // reserved
		"4f545046" + // magic OTPF
		"d60ce009" // footer crc32

	// The descriptor is bit-packed — 12 bits of width, 12 of height, 8 of cell
	// size, 4 of flags, then a 16-bit checksum — so its fields deliberately do not
	// land on byte boundaries. Reading left to right: 0x060, 0x060, 0x08, 0x3,
	// checksum 0xf818, then four bits of padding.
	goldenDescriptorHex = "060060083f8180"

	// The manifest gained a callback URL after this vector was first written. That was a
	// deliberate wire-format change made before anything was deployed — the golden test is what
	// forced it to be a decision rather than an accident — so the vector was regenerated rather
	// than the protocol version raised. Once a build is in the field, the same failure means the
	// version has to move and both applications have to be redeployed together.
	goldenManifestHex = "" +
		"4f5450 4d" + // magic OTPM
		"0001" + // version 1
		"18" + // filename length 24
		"0000" + // callback URL length 0
		"0000000006400000" + // original size 104857600
		"8b2c4f0a4e26f5f0d3a2f7bd2a5e2f6a7c0e91b4e9c5f61b39a9e2d1c47a3f8e" + // original sha256
		"0000000002800000" + // compressed size 41943040
		"00005000" + // chunk count 20480
		"00000800" + // chunk size 2048
		"04" + // compression id: brotli
		"02" + // fec id: raptorq
		"0010" + // 16 data shards
		"0004" + // 4 parity shards
		"00000800" + // shard size 2048
		"0000000000000000" + // reserved
		"7175617274 65726c792d7265706f72742e7461722e7a7374" + // "quarterly-report.tar.zst"
		"aa829f91" // manifest crc32
)

// goldenHeader is the header the vector above encodes.
func goldenHeader() protocol.Header {
	return protocol.Header{
		Version:        protocol.Current,
		Flags:          protocol.FlagManifest | protocol.FlagLastChunk,
		EncoderID:      3,
		BitDepth:       3,
		CompressionID:  4,
		FECID:          2,
		CellPixels:     8,
		GridWidth:      96,
		GridHeight:     96,
		TransmissionID: uuid.MustParse("6f9619ff-8b86-d011-b42d-00cf4fc964ff"),
		SessionID:      uuid.MustParse("9c1e5f42-0d3a-4b21-8f77-2b1d5a6c9e01"),
		FrameNumber:    7,
		TotalFrames:    64,
		ChunkNumber:    6,
		TotalChunks:    63,
		PayloadLength:  512,
		TimestampMS:    1754300000000,
	}
}

func TestGoldenHeader(t *testing.T) {
	want := unhex(t, goldenHeaderHex)
	require.Len(t, want, protocol.HeaderSize)

	got, err := goldenHeader().MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(want), hex.EncodeToString(got),
		"the header wire format changed; see the note on the golden vectors")

	var parsed protocol.Header
	require.NoError(t, parsed.UnmarshalBinary(want))
	require.Equal(t, goldenHeader(), parsed)
}

func TestGoldenFooter(t *testing.T) {
	// The vector is the footer of a frame whose payload is the 512 bytes the golden
	// header describes, so the two vectors together describe one real frame rather
	// than two unrelated records.
	payload := goldenPayload()
	want := unhex(t, goldenFooterHex)
	require.Len(t, want, protocol.FooterSize)

	f := protocol.NewFrame(goldenHeader(), payload)
	got, err := f.Footer.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(want), hex.EncodeToString(got),
		"the footer wire format or the integrity functions changed")

	var parsed protocol.Footer
	require.NoError(t, parsed.UnmarshalBinary(want))
	require.Equal(t, f.Footer, parsed)
}

func TestGoldenDescriptor(t *testing.T) {
	want := unhex(t, goldenDescriptorHex)

	got, err := protocol.Descriptor{GridWidth: 96, GridHeight: 96, CellPixels: 8, Flags: 3}.MarshalBits()
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(want), hex.EncodeToString(got),
		"the grid descriptor's bit layout changed, which breaks geometry recovery")

	var parsed protocol.Descriptor
	require.NoError(t, parsed.UnmarshalBits(want))
	require.Equal(t, uint16(96), parsed.GridWidth)
	require.Equal(t, uint16(96), parsed.GridHeight)
	require.Equal(t, uint8(8), parsed.CellPixels)
	require.Equal(t, uint8(3), parsed.Flags)
}

func TestGoldenManifest(t *testing.T) {
	want := unhex(t, goldenManifestHex)

	m := protocol.Manifest{
		Filename:       "quarterly-report.tar.zst",
		OriginalSize:   104857600,
		OriginalSHA256: goldenSHA(t, "8b2c4f0a4e26f5f0d3a2f7bd2a5e2f6a7c0e91b4e9c5f61b39a9e2d1c47a3f8e"),
		CompressedSize: 41943040,
		ChunkCount:     20480,
		ChunkSize:      2048,
		CompressionID:  4,
		FEC:            protocol.FECParams{ID: 2, DataShards: 16, ParityShards: 4, ShardSize: 2048},
	}

	got, err := m.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(want), hex.EncodeToString(got),
		"the manifest wire format changed; a receiver on the old format cannot join a transmission")

	var parsed protocol.Manifest
	require.NoError(t, parsed.UnmarshalBinary(want))
	require.Equal(t, m, parsed)
}

// TestGoldenPayloadIsStable pins the test payload itself. Two of the vectors above
// are checksums over it, so if it were generated differently they would both fail
// for a reason that has nothing to do with the protocol.
func TestGoldenPayloadIsStable(t *testing.T) {
	sum := sha256.Sum256(goldenPayload())
	require.Equal(t,
		"c0d58d4473e23043996ddcdbe9b175816b96acd7dd9486df41b9de3b7097bd84",
		hex.EncodeToString(sum[:]))
}

// goldenPayload is 512 bytes of a fixed, non-repeating pattern.
func goldenPayload() []byte {
	b := make([]byte, 512)
	for i := range b {
		// A counter alone would be a poor test payload: every byte of it is
		// predictable from its position, so a modulation that dropped the low bits
		// of an index could still appear to work. Mixing in a second, coprime stride
		// makes each byte depend on more than one thing.
		b[i] = byte(i*31 + i/7 + 11)
	}
	return b
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	// The vectors are written with spaces where a field boundary falls awkwardly,
	// so they can be read against the marshalling code.
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			clean = append(clean, s[i])
		}
	}
	b, err := hex.DecodeString(string(clean))
	require.NoError(t, err)
	return b
}

func goldenSHA(t *testing.T, s string) [32]byte {
	t.Helper()
	b := unhex(t, s)
	require.Len(t, b, 32)
	return [32]byte(b)
}
