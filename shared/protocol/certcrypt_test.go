package protocol_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

// Certificate encryption, and the four properties it exists for.
//
// Confidentiality: only the intended receiver can open a frame. Authenticity: only the intended sender
// could have produced one. Independence: any frame opens on its own, in any order. And containment: none of
// it changes what the other encryption modes do.
//
// Each is tested by attacking it rather than by exercising the happy path, because the happy path passing
// is what a broken scheme looks like too.

// pair is a sender and a receiver, each holding its own private key and the other's public.
func pair(t *testing.T) (sender, receiver protocol.CertificateKeys) {
	t.Helper()

	s, err := protocol.NewIdentity("otp-sender")
	require.NoError(t, err)
	r, err := protocol.NewIdentity("otp-receiver")
	require.NoError(t, err)

	sPriv, err := protocol.PrivateKeyFrom(s.PrivateKeyPEM)
	require.NoError(t, err)
	rPriv, err := protocol.PrivateKeyFrom(r.PrivateKeyPEM)
	require.NoError(t, err)
	sPub, err := protocol.PublicKeyFrom(s.CertificatePEM)
	require.NoError(t, err)
	rPub, err := protocol.PublicKeyFrom(r.CertificatePEM)
	require.NoError(t, err)

	return protocol.CertificateKeys{Private: sPriv, PeerPublic: rPub},
		protocol.CertificateKeys{Private: rPriv, PeerPublic: sPub}
}

func certHeader(transmission uuid.UUID, chunk uint32) protocol.Header {
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
		FrameNumber:    chunk,
		ChunkNumber:    chunk,
		TotalChunks:    16,
		TimestampMS:    1754300000000,
	}
}

// The receiver reads what the sender sent, without either ever having shared a secret.
func TestTheReceiverOpensWhatTheSenderSealed(t *testing.T) {
	sender, receiver := pair(t)
	payload := []byte("the quick brown fox jumps over the lazy dog, several times over")

	key, err := protocol.NewRandomKey()
	require.NoError(t, err)

	frame, err := protocol.NewCertificateFrame(sender, key, certHeader(uuid.New(), 3), payload)
	require.NoError(t, err)
	require.NoError(t, frame.Verify(), "the frame's own checksums must still hold")

	got, err := protocol.OpenCertificateFrame(receiver, frame)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	// And the ciphertext is not the plaintext, which is the assertion that catches a mode wired to a no-op.
	assert.NotContains(t, string(frame.Payload), "quick brown fox")
}

// A third party holding both public certificates still cannot read anything.
//
// This is the property the whole scheme is for. The certificates are not secret — they are handed around by
// design — so an eavesdropper is assumed to have them, and confidentiality has to survive that.
func TestSomeoneHoldingBothCertificatesCannotRead(t *testing.T) {
	sender, _ := pair(t)

	// An interloper with its own keypair and the sender's public certificate: everything an observer of the
	// display could have.
	interloper, err := protocol.NewIdentity("interloper")
	require.NoError(t, err)
	iPriv, err := protocol.PrivateKeyFrom(interloper.PrivateKeyPEM)
	require.NoError(t, err)

	key, err := protocol.NewRandomKey()
	require.NoError(t, err)
	frame, err := protocol.NewCertificateFrame(sender, key, certHeader(uuid.New(), 1), []byte("secret"))
	require.NoError(t, err)

	_, err = protocol.OpenCertificateFrame(protocol.CertificateKeys{
		Private:    iPriv,
		PeerPublic: sender.PeerPublic, // the receiver's public certificate, which is not secret either
	}, frame)
	require.Error(t, err, "a frame must not open for anyone but the receiver it was sealed to")
	assert.ErrorIs(t, err, protocol.ErrWrappedKey)
}

// A frame from an impostor does not open, even though it is addressed correctly.
//
// The other half of the ECDH: the receiver derives the shared value from the *sender's* public key, so a
// frame sealed with a different private key derives a different value and the sealed key fails. Only the
// holder of the sender's private key can produce a frame this receiver opens.
func TestAFrameFromTheWrongSenderDoesNotOpen(t *testing.T) {
	_, receiver := pair(t)

	impostor, err := protocol.NewIdentity("impostor")
	require.NoError(t, err)
	iPriv, err := protocol.PrivateKeyFrom(impostor.PrivateKeyPEM)
	require.NoError(t, err)

	// The impostor seals correctly to the real receiver — it has the receiver's public certificate.
	key, err := protocol.NewRandomKey()
	require.NoError(t, err)
	frame, err := protocol.NewCertificateFrame(protocol.CertificateKeys{
		Private:    iPriv,
		PeerPublic: receiver.PeerPublic,
	}, key, certHeader(uuid.New(), 1), []byte("forged"))
	require.NoError(t, err)

	_, err = protocol.OpenCertificateFrame(receiver, frame)
	assert.ErrorIs(t, err, protocol.ErrWrappedKey,
		"the receiver expects the sender it was told about, and this is not it")
}

