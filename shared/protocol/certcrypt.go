package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Encryption where the two sides hold certificates rather than a shared secret.
//
// The modes beside this one take a symmetric key an operator carries across the gap by hand — typed,
// pasted, or loaded from a file — and that is their weakness rather than their strength: the key has to
// reach the receiver somehow, and every way of moving it is a way of losing it. This mode moves nothing.
// Each side generates a keypair once, and each is given the other's *public* certificate, which is not a
// secret and can travel by any means at all.
//
// What the frame carries is a fresh AES-256 key, generated per transfer, sealed so that only the intended
// receiver can open it:
//
//	shared   = ECDH(sender private, receiver public)      — which the receiver recomputes as
//	           ECDH(receiver private, sender public), the same value by construction
//	wrapKey  = HKDF-SHA256(shared, salt = transmission id)
//	sealed   = AES-256-GCM(wrapKey).Seal(random AES key)
//	payload  = AES-256-GCM(random AES key).Seal(chunk)
//
// Both properties the operator asked for fall out of that. Only the receiver can derive the shared value,
// because it needs a private key nobody else has — that is the confidentiality. And only the sender could
// have produced it, because it needs *that* private key — so a frame that opens is a frame the holder of
// the sender's certificate sent, which is the authenticity. Neither is a signature bolted on afterwards;
// they are the same ECDH result read two ways.
//
// The wrapped key rides in every frame rather than in the manifest alone. That costs 60 bytes a frame and
// buys the property the rest of this system is built on: a frame is independent. A receiver joining a
// transfer halfway through decrypts what it sees, in any order, without having caught a particular earlier
// frame — which is exactly why the manifest is re-emitted rather than sent once, and it would be odd for
// encryption to reintroduce the dependency the rest of the design removes.
//
// P-256 rather than RSA, and the reason is the frame budget rather than taste. An RSA-2048 wrapped key is
// 256 bytes; at grid 80 a frame carries 877, so RSA would spend 29% of every frame on key transport.
// ECDH spends 60 bytes — a nonce, the key, and a tag.

const (
	// EncryptionCertificate is the wire id for this mode. See the ids beside it in crypt.go.
	EncryptionCertificate uint8 = 3

	// wrappedKeySize is the sealed AES key: a nonce, the key itself, and the AEAD tag.
	wrappedKeySize = nonceSize + KeySize + tagSize

	// CertificateOverhead is what this mode adds beyond an ordinary encrypted payload, so the sender can
	// size chunks that still fit a frame once the wrapped key is in front of them.
	CertificateOverhead = wrappedKeySize

	// hkdfInfo separates this use of the shared value from any other. Two protocols deriving keys from one
	// ECDH result must not derive the same key, and the info string is what guarantees they do not.
	hkdfInfo = "otp/certificate-wrap/v1"
)

// Certificate encryption errors.
var (
	// ErrNoPeerCertificate means this side has not been given the other's public certificate, so there is
	// nobody to seal to or nobody to verify against.
	ErrNoPeerCertificate = errors.New("protocol: no peer certificate is installed")

	// ErrNoPrivateKey means this side has no keypair of its own.
	ErrNoPrivateKey = errors.New("protocol: no private key is installed")

	// ErrWrappedKey means the sealed key would not open. Undetailed for the same reason ErrDecrypt is:
	// distinguishing "wrong certificate" from "altered frame" for a caller distinguishes it for a prober.
	ErrWrappedKey = errors.New("protocol: the sealed key failed authentication")
)

// CertificateKeys is one side's half of the arrangement: its own private key and the other side's public.
//
// Both are needed in both directions, which is what makes this mutual rather than one-way. A sender seals
// with its private key and the receiver's public; the receiver opens with its private and the sender's
// public. Neither can do the other's job, and a frame proves the sender held the private key that matches
// the certificate the receiver was given.
type CertificateKeys struct {
	// Private is this side's own key, from its certificate.
	Private *ecdh.PrivateKey

	// PeerPublic is the other side's, from the certificate installed here.
	PeerPublic *ecdh.PublicKey
}

// Valid reports whether both halves are present, which is the check a caller wants before offering this
// mode at all: a sender with no peer certificate cannot encrypt to anyone.
func (k CertificateKeys) Valid() bool {
	return k.Private != nil && k.PeerPublic != nil
}

// wrapKeyFor derives the key that seals the per-transfer AES key.
//
// Salted with the transmission id so that two transfers between the same pair of certificates do not share
// a wrapping key. The ECDH value is static — the same two certificates always agree on it — so without the
// salt every frame of every transfer this pair ever exchanged would be sealed under one key, and the
// 96-bit random nonces would be the only thing standing between them and a collision. Per transfer, the
// number of frames under any one key is bounded by the transfer, which is the bound worth having.
func wrapKeyFor(k CertificateKeys, transmission [16]byte) ([]byte, error) {
	if k.Private == nil {
		return nil, ErrNoPrivateKey
	}
	if k.PeerPublic == nil {
		return nil, ErrNoPeerCertificate
	}

	shared, err := k.Private.ECDH(k.PeerPublic)
	if err != nil {
		return nil, fmt.Errorf("protocol: the certificates do not agree: %w", err)
	}

	out := make([]byte, KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, transmission[:], []byte(hkdfInfo)), out); err != nil {
		return nil, fmt.Errorf("protocol: could not derive a wrapping key: %w", err)
	}
	return out, nil
}

