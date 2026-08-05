-- The sender's core schema: the pipeline's own tables.
--
-- Two conventions run through all of it. Every table carries created_at and, where it
-- is mutable, updated_at, because the operator-facing history views are built on them.
-- And every foreign key states its delete behaviour explicitly: a transmission's chunks
-- and frames are meaningless without it and cascade, while an audit row outlives
-- whatever it describes and does not.

CREATE TABLE files (
    id              uuid        PRIMARY KEY,
    filename        text        NOT NULL,
    -- The name as stored, which is derived rather than taken from the upload. An
    -- uploaded name is attacker-controlled and is never used as a path.
    stored_path     text        NOT NULL,
    size_bytes      bigint      NOT NULL CHECK (size_bytes >= 0),
    sha256          bytea       NOT NULL CHECK (octet_length(sha256) = 32),
    content_type    text        NOT NULL DEFAULT '',
    uploaded_by     uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX files_created_at_idx ON files (created_at DESC);
CREATE INDEX files_sha256_idx ON files (sha256);

-- Compression and encoding profiles are rows rather than configuration so that a
-- transmission records the profile it actually used. Configuration changes; a
-- transmission's history must not change with it.
CREATE TABLE compression_profiles (
    id              uuid        PRIMARY KEY,
    name            text        NOT NULL UNIQUE,
    codec           text        NOT NULL,
    level           integer     NOT NULL CHECK (level BETWEEN 0 AND 9),
    is_default      boolean     NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE encoding_profiles (
    id              uuid        PRIMARY KEY,
    name            text        NOT NULL UNIQUE,
    encoder         text        NOT NULL,
    bit_depth       smallint    NOT NULL CHECK (bit_depth BETWEEN 0 AND 8),
    grid_width      integer     NOT NULL CHECK (grid_width > 0),
    grid_height     integer     NOT NULL CHECK (grid_height > 0),
    cell_pixels     integer     NOT NULL CHECK (cell_pixels > 0),
    quiet_zone      integer     NOT NULL CHECK (quiet_zone >= 0),
    fec_codec       text        NOT NULL,
    fec_data_shards integer     NOT NULL CHECK (fec_data_shards >= 0),
    fec_parity      integer     NOT NULL CHECK (fec_parity >= 0),
    is_default      boolean     NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Only one profile of each kind may be the default, enforced by the database rather
-- than by whichever code path happens to be setting it.
CREATE UNIQUE INDEX compression_profiles_one_default
    ON compression_profiles (is_default) WHERE is_default;
CREATE UNIQUE INDEX encoding_profiles_one_default
    ON encoding_profiles (is_default) WHERE is_default;

CREATE TABLE transmissions (
    id                  uuid        PRIMARY KEY,
    file_id             uuid        NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    status              text        NOT NULL,
    priority            text        NOT NULL DEFAULT 'normal',

    compression_profile uuid        REFERENCES compression_profiles (id) ON DELETE SET NULL,
    encoding_profile    uuid        REFERENCES encoding_profiles (id) ON DELETE SET NULL,

    -- The profile as resolved, copied here so the transmission is self-describing even
    -- after the profile row it came from has been edited or deleted.
    encoder             text        NOT NULL,
    bit_depth           smallint    NOT NULL,
    compression         text        NOT NULL,
    compression_level   integer     NOT NULL,
    fec_codec           text        NOT NULL,
    fec_data_shards     integer     NOT NULL,
    fec_parity_shards   integer     NOT NULL,
    grid_width          integer     NOT NULL,
    grid_height         integer     NOT NULL,
    cell_pixels         integer     NOT NULL,
    quiet_zone          integer     NOT NULL,
    encrypted           boolean     NOT NULL DEFAULT false,

    original_size       bigint      NOT NULL DEFAULT 0,
    compressed_size     bigint      NOT NULL DEFAULT 0,
    chunk_size          integer     NOT NULL DEFAULT 0,
    chunk_count         integer     NOT NULL DEFAULT 0,
    frame_count         integer     NOT NULL DEFAULT 0,

    acked_chunks        integer     NOT NULL DEFAULT 0,
    retransmits         integer     NOT NULL DEFAULT 0,
    dropped_frames      integer     NOT NULL DEFAULT 0,

    error               text        NOT NULL DEFAULT '',
    started_at          timestamptz,
    completed_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT transmissions_status_known CHECK (status IN (
        'pending', 'preparing', 'ready', 'transmitting',
        'paused', 'completed', 'failed', 'cancelled')),
    CONSTRAINT transmissions_priority_known CHECK (priority IN ('high', 'normal', 'low'))
);

CREATE INDEX transmissions_status_idx ON transmissions (status);
CREATE INDEX transmissions_created_at_idx ON transmissions (created_at DESC);
CREATE INDEX transmissions_file_idx ON transmissions (file_id);

CREATE TABLE chunks (
    id              uuid        PRIMARY KEY,
    transmission_id uuid        NOT NULL REFERENCES transmissions (id) ON DELETE CASCADE,
    -- The Encoding Symbol Identifier: below the data-shard count it names a source
    -- chunk, at or above it a repair shard.
    esi             integer     NOT NULL CHECK (esi >= 0),
    block_index     integer     NOT NULL DEFAULT 0 CHECK (block_index >= 0),
    is_parity       boolean     NOT NULL DEFAULT false,
    size_bytes      integer     NOT NULL CHECK (size_bytes >= 0),
    crc32           bigint      NOT NULL,
    sha256          bytea       NOT NULL CHECK (octet_length(sha256) = 32),
    stored_path     text        NOT NULL,
    acked           boolean     NOT NULL DEFAULT false,
    acked_at        timestamptz,
    retry_count     integer     NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (transmission_id, esi)
);

CREATE INDEX chunks_transmission_idx ON chunks (transmission_id);
-- The scheduler's hot query: the unacknowledged chunks of one transmission, in order.
CREATE INDEX chunks_pending_idx ON chunks (transmission_id, esi) WHERE NOT acked;

CREATE TABLE encoded_frames (
    id              uuid        PRIMARY KEY,
    transmission_id uuid        NOT NULL REFERENCES transmissions (id) ON DELETE CASCADE,
    chunk_id        uuid        REFERENCES chunks (id) ON DELETE CASCADE,
    frame_number    integer     NOT NULL CHECK (frame_number >= 0),
    -- A manifest frame carries no chunk, which is why chunk_id is nullable.
    is_manifest     boolean     NOT NULL DEFAULT false,
    flags           integer     NOT NULL DEFAULT 0,
    width_px        integer     NOT NULL CHECK (width_px > 0),
    height_px       integer     NOT NULL CHECK (height_px > 0),
    payload_bytes   integer     NOT NULL CHECK (payload_bytes >= 0),
    stored_path     text        NOT NULL,
    sha256          bytea       NOT NULL CHECK (octet_length(sha256) = 32),
    displayed_count integer     NOT NULL DEFAULT 0,
    last_displayed  timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (transmission_id, frame_number)
);

CREATE INDEX encoded_frames_transmission_idx ON encoded_frames (transmission_id);
CREATE INDEX encoded_frames_chunk_idx ON encoded_frames (chunk_id);

CREATE TABLE display_sessions (
    id              uuid        PRIMARY KEY,
    transmission_id uuid        NOT NULL REFERENCES transmissions (id) ON DELETE CASCADE,
    status          text        NOT NULL,
    sink            text        NOT NULL,
    fps             double precision NOT NULL CHECK (fps > 0),
    brightness      double precision NOT NULL DEFAULT 0,
    gamma           double precision NOT NULL DEFAULT 1 CHECK (gamma > 0),
    window_size     integer     NOT NULL CHECK (window_size > 0),
    frames_shown    bigint      NOT NULL DEFAULT 0,
    started_at      timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz,
    error           text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT display_sessions_status_known CHECK (status IN (
        'running', 'paused', 'stopped', 'completed', 'failed'))
);

CREATE INDEX display_sessions_transmission_idx ON display_sessions (transmission_id);

CREATE TABLE callbacks (
    id              uuid        PRIMARY KEY,
    transmission_id uuid        REFERENCES transmissions (id) ON DELETE CASCADE,
    url             text        NOT NULL,
    event           text        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',
    attempts        integer     NOT NULL DEFAULT 0,
    last_status     integer,
    last_error      text        NOT NULL DEFAULT '',
    payload         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    next_attempt_at timestamptz,
    delivered_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT callbacks_status_known CHECK (status IN ('pending', 'delivered', 'failed'))
);

CREATE INDEX callbacks_pending_idx ON callbacks (next_attempt_at) WHERE status = 'pending';

-- Statistics are append-only samples rather than counters that get updated, so a chart
-- can show how a figure moved rather than only where it ended up.
CREATE TABLE statistics (
    id              bigserial   PRIMARY KEY,
    transmission_id uuid        REFERENCES transmissions (id) ON DELETE CASCADE,
    metric          text        NOT NULL,
    value           double precision NOT NULL,
    recorded_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX statistics_metric_idx ON statistics (metric, recorded_at DESC);
CREATE INDEX statistics_transmission_idx ON statistics (transmission_id, recorded_at DESC);

-- The protocol versions this build can speak, recorded so an operator can see what a
-- deployed binary supports without reading its source.
CREATE TABLE protocol_versions (
    version     integer     PRIMARY KEY,
    supported   boolean     NOT NULL DEFAULT true,
    is_current  boolean     NOT NULL DEFAULT false,
    notes       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX protocol_versions_one_current
    ON protocol_versions (is_current) WHERE is_current;
