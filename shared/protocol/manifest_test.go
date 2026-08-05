package protocol_test

import (
	"crypto/sha256"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

func sampleManifest() protocol.Manifest {
	return protocol.Manifest{
		Filename:       "quarterly-report.tar.zst",
		OriginalSize:   104857600,
		OriginalSHA256: sha256.Sum256([]byte("quarterly-report")),
		CompressedSize: 41943040,
		ChunkCount:     20480,
		ChunkSize:      2048,
		CompressionID:  4,
		FEC: protocol.FECParams{
			ID:           2,
			DataShards:   16,
			ParityShards: 4,
			ShardSize:    2048,
		},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	want := sampleManifest()

	b, err := want.MarshalBinary()
	require.NoError(t, err)
	require.Len(t, b, want.Size())

	var got protocol.Manifest
	require.NoError(t, got.UnmarshalBinary(b))
	require.Equal(t, want, got)
}

// TestManifestSurvivesFramePadding covers the case that actually occurs: a
// manifest is carried as a frame payload, and a payload is whatever length the
// grid demanded, so the parser has to work from the record's declared length
// rather than the buffer it was handed.
func TestManifestSurvivesFramePadding(t *testing.T) {
	want := sampleManifest()
	b, err := want.MarshalBinary()
	require.NoError(t, err)

	padded := append(b, make([]byte, 512)...)
	var got protocol.Manifest
	require.NoError(t, got.UnmarshalBinary(padded))
	require.Equal(t, want, got)
}

func TestManifestRejectsCorruption(t *testing.T) {
	b, err := sampleManifest().MarshalBinary()
	require.NoError(t, err)

	t.Run("magic", func(t *testing.T) {
		bad := append([]byte(nil), b...)
		bad[0] = 'X'
		var m protocol.Manifest
		require.ErrorIs(t, m.UnmarshalBinary(bad), protocol.ErrBadMagic)
	})

	t.Run("crc", func(t *testing.T) {
		bad := append([]byte(nil), b...)
		bad[20] ^= 0x01
		var m protocol.Manifest
		require.ErrorIs(t, m.UnmarshalBinary(bad), protocol.ErrManifestCRC)
	})

	t.Run("truncated", func(t *testing.T) {
		var m protocol.Manifest
		require.ErrorIs(t, m.UnmarshalBinary(b[:40]), protocol.ErrShortBuffer)
	})

	t.Run("name longer than the record", func(t *testing.T) {
		bad := append([]byte(nil), b...)
		bad[6] = 255
		var m protocol.Manifest
		require.ErrorIs(t, m.UnmarshalBinary(bad), protocol.ErrShortBuffer)
	})
}

// TestManifestRejectsHostileFilenames is a security test, not a validation one.
// The name in a manifest arrives from outside the receiver's trust boundary and is
// then used to write a file, so anything that could redirect that write has to be
// refused at the protocol boundary rather than by every caller remembering to.
func TestManifestRejectsHostileFilenames(t *testing.T) {
	for _, name := range []string{
		"",
		"..",
		".",
		"../../etc/passwd",
		"/etc/passwd",
		"subdir/file.bin",
		`windows\path.bin`,
		"trailing\x00nul.bin",
		"bell\x07.bin",
		"newline\n.bin",
		strings.Repeat("a", protocol.MaxFilenameBytes+1),
	} {
		m := sampleManifest()
		m.Filename = name
		_, err := m.MarshalBinary()
		require.ErrorIs(t, err, protocol.ErrBadFilename, "filename %q must be refused", name)
	}

	// A hostile name must also be refused on the way in, since a manifest is
	// authored by whatever was on the other end of the optical channel.
	m := sampleManifest()
	m.Filename = "harmless.bin"
	b, err := m.MarshalBinary()
	require.NoError(t, err)

	// Rewrite the name in place to one that would escape the output directory, and
	// repair the CRC so only the semantic check can catch it.
	require.Equal(t, len("harmless.bin"), int(b[6]))
	copy(b[81:81+12], "../../ha.bin")
	fixCRC(b, m.Size())

	var parsed protocol.Manifest
	require.ErrorIs(t, parsed.UnmarshalBinary(b), protocol.ErrBadFilename,
		"a valid checksum makes a record intact, not trustworthy")
}

// fixCRC repairs a manifest's trailing checksum after the record has been edited,
// so a test can prove the semantic checks stand on their own rather than being
// carried by the CRC.
func fixCRC(b []byte, size int) {
	binary.BigEndian.PutUint32(b[size-4:size], crc32.ChecksumIEEE(b[:size-4]))
}

// TestManifestRejectsInconsistentChunking covers manifests that are internally
// impossible. A receiver acting on one of these would either truncate the file or
// wait forever for a chunk that was never sent.
func TestManifestRejectsInconsistentChunking(t *testing.T) {
	cases := map[string]func(m *protocol.Manifest){
		"no chunks":           func(m *protocol.Manifest) { m.ChunkCount = 0 },
		"no chunk size":       func(m *protocol.Manifest) { m.ChunkSize = 0 },
		"too few chunks":      func(m *protocol.Manifest) { m.ChunkCount = 10 },
		"one chunk too many":  func(m *protocol.Manifest) { m.ChunkCount = 20482 },
		"fec without shards":  func(m *protocol.Manifest) { m.FEC.DataShards = 0 },
		"compressed to zero":  func(m *protocol.Manifest) { m.CompressedSize = 0 },
		"empty but multipart": func(m *protocol.Manifest) { *m = emptyManifest(); m.ChunkCount = 3 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := sampleManifest()
			mutate(&m)
			require.Error(t, m.Validate())
		})
	}

	// The exact-fit boundaries either side of a valid split must be accepted, since
	// a real transmission always sits on one of them.
	m := sampleManifest()
	m.CompressedSize = uint64(m.ChunkCount) * uint64(m.ChunkSize)
	require.NoError(t, m.Validate(), "a stream that fills its chunks exactly is valid")

	m.CompressedSize -= uint64(m.ChunkSize) - 1
	require.NoError(t, m.Validate(), "a final chunk of one byte is valid")
}

func emptyManifest() protocol.Manifest {
	return protocol.Manifest{
		Filename:       "empty.bin",
		OriginalSHA256: sha256.Sum256(nil),
		ChunkCount:     1,
		ChunkSize:      2048,
	}
}

// TestEmptyFileIsTransmittable checks the degenerate case is representable rather
// than rejected. A zero-byte file is a strange thing to send optically, but a
// platform that could not send one would fail in the field rather than in a test.
func TestEmptyFileIsTransmittable(t *testing.T) {
	want := emptyManifest()
	b, err := want.MarshalBinary()
	require.NoError(t, err)

	var got protocol.Manifest
	require.NoError(t, got.UnmarshalBinary(b))
	require.Equal(t, want, got)
}

// TestManifestFrameCannotDisagreeWithItsManifest checks the frame builder keeps
// the flag and the derived header fields in step with the record. A manifest
// payload in an unflagged frame would be merged into the output file as data, which
// no per-frame checksum catches — the corruption only surfaces at the final hash.
func TestManifestFrameCannotDisagreeWithItsManifest(t *testing.T) {
	m := sampleManifest()

	f, err := protocol.NewManifestFrame(protocol.Header{FrameNumber: 0}, m)
	require.NoError(t, err)
	require.True(t, f.Header.Flags.Has(protocol.FlagManifest))
	require.Equal(t, m.CompressionID, f.Header.CompressionID)
	require.Equal(t, m.FEC.ID, f.Header.FECID)
	require.Equal(t, m.ChunkCount, f.Header.TotalChunks)
	require.NoError(t, f.Verify())

	got, err := protocol.ParseManifest(f)
	require.NoError(t, err)
	require.Equal(t, m, got)

	// A data frame asked for a manifest must say so rather than parse whatever its
	// payload happens to start with.
	data := protocol.NewFrame(protocol.Header{}, []byte("chunk bytes"))
	_, err = protocol.ParseManifest(data)
	require.ErrorIs(t, err, protocol.ErrNotManifest)

	_, err = protocol.ParseManifest(nil)
	require.ErrorIs(t, err, protocol.ErrNotManifest)
}

// TestManifestFitsSmallestGrid checks a manifest is carryable where it has to be.
// It is the first frame of every transmission, so a manifest that did not fit the
// smallest configurable grid would make that grid unusable for anything.
func TestManifestFitsSmallestGrid(t *testing.T) {
	m := sampleManifest()
	m.Filename = strings.Repeat("n", protocol.MaxFilenameBytes)
	require.LessOrEqual(t, m.Size(), 512,
		"the manifest must stay small enough for one frame at the smallest grid")
}