// Every frame opens on its own, in any order.
//
// The property the rest of this system rests on. A receiver joins a transfer part way through and reads what
// it sees; if a frame depended on an earlier one, the manifest being re-emitted for exactly that reason
// would be pointless.
func TestEveryFrameOpensIndependently(t *testing.T) {
	sender, receiver := pair(t)
	transmission := uuid.New()

	key, err := protocol.NewRandomKey()
	require.NoError(t, err)

	var frames []*protocol.Frame
	for chunk := range uint32(5) {
		f, err := protocol.NewCertificateFrame(sender, key, certHeader(transmission, chunk),
			[]byte("chunk "+string(rune('a'+chunk))))
		require.NoError(t, err)
		frames = append(frames, f)
	}

	// Backwards, which is as arbitrary an order as any and is what a camera catching the tail of a display
	// then wrapping around actually produces.
	for i := len(frames) - 1; i >= 0; i-- {
		got, err := protocol.OpenCertificateFrame(receiver, frames[i])
		require.NoError(t, err, "frame %d should open with no reference to any other", i)
		assert.Equal(t, "chunk "+string(rune('a'+i)), string(got))
	}
}

// A sealed key lifted from one frame into another does not open.
//
// The header is the AEAD's additional data for the wrapped key as well as for the payload, so a key and the
// frame it was sealed for cannot be separated. Nothing could be read either way, but shuffling keys between
// frames would turn a clean failure into a puzzling one.
func TestASealedKeyCannotBeMovedBetweenFrames(t *testing.T) {
	sender, receiver := pair(t)
	transmission := uuid.New()

	key, err := protocol.NewRandomKey()
	require.NoError(t, err)

	first, err := protocol.NewCertificateFrame(sender, key, certHeader(transmission, 1), []byte("one"))
	require.NoError(t, err)
	second, err := protocol.NewCertificateFrame(sender, key, certHeader(transmission, 2), []byte("two"))
	require.NoError(t, err)

	// Graft the first frame's sealed key onto the second's body.
	const sealed = 60 // nonce + key + tag; see wrappedKeySize
	spliced := &protocol.Frame{Header: second.Header}
	spliced.Payload = append(append([]byte{}, first.Payload[:sealed]...), second.Payload[sealed:]...)

	_, err = protocol.OpenCertificateFrame(receiver, spliced)
	assert.Error(t, err, "a sealed key belongs to the frame it was sealed for")
}

// Altering a single byte of the payload is caught.
func TestATamperedPayloadIsRefused(t *testing.T) {
	sender, receiver := pair(t)

	key, err := protocol.NewRandomKey()
	require.NoError(t, err)
	frame, err := protocol.NewCertificateFrame(sender, key, certHeader(uuid.New(), 1), []byte("intact"))
	require.NoError(t, err)

	frame.Payload[len(frame.Payload)-1] ^= 0x01
	_, err = protocol.OpenCertificateFrame(receiver, frame)
	assert.Error(t, err)
}

// Two transfers between the same certificates do not share a wrapping key.
//
// The ECDH value is static — these two certificates always agree on it — so without the transmission id as
// a salt, every frame of every transfer this pair ever exchanged would be sealed under one key, leaving
// 96-bit random nonces as the only thing between them and a collision.
func TestTwoTransfersDoNotShareAWrappingKey(t *testing.T) {
	sender, _ := pair(t)
	key, err := protocol.NewRandomKey()
	require.NoError(t, err)

	// The same AES key and the same nonce-free inputs, differing only in the transmission.
	a, err := protocol.SealKey(sender, key, certHeader(uuid.New(), 1))
	require.NoError(t, err)
	b, err := protocol.SealKey(sender, key, certHeader(uuid.New(), 1))
	require.NoError(t, err)

	assert.NotEqual(t, a, b, "sealing is randomised per frame, so these cannot match")
}

// Missing either half is refused, and says which half.
//
// An operator who has generated their own keypair but not yet installed the peer's certificate is in a
// perfectly ordinary state, and "no peer certificate" is what tells them what to do next.
func TestMissingCertificatesAreRefusedClearly(t *testing.T) {
	sender, _ := pair(t)
	key, err := protocol.NewRandomKey()
	require.NoError(t, err)
	h := certHeader(uuid.New(), 1)

	_, err = protocol.NewCertificateFrame(protocol.CertificateKeys{Private: sender.Private}, key, h, nil)
	assert.ErrorIs(t, err, protocol.ErrNoPeerCertificate)

	_, err = protocol.NewCertificateFrame(protocol.CertificateKeys{PeerPublic: sender.PeerPublic}, key, h, nil)
	assert.ErrorIs(t, err, protocol.ErrNoPrivateKey)
}

// The overhead is what the sender budgets for, and a wrong figure is a chunk that will not fit its frame.
func TestTheOverheadIsWhatTheSenderBudgetsFor(t *testing.T) {
	sender, _ := pair(t)
	key, err := protocol.NewRandomKey()
	require.NoError(t, err)

	payload := make([]byte, 200)
	frame, err := protocol.NewCertificateFrame(sender, key, certHeader(uuid.New(), 1), payload)
	require.NoError(t, err)

	assert.Len(t, frame.Payload,
		len(payload)+protocol.CertificateOverhead+protocol.EncryptionOverhead,
		"a chunk sized against these constants must fit exactly")
}
