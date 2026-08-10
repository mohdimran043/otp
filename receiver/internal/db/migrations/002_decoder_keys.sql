-- Decryption keys the operator has loaded. A key ring rather than a single configured
-- key, because the sender chooses a key per transfer and carries it here out of band —
-- the receiver cannot know which transfer the display will show next.
CREATE TABLE decoder_keys (
    id         bigserial   PRIMARY KEY,
    key        bytea       NOT NULL,
    label      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
