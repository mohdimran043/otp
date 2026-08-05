-- The receiver's schema.
--
-- Its shape follows from one rule in the design: captured frames are written to disk before
-- being decoded, always. That is what makes a session replayable — a stored session can be
-- decoded again later under a different decoder profile, which is the only way to debug a
-- capture that went wrong after the fact, since the frames themselves are long gone from the
-- display.

CREATE TABLE capture_sessions (
    id                  uuid        PRIMARY KEY,
    transmission_id     uuid,
    status              text        NOT NULL DEFAULT 'capturing',
    source              text        NOT NULL,
    frames_captured     bigint      NOT NULL DEFAULT 0,
    frames_decoded      bigint      NOT NULL DEFAULT 0,
    frames_failed       bigint      NOT NULL DEFAULT 0,
    started_at          timestamptz NOT NULL DEFAULT now(),
    ended_at            timestamptz,
    error               text        NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT capture_sessions_status_known CHECK (status IN (
        'capturing', 'idle', 'stopped', 'completed', 'failed'))
);

CREATE INDEX capture_sessions_transmission_idx ON capture_sessions (transmission_id);

-- Every frame the camera saw, decoded or not.
--
-- The ones that failed are kept deliberately: a frame that could not be read is the primary
-- evidence for why a capture is going badly, and discarding it would leave an operator with a
-- rising failure count and nothing to look at.
CREATE TABLE captured_frames (
    id              uuid        PRIMARY KEY,
    session_id      uuid        NOT NULL REFERENCES capture_sessions (id) ON DELETE CASCADE,
    sequence        bigint      NOT NULL,
    stored_path     text        NOT NULL,
    sha256          bytea       NOT NULL CHECK (octet_length(sha256) = 32),
    decoded         boolean     NOT NULL DEFAULT false,
    decode_error    text        NOT NULL DEFAULT '',

    -- Everything below is known only once the frame decodes, so all of it is nullable.
    transmission_id uuid,
    frame_number    bigint,
    chunk_number    bigint,
    is_manifest     boolean     NOT NULL DEFAULT false,
    is_parity       boolean     NOT NULL DEFAULT false,
    bit_error_rate  double precision NOT NULL DEFAULT 0,
    finder_score    double precision NOT NULL DEFAULT 0,
    timing_score    double precision NOT NULL DEFAULT 0,
    contrast        double precision NOT NULL DEFAULT 0,

    captured_at     timestamptz NOT NULL DEFAULT now(),

    UNIQUE (session_id, sequence)
);

CREATE INDEX captured_frames_session_idx ON captured_frames (session_id, sequence);
CREATE INDEX captured_frames_failed_idx ON captured_frames (session_id) WHERE NOT decoded;

-- What a transmission's manifest declared. It arrives repeatedly, so the row is upserted rather
-- than inserted: a receiver that joined late learns the same thing from a later copy.
CREATE TABLE manifests (
    transmission_id uuid        PRIMARY KEY,
    filename        text        NOT NULL,
    original_size   bigint      NOT NULL,
    original_sha256 bytea       NOT NULL CHECK (octet_length(original_sha256) = 32),
    compressed_size bigint      NOT NULL,
    chunk_count     integer     NOT NULL,
    chunk_size      integer     NOT NULL,
    compression_id  smallint    NOT NULL,
    fec_id          smallint    NOT NULL,
    fec_data_shards integer     NOT NULL,
    fec_parity      integer     NOT NULL,
    shard_size      integer     NOT NULL,

    -- Where the merged file is delivered. It arrived over the optical channel, so it is
    -- attacker-influenced input that will become an outbound request: the receiver checks it
    -- against an allowlist before acting on it, and stores it either way so a refused delivery can
    -- be explained rather than merely failing.
    callback_url    text        NOT NULL DEFAULT '',

    received_at     timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- A chunk that arrived intact. The uniqueness constraint is what makes a duplicate arrival
-- free: a retransmission that crossed an acknowledgement in flight is inserted once and
-- reported as a duplicate rather than corrupting anything.
CREATE TABLE decoded_chunks (
    id              uuid        PRIMARY KEY,
    transmission_id uuid        NOT NULL,
    chunk_number    integer     NOT NULL CHECK (chunk_number >= 0),
    is_parity       boolean     NOT NULL DEFAULT false,
    block_index     integer     NOT NULL DEFAULT 0,
    size_bytes      integer     NOT NULL CHECK (size_bytes >= 0),
    crc32           bigint      NOT NULL,
    sha256          bytea       NOT NULL CHECK (octet_length(sha256) = 32),
    stored_path     text        NOT NULL,
    recovered       boolean     NOT NULL DEFAULT false,
    received_at     timestamptz NOT NULL DEFAULT now(),

    UNIQUE (transmission_id, chunk_number)
);

CREATE INDEX decoded_chunks_transmission_idx ON decoded_chunks (transmission_id, chunk_number);

-- Chunks the receiver knows it is waiting for. Derived from the manifest's chunk count and what
-- has arrived, and kept as rows so the operator UI can show what is outstanding and for how
-- long without recomputing it on every request.
CREATE TABLE missing_chunks (
    transmission_id uuid        NOT NULL,
    chunk_number    integer     NOT NULL,
    first_noticed   timestamptz NOT NULL DEFAULT now(),
    last_reported   timestamptz NOT NULL DEFAULT now(),
    reports         integer     NOT NULL DEFAULT 0,

    PRIMARY KEY (transmission_id, chunk_number)
);

CREATE TABLE merged_files (
    id              uuid        PRIMARY KEY,
    transmission_id uuid        NOT NULL UNIQUE,
    filename        text        NOT NULL,
    stored_path     text        NOT NULL,
    size_bytes      bigint      NOT NULL,
    sha256          bytea       NOT NULL CHECK (octet_length(sha256) = 32),

    -- Whether the merged file matched the hash the manifest declared. A merged file that did not
    -- is kept rather than deleted, because it is the only evidence of what went wrong.
    verified        boolean     NOT NULL DEFAULT false,
    verify_error    text        NOT NULL DEFAULT '',
    verified_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- The acknowledgement sequence, one row per transmission.
--
-- It is a row rather than an in-memory counter because the sender uses the sequence to tell an
-- old record from a new one and to notice a gap. A receiver that restarted and began again at
-- one would produce records the sender had already seen, and its newest reports would look like
-- its oldest.
CREATE TABLE ack_state (
    transmission_id uuid        PRIMARY KEY,
    next_sequence   bigint      NOT NULL DEFAULT 1,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE callbacks (
    id              uuid        PRIMARY KEY,
    transmission_id uuid,
    url             text        NOT NULL,
    event           text        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',
    attempts        integer     NOT NULL DEFAULT 0,
    max_attempts    integer     NOT NULL DEFAULT 8,
    last_status     integer,
    last_error      text        NOT NULL DEFAULT '',
    payload         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    delivered_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT callbacks_status_known CHECK (status IN ('pending', 'delivered', 'failed'))
);

CREATE INDEX callbacks_due_idx ON callbacks (next_attempt_at) WHERE status = 'pending';

CREATE TABLE statistics (
    id              bigserial   PRIMARY KEY,
    transmission_id uuid,
    metric          text        NOT NULL,
    value           double precision NOT NULL,
    recorded_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX statistics_metric_idx ON statistics (metric, recorded_at DESC);
