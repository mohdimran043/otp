-- The job engine, its logs, and the identities that drive it.
--
-- Jobs are rows rather than in-memory work because every stage of the pipeline has to
-- survive a restart. A file uploaded, compressed, and chunked represents real time
-- spent; a process that lost that on a deploy would make the platform unusable for the
-- large files it exists to move.

CREATE TABLE jobs (
    id              uuid        PRIMARY KEY,
    type            text        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',
    priority        text        NOT NULL DEFAULT 'normal',

    -- The subject of the work. Both are nullable because some jobs are housekeeping and
    -- belong to no transmission.
    transmission_id uuid        REFERENCES transmissions (id) ON DELETE CASCADE,
    file_id         uuid        REFERENCES files (id) ON DELETE CASCADE,

    payload         jsonb       NOT NULL DEFAULT '{}'::jsonb,

    -- A job becomes runnable only when every job listed here has completed. Held as an
    -- array rather than a join table because it is read on every claim attempt and is
    -- never queried from the other direction.
    depends_on      uuid[]      NOT NULL DEFAULT '{}',

    attempts        integer     NOT NULL DEFAULT 0,
    max_attempts    integer     NOT NULL DEFAULT 5,

    -- run_after is how both backoff and scheduling are expressed: a retry sets it into
    -- the future, and so does a job that is meant to start later.
    run_after       timestamptz NOT NULL DEFAULT now(),

    -- claimed_by and claimed_at let a job be taken over if the worker holding it dies.
    -- Without them a crash would leave work claimed for ever.
    claimed_by      text,
    claimed_at      timestamptz,

    progress        integer     NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    message         text        NOT NULL DEFAULT '',
    error           text        NOT NULL DEFAULT '',

    -- control is a request rather than a state, because a running job can only be
    -- stopped by its own handler noticing. Setting it cancels the handler's context; the
    -- status changes when the handler actually returns, and which status it changes to
    -- depends on what was asked for. Pause and cancel have to be distinguishable here
    -- rather than collapsed into one flag: a paused job is resumed without having spent
    -- an attempt, and a cancelled one is finished.
    control         text        NOT NULL DEFAULT '',

    started_at      timestamptz,
    finished_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT jobs_status_known CHECK (status IN (
        'pending', 'running', 'completed', 'failed', 'cancelled', 'paused')),
    CONSTRAINT jobs_priority_known CHECK (priority IN ('high', 'normal', 'low')),
    CONSTRAINT jobs_control_known CHECK (control IN ('', 'pause', 'cancel'))
);

-- The claim query's index. It covers exactly the predicate a worker uses — pending,
-- runnable now — and orders by the priority and age it claims in, so claiming does not
-- scan the finished jobs that accumulate over a deployment's life.
CREATE INDEX jobs_claimable_idx
    ON jobs (priority, run_after, created_at)
    WHERE status = 'pending';

CREATE INDEX jobs_transmission_idx ON jobs (transmission_id, created_at DESC);
CREATE INDEX jobs_status_idx ON jobs (status);
CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE finished_at IS NOT NULL;

CREATE TABLE job_logs (
    id          bigserial   PRIMARY KEY,
    job_id      uuid        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    level       text        NOT NULL DEFAULT 'info',
    message     text        NOT NULL,
    fields      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    logged_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX job_logs_job_idx ON job_logs (job_id, id);

CREATE TABLE users (
    id              uuid        PRIMARY KEY,
    username        text        NOT NULL UNIQUE,
    -- A bcrypt hash. The column is named for what it holds so that nobody can mistake
    -- it for something a password could be compared against directly.
    password_hash   text        NOT NULL,
    role            text        NOT NULL,
    active          boolean     NOT NULL DEFAULT true,
    last_login_at   timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_role_known CHECK (role IN ('admin', 'operator', 'viewer'))
);

-- Audit rows outlive what they describe, which is the whole point of an audit trail:
-- the record of who deleted a transmission must survive the transmission. So the
-- references are recorded as plain identifiers with no foreign key, and the actor is
-- kept as a name as well as an id in case the user is later removed.
CREATE TABLE audit_logs (
    id              bigserial   PRIMARY KEY,
    actor_id        uuid,
    actor_username  text        NOT NULL DEFAULT '',
    action          text        NOT NULL,
    resource_type   text        NOT NULL,
    resource_id     text        NOT NULL DEFAULT '',
    detail          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    request_id      text        NOT NULL DEFAULT '',
    remote_addr     text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_id, created_at DESC);
CREATE INDEX audit_logs_resource_idx ON audit_logs (resource_type, resource_id);
