package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/db"
)

// The two certificates this side holds: its own, and the one it trusts.
//
// Kept apart by role rather than by a flag, because they are not two of a kind. The local certificate comes
// with a private key and is generated here; the peer certificate is public, arrives from the other machine,
// and must never carry a private key. The schema enforces both, so the wrong half of a pair cannot be
// stored even by a caller that tries — which matters, because a private key copied across the gap is the
// one mistake this scheme cannot survive and the easiest one to make by hand.

// Roles a stored certificate can have.
const (
	// RoleLocal is this machine's own keypair.
	RoleLocal = "local"

	// RolePeer is the other machine's public certificate.
	RolePeer = "peer"
)

// Certificate is one stored certificate, without its private key.
//
// The key is absent from this struct on purpose. It is read by one caller, through PrivateKey below, and a
// field carrying it would eventually be serialised into an API response by someone adding a handler and
// reusing the type that was already there.
type Certificate struct {
	Role           string     `json:"role"`
	CertificatePEM string     `json:"certificate_pem"`
	Fingerprint    string     `json:"fingerprint"`
	Subject        string     `json:"subject"`
	NotBefore      *time.Time `json:"not_before,omitempty"`
	NotAfter       *time.Time `json:"not_after,omitempty"`
	InstalledAt    time.Time  `json:"installed_at"`
	HasPrivateKey  bool       `json:"has_private_key"`
}

// Expired reports whether the certificate's validity window has passed.
func (c Certificate) Expired() bool {
	return c.NotAfter != nil && time.Now().After(*c.NotAfter)
}

// Certificates is the certificate repository.
type Certificates struct{ pool *db.Pool }

// Get returns the certificate in a role, or ErrNotFound.
func (r *Certificates) Get(ctx context.Context, role string) (Certificate, error) {
	var c Certificate
	var key *string
	err := r.pool.QueryRow(ctx, `
		SELECT role, certificate_pem, private_key_pem, fingerprint, subject,
		       not_before, not_after, installed_at
		FROM certificates WHERE role = $1`, role).
		Scan(&c.Role, &c.CertificatePEM, &key, &c.Fingerprint, &c.Subject,
			&c.NotBefore, &c.NotAfter, &c.InstalledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, ErrNotFound
	}
	if err != nil {
		return Certificate{}, err
	}
	c.HasPrivateKey = key != nil && *key != ""
	return c, nil
}

// PrivateKey returns the local certificate's private key PEM.
//
// Separate from Get, and narrow, because this is the only path by which a private key leaves the database.
// Refusing any role but local here means a bug that asked for the peer's key gets nothing rather than
// whatever happened to be in the column.
func (r *Certificates) PrivateKey(ctx context.Context) ([]byte, error) {
	var key *string
	err := r.pool.QueryRow(ctx,
		`SELECT private_key_pem FROM certificates WHERE role = $1`, RoleLocal).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if key == nil || *key == "" {
		return nil, ErrNotFound
	}
	return []byte(*key), nil
}

// SaveLocal stores this machine's own certificate and key, replacing whatever was there.
func (r *Certificates) SaveLocal(ctx context.Context, id protocol.Identity) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO certificates (role, certificate_pem, private_key_pem, fingerprint, subject,
		                          not_before, not_after, installed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (role) DO UPDATE SET
			certificate_pem = EXCLUDED.certificate_pem,
			private_key_pem = EXCLUDED.private_key_pem,
			fingerprint     = EXCLUDED.fingerprint,
			subject         = EXCLUDED.subject,
			not_before      = EXCLUDED.not_before,
			not_after       = EXCLUDED.not_after,
			installed_at    = now()`,
		RoleLocal, string(id.CertificatePEM), string(id.PrivateKeyPEM), id.Fingerprint, id.Subject,
		id.NotBefore, id.NotAfter)
	return err
}

// SavePeer stores the other machine's public certificate, replacing whatever was there.
//
// Takes the PEM rather than an Identity, because a peer certificate is exactly a public certificate and
// there is no private half to pass. Everything else is read out of it here so the caller cannot record a
// fingerprint that does not describe the bytes stored beside it.
func (r *Certificates) SavePeer(ctx context.Context, certPEM []byte) (Certificate, error) {
	parsed, fingerprint, err := protocol.ParseCertificate(certPEM)
	if err != nil {
		return Certificate{}, err
	}
	// Refused here as well as by the schema. A certificate that cannot agree is one this scheme cannot use,
	// and finding that out when the first transfer fails would be a poor way to learn it.
	if _, err := protocol.PublicKeyFrom(certPEM); err != nil {
		return Certificate{}, err
	}

	_, err = r.pool.Exec(ctx, `
		INSERT INTO certificates (role, certificate_pem, private_key_pem, fingerprint, subject,
		                          not_before, not_after, installed_at)
		VALUES ($1, $2, NULL, $3, $4, $5, $6, now())
		ON CONFLICT (role) DO UPDATE SET
			certificate_pem = EXCLUDED.certificate_pem,
			private_key_pem = NULL,
			fingerprint     = EXCLUDED.fingerprint,
			subject         = EXCLUDED.subject,
			not_before      = EXCLUDED.not_before,
			not_after       = EXCLUDED.not_after,
			installed_at    = now()`,
		RolePeer, string(certPEM), fingerprint, parsed.Subject.CommonName,
		parsed.NotBefore, parsed.NotAfter)
	if err != nil {
		return Certificate{}, err
	}
	return r.Get(ctx, RolePeer)
}

// Delete removes a certificate.
func (r *Certificates) Delete(ctx context.Context, role string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM certificates WHERE role = $1`, role)
	return err
}

// Keys assembles what the protocol needs to seal or open: this side's private key and the peer's public.
//
// Returns ErrNotFound when either half is missing, since neither is useful alone — a caller with only its
// own key has nobody to seal to, and one with only the peer's cannot prove who it is.
func (r *Certificates) Keys(ctx context.Context) (protocol.CertificateKeys, error) {
	keyPEM, err := r.PrivateKey(ctx)
	if err != nil {
		return protocol.CertificateKeys{}, err
	}
	peer, err := r.Get(ctx, RolePeer)
	if err != nil {
		return protocol.CertificateKeys{}, err
	}

	priv, err := protocol.PrivateKeyFrom(keyPEM)
	if err != nil {
		return protocol.CertificateKeys{}, err
	}
	pub, err := protocol.PublicKeyFrom([]byte(peer.CertificatePEM))
	if err != nil {
		return protocol.CertificateKeys{}, err
	}
	return protocol.CertificateKeys{Private: priv, PeerPublic: pub}, nil
}
