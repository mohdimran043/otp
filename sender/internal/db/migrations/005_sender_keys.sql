-- Encryption keys the operator has saved, so a transfer can be created against one that was
-- generated earlier rather than pasted in again every time. The sender still accepts a key hex
-- directly on a request; this is the alternative for a key an operator expects to reuse.
CREATE TABLE sender_keys (
    id         bigserial   PRIMARY KEY,
    key        bytea       NOT NULL,
    label      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
