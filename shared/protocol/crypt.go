package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// Payload encryption, for transmissions whose contents must not be readable by
// whatever else can see the display.
//
// The threat is specific and worth naming, because it is the one an air gap does not
// address. An optical channel is a broadcast: anything with line of sight to the
// monitor receives every frame, and the protocol is documented, so a second camera in
// the room decodes the file as easily as the intended receiver does. Encryption is
// what makes the channel confidential rather than merely inconvenient to intercept.
const (
	// KeySize is the AES-256 key length.
	KeySize = 32

	// nonceSize is the standard GCM nonce length, and tagSize its authentication tag.
	nonceSize = 12
	tagSize   = 16

	// EncryptionOverhead is how many bytes encryption adds to a payload. The sender
	// subtracts it when sizing chunks, since a chunk still has to fit in one frame
	// after it has been encrypted.
	EncryptionOverhead = nonceSize + tagSize
)

// Encryption errors.
var (
	// ErrKeySize means the key was not 32 bytes.
	ErrKeySize = errors.New("protocol: encryption key must be 32 bytes")

	// ErrDecrypt means the payload failed authentication. It is deliberately
	// undetailed: a payload can fail because the key is wrong, because the ciphertext
	// was altered, or because it was taken from a different frame, and distinguishing
	// those for the caller would also distinguish them for anyone probing the receiver.
	ErrDecrypt = errors.New("protocol: payload failed authentication")
)

// EncryptPayload encrypts a chunk for a frame, binding it to that frame's identity.
//
// The header fields go in as additional authenticated data rather than being
// encrypted, which is what stops a payload being moved. Without that binding, an
// interceptor could replay chunk five of one transmission as chunk nine of another
// and the receiver would decrypt it happily, assemble it in the wrong place, and only
// discover the problem at the file's final hash — if at all, since a file that
// contains the right blocks in the wrong order can still be a valid file of its type.
// Binding makes a chunk decryptable only in the position it was sent in.
//
// The header must already carry the transmission id and chunk numbering; the caller
// then sets FlagEncrypted, which NewEncryptedFrame does.
func EncryptPayload(key, plaintext []byte, h Header) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	// A fresh random nonce each time. A nonce derived from the frame's identity would
	// also be unique — transmission ids are fresh and chunk numbers do not repeat
	// within one — but it would stake the security of the whole scheme on that
	// remaining true through every future change to how frames are numbered, and there
	// is no benefit worth that.
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("protocol: could not read a nonce: %w", err)
	}

	out := make([]byte, 0, nonceSize+len(plaintext)+tagSize)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, frameAAD(h)), nil
}

// DecryptPayload reverses EncryptPayload, and fails if the payload was altered or
// belongs to a different frame.
func DecryptPayload(key, ciphertext []byte, h Header) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < EncryptionOverhead {
		return nil, fmt.Errorf("%w: %d bytes cannot hold a nonce and a tag", ErrDecrypt, len(ciphertext))
	}

	plaintext, err := gcm.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], frameAAD(h))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// frameAAD is the frame identity a payload is bound to.
//
// It deliberately excludes the fields that legitimately differ between a frame and
// its retransmission — the flags, the timestamp, the frame number — and includes
// only what identifies the *chunk*: which transmission it belongs to and where in
// that transmission it sits. A retransmitted chunk has a new frame number and a new
// timestamp, and must still decrypt.
//
// The session id is excluded for the same reason: a chunk resent after the operator
// restarted the display belongs to a new display session but to the same
// transmission, and it has to remain readable.
func frameAAD(h Header) []byte {
	aad := make([]byte, 0, len(h.TransmissionID)+8)
	aad = append(aad, h.TransmissionID[:]...)
	aad = binary.BigEndian.AppendUint32(aad, h.ChunkNumber)
	aad = binary.BigEndian.AppendUint32(aad, h.TotalChunks)
	return aad
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d", ErrKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// NewEncryptedFrame builds a frame whose payload is encrypted.
//
// The integrity fields in the footer cover the ciphertext, not the plaintext, which
// is the right way round: the receiver has to be able to tell a frame it captured
// badly from a frame it decrypted wrongly, and it can only checksum what the camera
// actually saw.
func NewEncryptedFrame(key []byte, h Header, plaintext []byte) (*Frame, error) {
	ciphertext, err := EncryptPayload(key, plaintext, h)
	if err != nil {
		return nil, err
	}
	h.Flags |= FlagEncrypted
	return NewFrame(h, ciphertext), nil
}

// OpenFrame returns a frame's payload, decrypting it if it is encrypted.
//
// Frames that are not encrypted pass through, so a receiver can hand every frame to
// the same call rather than branching on the flag — and a receiver configured with a
// key still accepts a plaintext transmission, which is what makes changing the
// sender's encryption setting mid-deployment survivable.
func OpenFrame(key []byte, f *Frame) ([]byte, error) {
	if f == nil {
		return nil, errors.New("protocol: nil frame")
	}
	if !f.Header.Flags.Has(FlagEncrypted) {
		return f.Payload, nil
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: the frame is encrypted and no key is configured", ErrDecrypt)
	}
	return DecryptPayload(key, f.Payload, f.Header)
}
