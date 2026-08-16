-- What actually happened to a captured frame, kept rather than only logged.
--
-- The receiver already recorded the verdict — decoded or not, and the error if not — which answers
-- "did this work" and nothing else. When a transfer goes badly the questions are all the ones under
-- that: which stage did it die at, was it rescued and by what, how long did that take, and on a tiled
-- display which lane of the photograph is this row even talking about. All of it existed at the
-- moment of decoding and was written to the log, where it cannot be joined to the picture it
-- describes and is gone as soon as the container is recreated.
--
-- Every column here is a value the pipeline already computes on its way to a verdict, so recording
-- them costs a wider insert and nothing else.

ALTER TABLE captured_frames
    -- The stage the decode died at, as classify.Of names it: no_quad, descriptor_crc, header_crc,
    -- payload_crc, below_floors, and so on. Empty for a frame that read cleanly.
    --
    -- This is the field that tells an operator what to *do*, and the two commonest values call for
    -- opposite actions: no_quad is a camera that cannot see the fiducials, payload_crc is a camera
    -- that sees them perfectly and is misreading cells. Aiming advice for one is wrong for the other.
    ADD COLUMN failure_stage text NOT NULL DEFAULT '',

    -- Whether the recovery engine rescued this frame, and which engine did it.
    --
    -- Distinct from `decoded`, deliberately. A recovered frame is decoded — its payload matched both
    -- the footer's CRC32 and its SHA-256 — but it did not decode *on the first read*, and a session
    -- where most successes came from recovery is a session one camera nudge away from failing. That
    -- distinction is invisible if recovery is folded into the verdict.
    ADD COLUMN recovered boolean NOT NULL DEFAULT false,
    ADD COLUMN recovery_engine text NOT NULL DEFAULT '',
    ADD COLUMN recovery_stage text NOT NULL DEFAULT '',
    ADD COLUMN recovery_candidates integer NOT NULL DEFAULT 0,
    ADD COLUMN recovery_flips integer NOT NULL DEFAULT 0,
    ADD COLUMN recovery_ms double precision NOT NULL DEFAULT 0,

    -- How many photographs of this same displayed frame were combined to read it, when that is what
    -- read it. One means the frame was read from a single shot.
    ADD COLUMN merged_shots integer NOT NULL DEFAULT 0,

    -- Which lane of the photograph this row describes, and how many lanes the photograph held.
    --
    -- A tiled display puts several independent frames in one picture and each becomes its own row
    -- against the same stored image, so without these two the rows are indistinguishable: several
    -- results, one photograph, nothing saying which part of it any of them came from.
    ADD COLUMN lane_index integer NOT NULL DEFAULT 0,
    ADD COLUMN lane_count integer NOT NULL DEFAULT 1,

    -- How long the decode itself took, before any recovery.
    ADD COLUMN decode_ms double precision NOT NULL DEFAULT 0;

-- Reading a transmission's frames back is a first-class operation now, not a diagnostic detour: the
-- transfer page lists them and an operator downloads the set to replay against the corpus harness.
-- Without this that listing is a sequential scan of every capture the receiver has ever taken.
CREATE INDEX captured_frames_transmission_idx
    ON captured_frames (transmission_id, captured_at)
    WHERE transmission_id IS NOT NULL;

-- Finding the frames that died at one stage, which is how a session's dominant failure is read.
CREATE INDEX captured_frames_stage_idx
    ON captured_frames (session_id, failure_stage)
    WHERE failure_stage <> '';
