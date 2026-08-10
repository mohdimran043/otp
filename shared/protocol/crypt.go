package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
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

// Cipher identities, as written into Header.EncryptionID.
//
// Zero is doing double duty deliberately: on a plaintext frame it means "not encrypted",
// and on a frame that sets FlagEncrypted it means the AES-256-GCM of builds that predate
// the field. Both readings are what the bytes on old frames already say.
const (
	EncryptionNone             uint8 = 0
	EncryptionAES256GCM        uint8 = 1
	EncryptionChaCha20Poly1305 uint8 = 2
)

// ErrEncryptionID means the cipher id is not one this build implements.
var ErrEncryptionID = errors.New("protocol: unknown encryption id")

// EncryptionByName translates an API name into a wire id.
func EncryptionByName(name string) (uint8, error) {
	switch name {
	case "", "none":
		return EncryptionNone, nil
	case "aes256gcm":
		return EncryptionAES256GCM, nil
	case "chacha20poly1305":
		return EncryptionChaCha20Poly1305, nil
	}
	return 0, fmt.Errorf("%w: %q is not one of %s", ErrEncryptionID, name, strings.Join(EncryptionNames(), ", "))
}

// EncryptionName renders a wire id for APIs and logs.
func EncryptionName(id uint8) string {
	switch id {
	case EncryptionAES256GCM:
		return "aes256gcm"
	case EncryptionChaCha20Poly1305:
		return "chacha20poly1305"
	}
	return "none"
}

// EncryptionNames lists the choices a sender can offer.
func EncryptionNames() []string { return []string{"none", "aes256gcm", "chacha20poly1305"} }

// aeadFor builds the AEAD a frame declares. Id zero is legacy AES-256-GCM (see the
// constants). Both ciphers share the 32-byte key, 12-byte nonce and 16-byte tag, which
// is what keeps KeySize and EncryptionOverhead cipher-independent.
func aeadFor(id uint8, key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d", ErrKeySize, len(key))
	}
	switch id {
	case EncryptionNone, EncryptionAES256GCM:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	case EncryptionChaCha20Poly1305:
		return chacha20poly1305.New(key)
	}
	return nil, fmt.Errorf("%w: %d", ErrEncryptionID, id)
}

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
// then sets FlagEncrypted, which NewEncryptedFrame does. The cipher used is the
// caller's id argument, not a header field; NewEncryptedFrame keeps h.EncryptionID
// in sync with it before sealing, which is what lets DecryptPayload read it back.
func EncryptPayload(key []byte, id uint8, plaintext []byte, h Header) ([]byte, error) {
	gcm, err := aeadFor(id, key)
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
// belongs to a different frame. The cipher comes from h.EncryptionID, not a parameter,
// because a receiver never chooses the cipher — the sender did, and recorded which.
func DecryptPayload(key, ciphertext []byte, h Header) ([]byte, error) {
	gcm, err := aeadFor(h.EncryptionID, key)
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

// NewEncryptedFrame builds a frame whose payload is encrypted with the named cipher.
//
// EncryptionNone here does not mean "leave it plaintext" — it selects the legacy
// wire id (see the constants) while still sealing with AES-256-GCM, which is what a
// build that predates EncryptionID always did. Callers that want an actual plaintext
// frame use NewFrame directly.
//
// The integrity fields in the footer cover the ciphertext, not the plaintext, which
// is the right way round: the receiver has to be able to tell a frame it captured
// badly from a frame it decrypted wrongly, and it can only checksum what the camera
// actually saw.
func NewEncryptedFrame(key []byte, id uint8, h Header, plaintext []byte) (*Frame, error) {
	h.EncryptionID = id
	ciphertext, err := EncryptPayload(key, id, plaintext, h)
	if err != nil {
		return nil, err
	}
	h.Flags |= FlagEncrypted
	return NewFrame(h, ciphertext), nil
}

// OpenFrame returns a frame's payload, decrypting it if it is encrypted.
//
// It takes every key the receiver holds rather than one, because keys are chosen per
// transfer on the sender and a receiver cannot know which transfer is on the display.
// Trying each is safe — an AEAD authenticates, so the wrong key fails rather than
// yielding garbage — and the ring is small: however many transfers' keys an operator
// has loaded, not a keyspace.
func OpenFrame(keys [][]byte, f *Frame) ([]byte, error) {
	if f == nil {
		return nil, errors.New("protocol: nil frame")
	}
	if !f.Header.Flags.Has(FlagEncrypted) {
		return f.Payload, nil
	}
	for _, key := range keys {
		if len(key) == 0 {
			continue
		}
		plaintext, err := DecryptPayload(key, f.Payload, f.Header)
		if err == nil {
			return plaintext, nil
		}
		// An unknown cipher id is a property of the frame, not the key: no key in the
		// ring can help, so stop rather than keep failing the same way. A wrong-length
		// key, in contrast, is a property of just that ring entry — a malformed or
		// mismatched key must not veto the keys after it, so that one is skipped and
		// the loop continues.
		if errors.Is(err, ErrEncryptionID) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: no configured key opens this frame", ErrDecrypt)
}
