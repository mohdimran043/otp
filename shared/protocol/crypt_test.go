package protocol_test

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

func testKey() []byte {
	k := make([]byte, protocol.KeySize)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

func encryptHeader() protocol.Header {
	return protocol.Header{
		TransmissionID: uuid.MustParse("6f9619ff-8b86-d011-b42d-00cf4fc964ff"),
		SessionID:      uuid.MustParse("9c1e5f42-0d3a-4b21-8f77-2b1d5a6c9e01"),
		FrameNumber:    12,
		ChunkNumber:    9,
		TotalChunks:    40,
	}
}

func TestEncryptedPayloadRoundTrips(t *testing.T) {
	key, h := testKey(), encryptHeader()
	plaintext := []byte("the quiet zone is outside the grid coordinate space")

	ciphertext, err := protocol.EncryptPayload(key, protocol.EncryptionAES256GCM, plaintext, h)
	require.NoError(t, err)
	require.Len(t, ciphertext, len(plaintext)+protocol.EncryptionOverhead)
	require.NotContains(t, string(ciphertext), "quiet zone")

	got, err := protocol.DecryptPayload(key, ciphertext, h)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

// TestEncryptionIsRandomised checks two encryptions of the same chunk differ. Equal
// ciphertexts would tell an observer that two frames carry the same chunk, which for a
// file with repeated blocks leaks its structure.
func TestEncryptionIsRandomised(t *testing.T) {
	key, h := testKey(), encryptHeader()
	plaintext := []byte("same chunk, twice")

	first, err := protocol.EncryptPayload(key, protocol.EncryptionAES256GCM, plaintext, h)
	require.NoError(t, err)
	second, err := protocol.EncryptPayload(key, protocol.EncryptionAES256GCM, plaintext, h)
	require.NoError(t, err)
	require.False(t, bytes.Equal(first, second), "each encryption must use a fresh nonce")

	// Both must still decrypt, since a retransmission encrypts afresh.
	for _, c := range [][]byte{first, second} {
		got, err := protocol.DecryptPayload(key, c, h)
		require.NoError(t, err)
		require.Equal(t, plaintext, got)
	}
}

// TestPayloadCannotBeMovedBetweenFrames is the reason the header is authenticated.
// An interceptor who could replay a chunk into a different position would corrupt the
// received file in a way no per-frame checksum detects, since every individual frame
// would be perfectly valid.
func TestPayloadCannotBeMovedBetweenFrames(t *testing.T) {
	key, h := testKey(), encryptHeader()
	ciphertext, err := protocol.EncryptPayload(key, protocol.EncryptionAES256GCM, []byte("chunk nine"), h)
	require.NoError(t, err)

	for name, mutate := range map[string]func(*protocol.Header){
		"a different chunk number": func(h *protocol.Header) { h.ChunkNumber = 10 },
		"a different chunk count":  func(h *protocol.Header) { h.TotalChunks = 41 },
		"a different transmission": func(h *protocol.Header) { h.TransmissionID = uuid.New() },
	} {
		t.Run(name, func(t *testing.T) {
			moved := h
			mutate(&moved)
			_, err := protocol.DecryptPayload(key, ciphertext, moved)
			require.ErrorIs(t, err, protocol.ErrDecrypt)
		})
	}

	// A retransmission legitimately changes the frame number, the flags, and the
	// timestamp, and must still decrypt — otherwise no retransmitted chunk could ever
	// be read.
	for name, mutate := range map[string]func(*protocol.Header){
		"a new frame number":  func(h *protocol.Header) { h.FrameNumber = 900 },
		"the retransmit flag": func(h *protocol.Header) { h.Flags |= protocol.FlagRetransmit },
		"a later timestamp":   func(h *protocol.Header) { h.TimestampMS = 1754300000000 },
		"a new display session": func(h *protocol.Header) {
			h.SessionID = uuid.MustParse("11111111-2222-3333-4444-555555555555")
		},
	} {
		t.Run(name, func(t *testing.T) {
			resent := h
			mutate(&resent)
			got, err := protocol.DecryptPayload(key, ciphertext, resent)
			require.NoError(t, err, "a retransmitted chunk must still decrypt")
			require.Equal(t, []byte("chunk nine"), got)
		})
	}
}

func TestDecryptRejectsTamperingAndBadKeys(t *testing.T) {
	key, h := testKey(), encryptHeader()
	ciphertext, err := protocol.EncryptPayload(key, protocol.EncryptionAES256GCM, []byte("authenticated"), h)
	require.NoError(t, err)

	// Every single-byte change anywhere in the record — nonce, ciphertext, or tag —
	// has to be caught.
	for i := range ciphertext {
		altered := append([]byte(nil), ciphertext...)
		altered[i] ^= 0x01
		_, err := protocol.DecryptPayload(key, altered, h)
		require.ErrorIs(t, err, protocol.ErrDecrypt, "byte %d went undetected", i)
	}

	wrong := testKey()
	wrong[0] ^= 0xFF
	_, err = protocol.DecryptPayload(wrong, ciphertext, h)
	require.ErrorIs(t, err, protocol.ErrDecrypt)

	_, err = protocol.DecryptPayload(key, ciphertext[:protocol.EncryptionOverhead-1], h)
	require.ErrorIs(t, err, protocol.ErrDecrypt)

	for _, size := range []int{0, 16, 31, 33, 64} {
		_, err := protocol.EncryptPayload(make([]byte, size), protocol.EncryptionAES256GCM, []byte("x"), h)
		require.ErrorIs(t, err, protocol.ErrKeySize, "a %d-byte key must be refused", size)
	}
}

// TestEncryptedFrameChecksumsTheCiphertext pins which bytes the footer covers. The
// receiver has to be able to tell a badly captured frame from a wrongly decrypted one,
// and it can only checksum what the camera actually saw.
func TestEncryptedFrameChecksumsTheCiphertext(t *testing.T) {
	key := testKey()
	plaintext := []byte("integrity covers what the camera saw")

	f, err := protocol.NewEncryptedFrame(key, protocol.EncryptionNone, encryptHeader(), plaintext)
	require.NoError(t, err)
	require.True(t, f.Header.Flags.Has(protocol.FlagEncrypted))
	require.NoError(t, f.Verify(), "the footer must cover the ciphertext as transmitted")
	require.Equal(t, uint32(len(plaintext)+protocol.EncryptionOverhead), f.Header.PayloadLength)

	got, err := protocol.OpenFrame([][]byte{key}, f)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

// TestOpenFramePassesPlaintextThrough covers the mixed case an operator creates by
// turning encryption on or off while a receiver is running: a receiver holding a key
// must still accept unencrypted frames, and one holding no key must refuse encrypted
// ones rather than pass ciphertext on as data.
func TestOpenFramePassesPlaintextThrough(t *testing.T) {
	plain := protocol.NewFrame(encryptHeader(), []byte("not encrypted"))

	got, err := protocol.OpenFrame([][]byte{testKey()}, plain)
	require.NoError(t, err)
	require.Equal(t, []byte("not encrypted"), got)

	got, err = protocol.OpenFrame(nil, plain)
	require.NoError(t, err)
	require.Equal(t, []byte("not encrypted"), got)

	encrypted, err := protocol.NewEncryptedFrame(testKey(), protocol.EncryptionNone, encryptHeader(), []byte("secret"))
	require.NoError(t, err)
	_, err = protocol.OpenFrame(nil, encrypted)
	require.ErrorIs(t, err, protocol.ErrDecrypt)

	_, err = protocol.OpenFrame([][]byte{testKey()}, nil)
	require.Error(t, err)
}

func TestEncryptionIDsRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, protocol.KeySize)
	h := protocol.Header{TransmissionID: uuid.New(), ChunkNumber: 3, TotalChunks: 9}
	for _, id := range []uint8{protocol.EncryptionAES256GCM, protocol.EncryptionChaCha20Poly1305} {
		frame, err := protocol.NewEncryptedFrame(key, id, h, []byte("payload"))
		require.NoError(t, err)
		require.Equal(t, id, frame.Header.EncryptionID)
		require.True(t, frame.Header.Flags.Has(protocol.FlagEncrypted))

		got, err := protocol.OpenFrame([][]byte{key}, frame)
		require.NoError(t, err)
		require.Equal(t, []byte("payload"), got)
	}
}

func TestOpenFrameWrongCipherFails(t *testing.T) {
	// The same key with the wrong cipher must fail authentication, not mis-decrypt:
	// the id is read from the header, so this simulates a tampered id byte.
	key := bytes.Repeat([]byte{7}, protocol.KeySize)
	h := protocol.Header{TransmissionID: uuid.New(), ChunkNumber: 1, TotalChunks: 2}
	frame, err := protocol.NewEncryptedFrame(key, protocol.EncryptionAES256GCM, h, []byte("payload"))
	require.NoError(t, err)
	frame.Header.EncryptionID = protocol.EncryptionChaCha20Poly1305
	_, err = protocol.OpenFrame([][]byte{key}, frame)
	require.ErrorIs(t, err, protocol.ErrDecrypt)
}

func TestOpenFrameKeyring(t *testing.T) {
	right := bytes.Repeat([]byte{1}, protocol.KeySize)
	wrong := bytes.Repeat([]byte{2}, protocol.KeySize)
	h := protocol.Header{TransmissionID: uuid.New(), TotalChunks: 1}
	frame, err := protocol.NewEncryptedFrame(right, protocol.EncryptionChaCha20Poly1305, h, []byte("x"))
	require.NoError(t, err)

	got, err := protocol.OpenFrame([][]byte{wrong, right}, frame)
	require.NoError(t, err, "the second key in the ring must be tried")
	require.Equal(t, []byte("x"), got)

	_, err = protocol.OpenFrame([][]byte{wrong}, frame)
	require.ErrorIs(t, err, protocol.ErrDecrypt)

	_, err = protocol.OpenFrame(nil, frame)
	require.ErrorIs(t, err, protocol.ErrDecrypt, "an encrypted frame with no keys configured must fail closed")
}

// TestOpenFrameSkipsMalformedKeys covers a keyring holding a wrong-length key: that
// entry must not veto the keys after it, since a malformed key is a property of that
// ring entry, not of the frame.
func TestOpenFrameSkipsMalformedKeys(t *testing.T) {
	right := bytes.Repeat([]byte{3}, protocol.KeySize)
	h := protocol.Header{TransmissionID: uuid.New(), TotalChunks: 1}
	frame, err := protocol.NewEncryptedFrame(right, protocol.EncryptionAES256GCM, h, []byte("y"))
	require.NoError(t, err)

	got, err := protocol.OpenFrame([][]byte{[]byte("short"), right}, frame)
	require.NoError(t, err, "a malformed key must be skipped, not abort the ring")
	require.Equal(t, []byte("y"), got)

	_, err = protocol.OpenFrame([][]byte{[]byte("short"), []byte("also-wrong-length")}, frame)
	require.ErrorIs(t, err, protocol.ErrDecrypt, "a ring of only malformed keys must still fail closed")
}

func TestOpenFrameLegacyIDZeroIsAES(t *testing.T) {
	// Frames from builds that predate EncryptionID set only the flag. They decrypt as
	// AES-256-GCM, which is what those builds sealed with.
	key := bytes.Repeat([]byte{9}, protocol.KeySize)
	h := protocol.Header{TransmissionID: uuid.New(), TotalChunks: 1}
	frame, err := protocol.NewEncryptedFrame(key, protocol.EncryptionNone, h, []byte("legacy"))
	require.NoError(t, err)
	require.Equal(t, uint8(0), frame.Header.EncryptionID)
	require.True(t, frame.Header.Flags.Has(protocol.FlagEncrypted))
	got, err := protocol.OpenFrame([][]byte{key}, frame)
	require.NoError(t, err)
	require.Equal(t, []byte("legacy"), got)
}

func TestEncryptionNames(t *testing.T) {
	id, err := protocol.EncryptionByName("chacha20poly1305")
	require.NoError(t, err)
	require.Equal(t, protocol.EncryptionChaCha20Poly1305, id)
	id, err = protocol.EncryptionByName("")
	require.NoError(t, err)
	require.Equal(t, protocol.EncryptionNone, id)
	_, err = protocol.EncryptionByName("rot13")
	require.Error(t, err)
	require.Equal(t, "aes256gcm", protocol.EncryptionName(protocol.EncryptionAES256GCM))
}
