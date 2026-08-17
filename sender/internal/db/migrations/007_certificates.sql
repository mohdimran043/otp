-- The certificates this side identifies itself with, and the one it trusts.
--
-- Two rows at most, and the distinction between them is the whole scheme. The 'local' row is this machine's
-- own keypair: it holds a private key that never leaves here. The 'peer' row is the other machine's public
-- certificate, installed by an operator, and holds no private key at all — that is why it can be copied
-- between machines by any means, including ones nobody would trust with a secret.
--
-- A table rather than files on disk, so the pair survives a container being rebuilt and so an operator can
-- replace them through the API without a shell on the host. The private key is stored as it was generated;
-- a deployment that needs it protected at rest should encrypt the volume, which is the same answer as for
-- every other secret this database already holds.
CREATE TABLE certificates (
    -- 'local' or 'peer'. One row each, which is what the primary key enforces: replacing a certificate is an
    -- upsert rather than an insert, so there is never a moment with two and no way to say which is current.
    role            text        PRIMARY KEY,

    certificate_pem text        NOT NULL,

    -- Present only on the local row. A peer certificate with a private key would mean somebody had copied
    -- the wrong half of a pair across the gap, which is worth making structurally impossible to store.
    private_key_pem text,

    -- The SHA-256 of the certificate's DER, grouped for reading aloud. Kept rather than computed on every
    -- request because it is what an operator compares between two screens, and it must be the same string
    -- both times regardless of who formats it.
    fingerprint     text        NOT NULL,

    subject         text        NOT NULL DEFAULT '',
    not_before      timestamptz,
    not_after       timestamptz,

    installed_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT certificates_role_check CHECK (role IN ('local', 'peer')),
    -- A local certificate without its key cannot seal anything, and a peer certificate with one is a
    -- private key that has crossed the gap. Both are refused here rather than discovered at transfer time.
    CONSTRAINT certificates_key_matches_role CHECK (
        (role = 'local' AND private_key_pem IS NOT NULL) OR
        (role = 'peer'  AND private_key_pem IS NULL)
    )
);