// SealKey wraps a per-transfer AES key for the frame described by h.
//
// The frame header is the AEAD's additional data, exactly as it is for the payload, so a sealed key lifted
// from one frame and pasted into another fails to open. Without that an attacker could not read anything —
// the payload is still sealed — but could shuffle keys between frames and turn a decode failure into a
// confusing one.
func SealKey(k CertificateKeys, aesKey []byte, h Header) ([]byte, error) {
	if len(aesKey) != KeySize {
		return nil, ErrKeySize
	}
	wrapKey, err := wrapKeyFor(k, h.TransmissionID)
	if err != nil {
		return nil, err
	}
	gcm, err := gcmFor(wrapKey)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("protocol: could not read a nonce: %w", err)
	}

	out := make([]byte, 0, wrappedKeySize)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, aesKey, frameAAD(h)), nil
}

// OpenKey reverses SealKey.
func OpenKey(k CertificateKeys, sealed []byte, h Header) ([]byte, error) {
	if len(sealed) != wrappedKeySize {
		return nil, fmt.Errorf("%w: %d bytes is not a sealed key", ErrWrappedKey, len(sealed))
	}
	wrapKey, err := wrapKeyFor(k, h.TransmissionID)
	if err != nil {
		return nil, err
	}
	gcm, err := gcmFor(wrapKey)
	if err != nil {
		return nil, err
	}

	aesKey, err := gcm.Open(nil, sealed[:nonceSize], sealed[nonceSize:], frameAAD(h))
	if err != nil {
		return nil, ErrWrappedKey
	}
	return aesKey, nil
}

// NewCertificateFrame builds a frame whose payload is sealed under a fresh key, itself sealed to the peer.
//
// The random key is generated here rather than taken from the caller so there is no path by which a
// transfer reuses one. "Random per transfer" is what the operator asked for; per *frame* would be no less
// safe and would cost nothing extra to seal, but it would mean a receiver that recovered a frame's cells
// could not use what it had learned about the transfer — and reusing one key across a transfer is what lets
// the merge across several photographs work at all.
func NewCertificateFrame(k CertificateKeys, aesKey []byte, h Header, plaintext []byte) (*Frame, error) {
	if !k.Valid() {
		if k.Private == nil {
			return nil, ErrNoPrivateKey
		}
		return nil, ErrNoPeerCertificate
	}

	h.EncryptionID = EncryptionCertificate
	h.Flags |= FlagEncrypted

	sealedKey, err := SealKey(k, aesKey, h)
	if err != nil {
		return nil, err
	}

	// The payload is sealed with the ordinary AES-256-GCM path, so this mode adds a key-transport layer
	// rather than a second cipher: the bytes carrying the chunk are protected exactly as they are for a
	// transfer whose key was carried across by hand.
	body, err := EncryptPayload(aesKey, EncryptionAES256GCM, plaintext, h)
	if err != nil {
		return nil, err
	}

	payload := make([]byte, 0, len(sealedKey)+len(body))
	payload = append(payload, sealedKey...)
	payload = append(payload, body...)
	return NewFrame(h, payload), nil
}

// OpenCertificateFrame returns a certificate-encrypted frame's plaintext.
func OpenCertificateFrame(k CertificateKeys, f *Frame) ([]byte, error) {
	if f == nil {
		return nil, errors.New("protocol: nil frame")
	}
	if len(f.Payload) < wrappedKeySize+EncryptionOverhead {
		return nil, fmt.Errorf("%w: %d bytes cannot hold a sealed key and a payload",
			ErrDecrypt, len(f.Payload))
	}

	aesKey, err := OpenKey(k, f.Payload[:wrappedKeySize], f.Header)
	if err != nil {
		return nil, err
	}

	// The header says "certificate", but the body under the wrapped key is an ordinary AES-256-GCM
	// payload, so it is opened as one. Passing the header through unchanged would have DecryptPayload
	// look for a cipher named "certificate" and find none.
	body := f.Header
	body.EncryptionID = EncryptionAES256GCM
	return DecryptPayload(aesKey, f.Payload[wrappedKeySize:], body)
}

// gcmFor is AES-256-GCM over a 32-byte key.
func gcmFor(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// NewRandomKey returns a fresh AES-256 key.
func NewRandomKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("protocol: could not read a key: %w", err)
	}
	return key, nil
}
