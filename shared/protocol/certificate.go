package protocol

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// The certificates the two sides identify themselves with.
//
// Self-signed, and deliberately so. A certificate authority answers "is this party who they claim to be to
// the world", and nothing here needs that: there are two machines, an operator installs each side's public
// certificate on the other by hand, and the trust is that installation rather than a signature from a third
// party. Adding a CA would add a component to run, a chain to validate, and an expiry to renew, in exchange
// for a question nobody is asking. What the certificate provides is a *format* — a standard container for a
// public key with an identity and a validity window, which every tool can read and an operator can inspect.
//
// P-256 because the key has to be agreed with, not merely signed with. See certcrypt.go for why the key
// size matters here in a way it usually does not: this key's agreement output wraps a per-transfer AES key
// that rides in every frame, and a frame carries under a kilobyte.

// certificateValidity is how long a generated certificate is good for.
//
// Ten years, which is long for a web certificate and right for this one. The renewal story for a browser
// certificate is automated; here it is an operator walking to two machines. An expiry that arrives
// unexpectedly stops a transfer with an error about dates, which is the worst possible time to learn about
// certificate management — so the window is set past the life of the deployment and the operator replaces
// them when they choose to rather than when a clock says so.
const certificateValidity = 10 * 365 * 24 * time.Hour

// Certificate errors.
var (
	// ErrNotACertificate means the PEM did not parse, or held something other than a certificate.
	ErrNotACertificate = errors.New("protocol: not a certificate")

	// ErrWrongKeyType means the certificate carries a key this scheme cannot agree with. RSA and Ed25519
	// certificates are perfectly valid certificates and cannot do ECDH, which is what this needs.
	ErrWrongKeyType = errors.New("protocol: the certificate does not carry an agreement key")
)

// Identity is a generated keypair and the certificate that publishes its public half.
type Identity struct {
	// CertificatePEM is the public half, and the only part that crosses to the other side.
	CertificatePEM []byte

	// PrivateKeyPEM is the secret half, which never leaves the machine that generated it.
	PrivateKeyPEM []byte

	// Fingerprint identifies the certificate, for an operator confirming both sides hold the matching pair.
	Fingerprint string

	// Subject, NotBefore and NotAfter are what the certificate says about itself.
	Subject   string
	NotBefore time.Time
	NotAfter  time.Time
}

// NewIdentity generates a keypair and a self-signed certificate for it.
//
// name goes in the subject so an operator looking at two certificates can tell which is which — "otp-sender"
// against "otp-receiver" is the whole of the identity this needs.
func NewIdentity(name string) (Identity, error) {
	if strings.TrimSpace(name) == "" {
		name = "otp"
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("protocol: could not generate a key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Identity{}, fmt.Errorf("protocol: could not choose a serial: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		// A minute of backdating, because the two machines' clocks are not synchronised and a certificate
		// that is not yet valid on the other side is a confusing way to discover that.
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(certificateValidity),
		KeyUsage:              x509.KeyUsageKeyAgreement | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return Identity{}, fmt.Errorf("protocol: could not sign the certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Identity{}, fmt.Errorf("protocol: could not encode the key: %w", err)
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return Identity{}, fmt.Errorf("protocol: generated an unreadable certificate: %w", err)
	}

	return Identity{
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Fingerprint:    FingerprintOf(der),
		Subject:        parsed.Subject.CommonName,
		NotBefore:      parsed.NotBefore,
		NotAfter:       parsed.NotAfter,
	}, nil
}

// FingerprintOf is the SHA-256 of a certificate's DER, grouped for reading aloud.
//
// Grouped because that is what it is for: an operator reads it off one screen and checks it against the
// other, and sixty-four unbroken hex characters is a string nobody compares correctly.
func FingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	hexed := hex.EncodeToString(sum[:])

	var b strings.Builder
	for i := 0; i < len(hexed); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(hexed[i : i+4])
	}
	return b.String()
}

// ParseCertificate reads a PEM certificate and returns it with its fingerprint.
func ParseCertificate(pemBytes []byte) (*x509.Certificate, string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", fmt.Errorf("%w: expected a PEM block of type CERTIFICATE", ErrNotACertificate)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrNotACertificate, err)
	}
	return cert, FingerprintOf(block.Bytes), nil
}

// PublicKeyFrom extracts the agreement key a certificate publishes.
func PublicKeyFrom(pemBytes []byte) (*ecdh.PublicKey, error) {
	cert, _, err := ParseCertificate(pemBytes)
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: it carries a %T", ErrWrongKeyType, cert.PublicKey)
	}
	// An ECDSA key on a named curve is the same point as an ECDH key on that curve; this is the conversion
	// rather than a re-derivation, which is why a certificate can serve both purposes.
	key, err := pub.ECDH()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWrongKeyType, err)
	}
	return key, nil
}

// PrivateKeyFrom reads a PEM private key as an agreement key.
func PrivateKeyFrom(pemBytes []byte) (*ecdh.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("protocol: not a PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Older tooling writes SEC 1 rather than PKCS#8, and refusing a key an operator generated with
		// openssl would be an unhelpful way to be strict.
		ec, ecErr := x509.ParseECPrivateKey(block.Bytes)
		if ecErr != nil {
			return nil, fmt.Errorf("protocol: could not read the private key: %w", err)
		}
		parsed = ec
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: it is a %T", ErrWrongKeyType, parsed)
	}
	return key.ECDH()
}

// CheckPair reports whether a certificate and a private key belong together.
//
// Worth checking at the moment they are installed rather than at the moment a transfer fails. A mismatched
// pair produces frames nobody can open, and the error at that point — a sealed key that will not
// authenticate — says nothing about which of the four certificates involved is the wrong one.
func CheckPair(certPEM, keyPEM []byte) error {
	pub, err := PublicKeyFrom(certPEM)
	if err != nil {
		return err
	}
	priv, err := PrivateKeyFrom(keyPEM)
	if err != nil {
		return err
	}
	if !priv.PublicKey().Equal(pub) {
		return errors.New("protocol: the private key does not match the certificate")
	}
	return nil
}
