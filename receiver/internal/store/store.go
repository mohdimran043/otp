// Package store is the receiver's data access layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opticaltransport/otp/receiver/internal/db"
)

// ErrNotFound means no row matched.
var ErrNotFound = errors.New("store: not found")

// Store holds every repository.
type Store struct {
	pool *db.Pool

	Sessions      *Sessions
	Frames        *Frames
	Manifests     *Manifests
	Chunks        *Chunks
	Merged        *Merged
	Acks          *Acks
	Callbacks     *Callbacks
	Stats         *Stats
	DecoderKeys   *DecoderKeys
	Transmissions *Transmissions
}

// New returns a store over a connection pool.
func New(pool *db.Pool) *Store {
	return &Store{
		pool:          pool,
		Sessions:      &Sessions{pool: pool},
		Frames:        &Frames{pool: pool},
		Manifests:     &Manifests{pool: pool},
		Chunks:        &Chunks{pool: pool},
		Merged:        &Merged{pool: pool},
		Acks:          &Acks{pool: pool},
		Callbacks:     &Callbacks{pool: pool},
		Stats:         &Stats{pool: pool},
		DecoderKeys:   &DecoderKeys{pool: pool},
		Transmissions: &Transmissions{pool: pool},
	}
}

// Pool exposes the underlying pool.
func (s *Store) Pool() *db.Pool { return s.pool }

// Session is one capture run.
type Session struct {
	ID             uuid.UUID  `json:"id"`
	TransmissionID *uuid.UUID `json:"transmission_id,omitempty"`
	Status         string     `json:"status"`
	Source         string     `json:"source"`
	FramesCaptured int64      `json:"frames_captured"`
	FramesDecoded  int64      `json:"frames_decoded"`
	FramesFailed   int64      `json:"frames_failed"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// Sessions is the capture-session repository.
type Sessions struct{ pool *db.Pool }

// Create opens a capture session.
func (r *Sessions) Create(ctx context.Context, source string) (Session, error) {
	var s Session
	err := r.pool.QueryRow(ctx, `
		INSERT INTO capture_sessions (id, source) VALUES ($1, $2)
		RETURNING id, transmission_id, status, source, frames_captured, frames_decoded,
		          frames_failed, started_at, ended_at, error`,
		uuid.New(), source).Scan(&s.ID, &s.TransmissionID, &s.Status, &s.Source,
		&s.FramesCaptured, &s.FramesDecoded, &s.FramesFailed, &s.StartedAt, &s.EndedAt, &s.Error)
	return s, err
}

// Get returns one session.
func (r *Sessions) Get(ctx context.Context, id uuid.UUID) (Session, error) {
	var s Session
	err := r.pool.QueryRow(ctx, `
		SELECT id, transmission_id, status, source, frames_captured, frames_decoded,
		       frames_failed, started_at, ended_at, error
		FROM capture_sessions WHERE id = $1`, id).Scan(&s.ID, &s.TransmissionID, &s.Status,
		&s.Source, &s.FramesCaptured, &s.FramesDecoded, &s.FramesFailed, &s.StartedAt,
		&s.EndedAt, &s.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return s, err
}

// Count records what a capture pass saw.
//
// The three figures are incremented together in one statement so they cannot disagree: a captured
// frame is always either decoded or failed, and updating them separately would let an observer
// read a moment where it was neither.
func (r *Sessions) Count(ctx context.Context, id uuid.UUID, captured, decoded, failed int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE capture_sessions SET
			frames_captured = frames_captured + $2,
			frames_decoded  = frames_decoded + $3,
			frames_failed   = frames_failed + $4,
			updated_at = now()
		WHERE id = $1`, id, captured, decoded, failed)
	return err
}

// Bind records which transmission a session is capturing, once a frame has said so.
func (r *Sessions) Bind(ctx context.Context, id, transmissionID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE capture_sessions SET transmission_id = $2, updated_at = now() WHERE id = $1`,
		id, transmissionID)
	return err
}

// Finish closes a session.
func (r *Sessions) Finish(ctx context.Context, id uuid.UUID, status, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE capture_sessions SET status = $2, error = $3, ended_at = now(), updated_at = now()
		WHERE id = $1`, id, status, reason)
	return err
}

// CapturedFrame is one image the camera saw.
type CapturedFrame struct {
	ID             uuid.UUID  `json:"id"`
	SessionID      uuid.UUID  `json:"session_id"`
	Sequence       int64      `json:"sequence"`
	StoredPath     string     `json:"stored_path"`
	SHA256         []byte     `json:"sha256"`
	Decoded        bool       `json:"decoded"`
	DecodeError    string     `json:"decode_error,omitempty"`
	TransmissionID *uuid.UUID `json:"transmission_id,omitempty"`
	FrameNumber    *int64     `json:"frame_number,omitempty"`
	ChunkNumber    *int64     `json:"chunk_number,omitempty"`
	IsManifest     bool       `json:"is_manifest"`
	IsParity       bool       `json:"is_parity"`
	BitErrorRate   float64    `json:"bit_error_rate"`
	FinderScore    float64    `json:"finder_score"`
	TimingScore    float64    `json:"timing_score"`
	Contrast       float64    `json:"contrast"`
	CapturedAt     time.Time  `json:"captured_at"`
}

// Frames is the captured-frame repository.
type Frames struct{ pool *db.Pool }

// Record writes a captured frame's row.
func (r *Frames) Record(ctx context.Context, f CapturedFrame) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO captured_frames (id, session_id, sequence, stored_path, sha256, decoded,
			decode_error, transmission_id, frame_number, chunk_number, is_manifest, is_parity,
			bit_error_rate, finder_score, timing_score, contrast)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (session_id, sequence) DO NOTHING`,
		f.ID, f.SessionID, f.Sequence, f.StoredPath, f.SHA256, f.Decoded, f.DecodeError,
		f.TransmissionID, f.FrameNumber, f.ChunkNumber, f.IsManifest, f.IsParity,
		f.BitErrorRate, f.FinderScore, f.TimingScore, f.Contrast)
	return err
}

// Failed returns the frames of a session that could not be read, which is the evidence an
// operator needs when a capture is going badly.
// Recent returns the newest captures of a session, decoded or not.
//
// Newest first, because the question a live page asks is "what is arriving now" — and the answer to that is the
// last few frames, not the first. Both outcomes are included: a page showing only what decoded would look
// healthy while a camera drifted out of focus, and one showing only failures would look broken during a perfect
// transfer.
func (r *Frames) Recent(ctx context.Context, sessionID uuid.UUID, limit int) ([]CapturedFrame, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, session_id, sequence, stored_path, sha256, decoded, decode_error,
		       transmission_id, frame_number, chunk_number, is_manifest, is_parity,
		       bit_error_rate, finder_score, timing_score, contrast, captured_at
		FROM captured_frames WHERE session_id = $1
		ORDER BY sequence DESC LIMIT $2`, sessionID, page(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CapturedFrame
	for rows.Next() {
		var f CapturedFrame
		if err := rows.Scan(&f.ID, &f.SessionID, &f.Sequence, &f.StoredPath, &f.SHA256,
			&f.Decoded, &f.DecodeError, &f.TransmissionID, &f.FrameNumber, &f.ChunkNumber,
			&f.IsManifest, &f.IsParity, &f.BitErrorRate, &f.FinderScore, &f.TimingScore,
			&f.Contrast, &f.CapturedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *Frames) Failed(ctx context.Context, sessionID uuid.UUID, limit int) ([]CapturedFrame, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, session_id, sequence, stored_path, sha256, decoded, decode_error,
		       transmission_id, frame_number, chunk_number, is_manifest, is_parity,
		       bit_error_rate, finder_score, timing_score, contrast, captured_at
		FROM captured_frames WHERE session_id = $1 AND NOT decoded
		-- Newest first, and that is the whole point of the page this feeds. "Why is my camera not reading
		-- this" is a question about the last few seconds, but ordering ascending under a limit answered it with
		-- the first failures of the session: once more had accumulated than the limit, the page froze on
		-- ancient history. Observed 3,861 failures deep, still showing frames 1 to 24 from twenty minutes
		-- earlier while an operator moved the camera and watched nothing change.
		ORDER BY sequence DESC LIMIT $2`, sessionID, page(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CapturedFrame
	for rows.Next() {
		var f CapturedFrame
		if err := rows.Scan(&f.ID, &f.SessionID, &f.Sequence, &f.StoredPath, &f.SHA256,
			&f.Decoded, &f.DecodeError, &f.TransmissionID, &f.FrameNumber, &f.ChunkNumber,
			&f.IsManifest, &f.IsParity, &f.BitErrorRate, &f.FinderScore, &f.TimingScore,
			&f.Contrast, &f.CapturedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Manifest is what a transmission declared about itself.
type Manifest struct {
	TransmissionID uuid.UUID `json:"transmission_id"`
	Filename       string    `json:"filename"`
	OriginalSize   int64     `json:"original_size"`
	OriginalSHA256 []byte    `json:"original_sha256"`
	CompressedSize int64     `json:"compressed_size"`
	ChunkCount     int       `json:"chunk_count"`
	ChunkSize      int       `json:"chunk_size"`
	CompressionID  int       `json:"compression_id"`
	FECID          int       `json:"fec_id"`
	FECDataShards  int       `json:"fec_data_shards"`
	FECParity      int       `json:"fec_parity"`
	ShardSize      int       `json:"shard_size"`
	CallbackURL    string    `json:"callback_url,omitempty"`
	ReceivedAt     time.Time `json:"received_at"`
}

// Manifests is the manifest repository.
type Manifests struct{ pool *db.Pool }

// Upsert records a manifest, replacing whatever a previous copy said.
//
// The manifest arrives repeatedly by design, so this has to be an upsert rather than an insert:
// a receiver that joined a transmission late learns the same thing from a later copy, and the
// tenth copy must not be an error.
func (r *Manifests) Upsert(ctx context.Context, m Manifest) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO manifests (transmission_id, filename, original_size, original_sha256,
			compressed_size, chunk_count, chunk_size, compression_id, fec_id, fec_data_shards,
			fec_parity, shard_size, callback_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (transmission_id) DO UPDATE SET
			filename = excluded.filename,
			original_size = excluded.original_size,
			original_sha256 = excluded.original_sha256,
			compressed_size = excluded.compressed_size,
			chunk_count = excluded.chunk_count,
			chunk_size = excluded.chunk_size,
			compression_id = excluded.compression_id,
			fec_id = excluded.fec_id,
			fec_data_shards = excluded.fec_data_shards,
			fec_parity = excluded.fec_parity,
			shard_size = excluded.shard_size,
			callback_url = excluded.callback_url,
			updated_at = now()`,
		m.TransmissionID, m.Filename, m.OriginalSize, m.OriginalSHA256, m.CompressedSize,
		m.ChunkCount, m.ChunkSize, m.CompressionID, m.FECID, m.FECDataShards, m.FECParity,
		m.ShardSize, m.CallbackURL)
	return err
}

// Get returns a transmission's manifest.
func (r *Manifests) Get(ctx context.Context, transmissionID uuid.UUID) (Manifest, error) {
	var m Manifest
	err := r.pool.QueryRow(ctx, `
		SELECT transmission_id, filename, original_size, original_sha256, compressed_size,
		       chunk_count, chunk_size, compression_id, fec_id, fec_data_shards, fec_parity,
		       shard_size, callback_url, received_at
		FROM manifests WHERE transmission_id = $1`, transmissionID).Scan(
		&m.TransmissionID, &m.Filename, &m.OriginalSize, &m.OriginalSHA256, &m.CompressedSize,
		&m.ChunkCount, &m.ChunkSize, &m.CompressionID, &m.FECID, &m.FECDataShards,
		&m.FECParity, &m.ShardSize, &m.CallbackURL, &m.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Manifest{}, fmt.Errorf("%w: no manifest for %s", ErrNotFound, transmissionID)
	}
	return m, err
}

// Pending returns transmissions whose manifest has arrived but whose file has not been merged.
// All returns every transmission this receiver knows about, newest first.
//
// Distinct from Pending, and the distinction was a real bug rather than a nicety: Pending excludes anything
// already merged and verified, which is exactly the set an operator most wants to look at. The receiver's
// list was built from Pending alone, so a file vanished from the interface at the moment it arrived
// successfully — three completed transfers showed as none. A receiver that cannot show what it received is
// not much use whatever it did with the bytes.
//
// Ordered by arrival, newest first, because the thing just received is the thing being looked for.
func (r *Manifests) All(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		SELECT transmission_id FROM manifests
		ORDER BY received_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *Manifests) Pending(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT m.transmission_id FROM manifests m
		LEFT JOIN merged_files f ON f.transmission_id = m.transmission_id AND f.verified
		WHERE f.id IS NULL
		ORDER BY m.received_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Chunk is a chunk that arrived intact.
type Chunk struct {
	ID             uuid.UUID `json:"id"`
	TransmissionID uuid.UUID `json:"transmission_id"`
	ChunkNumber    int       `json:"chunk_number"`
	IsParity       bool      `json:"is_parity"`
	BlockIndex     int       `json:"block_index"`
	SizeBytes      int       `json:"size_bytes"`
	CRC32          int64     `json:"crc32"`
	SHA256         []byte    `json:"sha256"`
	StoredPath     string    `json:"stored_path"`
	Recovered      bool      `json:"recovered"`
	ReceivedAt     time.Time `json:"received_at"`
}

// Chunks is the decoded-chunk repository.
type Chunks struct{ pool *db.Pool }

// Insert records a chunk, and reports whether it was new.
//
// A duplicate is the ordinary case rather than an error: an acknowledgement and a retransmission
// cross in flight routinely, so the same chunk arrives twice. The caller needs to know which it
// was, because a duplicate is acknowledged differently — the sender uses that to stop resending
// something the receiver already holds.
func (r *Chunks) Insert(ctx context.Context, c Chunk) (inserted bool, err error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO decoded_chunks (id, transmission_id, chunk_number, is_parity, block_index,
			size_bytes, crc32, sha256, stored_path, recovered)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (transmission_id, chunk_number) DO NOTHING`,
		c.ID, c.TransmissionID, c.ChunkNumber, c.IsParity, c.BlockIndex, c.SizeBytes,
		c.CRC32, c.SHA256, c.StoredPath, c.Recovered)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// List returns a transmission's chunks in order.
func (r *Chunks) List(ctx context.Context, transmissionID uuid.UUID) ([]Chunk, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, transmission_id, chunk_number, is_parity, block_index, size_bytes, crc32,
		       sha256, stored_path, recovered, received_at
		FROM decoded_chunks WHERE transmission_id = $1 ORDER BY chunk_number`, transmissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.TransmissionID, &c.ChunkNumber, &c.IsParity,
			&c.BlockIndex, &c.SizeBytes, &c.CRC32, &c.SHA256, &c.StoredPath, &c.Recovered,
			&c.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Have returns which source chunk numbers have arrived.
func (r *Chunks) Have(ctx context.Context, transmissionID uuid.UUID) (map[int]bool, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT chunk_number FROM decoded_chunks WHERE transmission_id = $1 AND NOT is_parity`,
		transmissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// Counts returns how many source chunks have arrived and how many of those came from parity.
func (r *Chunks) Counts(ctx context.Context, transmissionID uuid.UUID) (arrived, recovered int, err error) {
	err = r.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE NOT is_parity),
		       count(*) FILTER (WHERE NOT is_parity AND recovered)
		FROM decoded_chunks WHERE transmission_id = $1`, transmissionID).Scan(&arrived, &recovered)
	return arrived, recovered, err
}

// SetMissing replaces the outstanding-chunk list for a transmission.
//
// Kept as rows rather than recomputed on demand so the operator UI can show what is outstanding
// and for how long, which is the question being asked when a transfer stalls.
func (r *Chunks) SetMissing(ctx context.Context, transmissionID uuid.UUID, missing []int) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// Nothing outstanding is its own case rather than an empty list, because in SQL they are not the same
	// thing: `chunk_number <> ALL('{}')` is true, but an empty Go slice arrives as NULL and `<> ALL(NULL)`
	// is NULL — so the delete matched nothing and a completed transfer went on reporting chunks it was
	// waiting for.
	if len(missing) == 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM missing_chunks WHERE transmission_id = $1`, transmissionID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM missing_chunks WHERE transmission_id = $1 AND chunk_number <> ALL ($2)`,
		transmissionID, missing); err != nil {
		return err
	}
	for _, n := range missing {
		if _, err := tx.Exec(ctx, `
			INSERT INTO missing_chunks (transmission_id, chunk_number) VALUES ($1, $2)
			ON CONFLICT (transmission_id, chunk_number) DO UPDATE SET
				last_reported = now(), reports = missing_chunks.reports + 1`,
			transmissionID, n); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Missing returns the chunk numbers still outstanding.
func (r *Chunks) Missing(ctx context.Context, transmissionID uuid.UUID) ([]int, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT chunk_number FROM missing_chunks WHERE transmission_id = $1 ORDER BY chunk_number`,
		transmissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MergedFile is a reassembled file.
type MergedFile struct {
	ID             uuid.UUID  `json:"id"`
	TransmissionID uuid.UUID  `json:"transmission_id"`
	Filename       string     `json:"filename"`
	StoredPath     string     `json:"stored_path"`
	SizeBytes      int64      `json:"size_bytes"`
	SHA256         []byte     `json:"sha256"`
	Verified       bool       `json:"verified"`
	VerifyError    string     `json:"verify_error,omitempty"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Merged is the merged-file repository.
type Merged struct{ pool *db.Pool }

// Upsert records a merge outcome.
func (r *Merged) Upsert(ctx context.Context, f MergedFile) (MergedFile, error) {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	var out MergedFile
	err := r.pool.QueryRow(ctx, `
		INSERT INTO merged_files (id, transmission_id, filename, stored_path, size_bytes,
			sha256, verified, verify_error, verified_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $7 THEN now() ELSE NULL END)
		ON CONFLICT (transmission_id) DO UPDATE SET
			filename = excluded.filename, stored_path = excluded.stored_path,
			size_bytes = excluded.size_bytes, sha256 = excluded.sha256,
			verified = excluded.verified, verify_error = excluded.verify_error,
			verified_at = excluded.verified_at
		RETURNING id, transmission_id, filename, stored_path, size_bytes, sha256, verified,
		          verify_error, verified_at, created_at`,
		f.ID, f.TransmissionID, f.Filename, f.StoredPath, f.SizeBytes, f.SHA256,
		f.Verified, f.VerifyError).Scan(&out.ID, &out.TransmissionID, &out.Filename,
		&out.StoredPath, &out.SizeBytes, &out.SHA256, &out.Verified, &out.VerifyError,
		&out.VerifiedAt, &out.CreatedAt)
	return out, err
}

// Get returns a transmission's merged file.
func (r *Merged) Get(ctx context.Context, transmissionID uuid.UUID) (MergedFile, error) {
	var f MergedFile
	err := r.pool.QueryRow(ctx, `
		SELECT id, transmission_id, filename, stored_path, size_bytes, sha256, verified,
		       verify_error, verified_at, created_at
		FROM merged_files WHERE transmission_id = $1`, transmissionID).Scan(&f.ID,
		&f.TransmissionID, &f.Filename, &f.StoredPath, &f.SizeBytes, &f.SHA256, &f.Verified,
		&f.VerifyError, &f.VerifiedAt, &f.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MergedFile{}, fmt.Errorf("%w: nothing merged for %s", ErrNotFound, transmissionID)
	}
	return f, err
}

// Acks tracks the acknowledgement sequence.
type Acks struct{ pool *db.Pool }

// Next returns the next sequence number for a transmission, atomically.
//
// It is a row rather than a counter in memory because the sender uses the sequence to tell an old
// record from a new one. A receiver that restarted and began again at one would produce records
// the sender had already processed, and its newest reports would look like its oldest.
func (r *Acks) Next(ctx context.Context, transmissionID uuid.UUID) (uint64, error) {
	var sequence uint64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO ack_state (transmission_id, next_sequence) VALUES ($1, 2)
		ON CONFLICT (transmission_id) DO UPDATE SET
			next_sequence = ack_state.next_sequence + 1, updated_at = now()
		RETURNING next_sequence - 1`, transmissionID).Scan(&sequence)
	return sequence, err
}

// Callback is an outbound notification.
type Callback struct {
	ID             uuid.UUID       `json:"id"`
	TransmissionID *uuid.UUID      `json:"transmission_id,omitempty"`
	URL            string          `json:"url"`
	Event          string          `json:"event"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	LastStatus     *int            `json:"last_status,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	NextAttemptAt  time.Time       `json:"next_attempt_at"`
	DeliveredAt    *time.Time      `json:"delivered_at,omitempty"`
}

// Callbacks is the callback repository.
type Callbacks struct{ pool *db.Pool }

// Enqueue records a callback to deliver.
func (r *Callbacks) Enqueue(ctx context.Context, c Callback) (Callback, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 8
	}
	if len(c.Payload) == 0 {
		c.Payload = json.RawMessage("{}")
	}
	var out Callback
	err := r.pool.QueryRow(ctx, `
		INSERT INTO callbacks (id, transmission_id, url, event, payload, max_attempts)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, transmission_id, url, event, status, attempts, max_attempts, last_status,
		          last_error, payload, next_attempt_at, delivered_at`,
		c.ID, c.TransmissionID, c.URL, c.Event, c.Payload, c.MaxAttempts).Scan(&out.ID,
		&out.TransmissionID, &out.URL, &out.Event, &out.Status, &out.Attempts, &out.MaxAttempts,
		&out.LastStatus, &out.LastError, &out.Payload, &out.NextAttemptAt, &out.DeliveredAt)
	return out, err
}

// Due returns callbacks whose next attempt is now, claiming them so two deliverers cannot send
// the same notification twice.
func (r *Callbacks) Due(ctx context.Context, limit int) ([]Callback, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE callbacks SET attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM callbacks
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		RETURNING id, transmission_id, url, event, status, attempts, max_attempts, last_status,
		          last_error, payload, next_attempt_at, delivered_at`, page(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Callback
	for rows.Next() {
		var c Callback
		if err := rows.Scan(&c.ID, &c.TransmissionID, &c.URL, &c.Event, &c.Status, &c.Attempts,
			&c.MaxAttempts, &c.LastStatus, &c.LastError, &c.Payload, &c.NextAttemptAt,
			&c.DeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Delivered marks a callback as accepted.
func (r *Callbacks) Delivered(ctx context.Context, id uuid.UUID, status int) error {
	// The attempt is counted here as well as in Due.
	//
	// Due increments it when it claims a callback from the queue, which is the retrying path — but a merged
	// file is delivered directly as soon as it verifies, without going through the queue at all. That path
	// left the counter at zero, so a successful delivery displayed as "0 of 8 attempts", which reads as
	// though nothing had been tried. GREATEST rather than +1 so the queued path is not double-counted.
	_, err := r.pool.Exec(ctx, `
		UPDATE callbacks SET status = 'delivered', last_status = $2, last_error = '',
		                     attempts = GREATEST(attempts, 1),
		                     delivered_at = now(), updated_at = now()
		WHERE id = $1`, id, status)
	return err
}

// Retry records a failed attempt and schedules the next, or gives up.
func (r *Callbacks) Retry(ctx context.Context, id uuid.UUID, status *int, reason string, delay time.Duration) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE callbacks SET
			status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
			last_status = $2, last_error = $3,
			next_attempt_at = now() + $4::interval, updated_at = now()
		WHERE id = $1`, id, status, reason, delay.String())
	return err
}

// Get returns one callback.
// ForTransmission returns the delivery attempts for one transmission, newest first.
//
// An operator looking at a received file wants to know where it was sent and whether it got there, and the
// receiver is the only side that knows: the URL crossed the optical channel in the manifest and the delivery
// was made from here. Reporting the attempts rather than a boolean matters when it went wrong — a refused
// host, a 500, a timeout and a delivery that simply has not been tried yet are four different problems.
func (r *Callbacks) ForTransmission(ctx context.Context, transmission uuid.UUID) ([]Callback, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, transmission_id, url, event, status, attempts, max_attempts, last_status,
		       last_error, payload, next_attempt_at, delivered_at
		FROM callbacks WHERE transmission_id = $1
		ORDER BY next_attempt_at DESC`, transmission)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Callback
	for rows.Next() {
		var c Callback
		if err := rows.Scan(&c.ID, &c.TransmissionID, &c.URL, &c.Event, &c.Status, &c.Attempts,
			&c.MaxAttempts, &c.LastStatus, &c.LastError, &c.Payload, &c.NextAttemptAt,
			&c.DeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Callbacks) Get(ctx context.Context, id uuid.UUID) (Callback, error) {
	var c Callback
	err := r.pool.QueryRow(ctx, `
		SELECT id, transmission_id, url, event, status, attempts, max_attempts, last_status,
		       last_error, payload, next_attempt_at, delivered_at
		FROM callbacks WHERE id = $1`, id).Scan(&c.ID, &c.TransmissionID, &c.URL, &c.Event,
		&c.Status, &c.Attempts, &c.MaxAttempts, &c.LastStatus, &c.LastError, &c.Payload,
		&c.NextAttemptAt, &c.DeliveredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Callback{}, fmt.Errorf("%w: callback %s", ErrNotFound, id)
	}
	return c, err
}

// Stats records measurements.
type Stats struct{ pool *db.Pool }

// Record appends a measurement.
func (r *Stats) Record(ctx context.Context, metric string, value float64, transmissionID *uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO statistics (transmission_id, metric, value) VALUES ($1, $2, $3)`,
		transmissionID, metric, value)
	return err
}

// DecoderKey is a decryption key the operator has loaded, alongside the configured one, so this
// receiver can decode a transmission encrypted with a key that arrived out of band.
type DecoderKey struct {
	ID        int64     `json:"id"`
	Key       []byte    `json:"-"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// DecoderKeys is the decryption-keyring repository.
type DecoderKeys struct{ pool *db.Pool }

// List returns every loaded key, oldest first — the order they were added, which is the order an
// operator would expect to see them in.
func (r *DecoderKeys) List(ctx context.Context) ([]DecoderKey, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, key, label, created_at FROM decoder_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DecoderKey
	for rows.Next() {
		var k DecoderKey
		if err := rows.Scan(&k.ID, &k.Key, &k.Label, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Add loads a new key into the ring.
func (r *DecoderKeys) Add(ctx context.Context, key []byte, label string) (DecoderKey, error) {
	var k DecoderKey
	err := r.pool.QueryRow(ctx, `
		INSERT INTO decoder_keys (key, label) VALUES ($1, $2)
		RETURNING id, key, label, created_at`,
		key, label).Scan(&k.ID, &k.Key, &k.Label, &k.CreatedAt)
	return k, err
}

// Delete removes a key from the ring.
func (r *DecoderKeys) Delete(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM decoder_keys WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: decoder key %d", ErrNotFound, id)
	}
	return nil
}

// Transmissions is the whole-transmission repository. Nothing else in this file operates
// across every table a transmission touches at once; deleting one does, so it gets a
// repository of its own rather than living on one of the narrower ones.
type Transmissions struct{ pool *db.Pool }

// deletedByTransmission lists every table keyed by a bare transmission_id. Order does not
// matter for correctness — the deletes run inside one transaction, so it is all seven rows
// gone or none of them — but manifests is listed last anyway, as the table a reader would
// check first if they were only glancing at this list.
//
// captured_frames is not here. It is keyed by session_id, cascades from capture_sessions on
// delete, and is the capture audit log rather than the file itself — a transmission's
// deletion must not take a session's history of what the camera saw along with it.
var deletedByTransmission = []string{
	"decoded_chunks", "missing_chunks", "merged_files", "ack_state", "callbacks", "statistics", "manifests",
}

// existsQuery reports whether any of the seven tables has a row for a transmission. Built once
// from deletedByTransmission rather than hand-written, so a table added to the deletion list is
// automatically part of the existence check too.
var existsQuery = buildExistsQuery(deletedByTransmission)

func buildExistsQuery(tables []string) string {
	clauses := make([]string, len(tables))
	for i, table := range tables {
		clauses[i] = fmt.Sprintf("EXISTS (SELECT 1 FROM %s WHERE transmission_id = $1)", table)
	}
	return "SELECT " + strings.Join(clauses, " OR ")
}

// rowScanner is the common surface of *db.Pool and pgx.Tx that existsAnywhere needs, so the
// same query backs both a standalone check and one made inside DeleteCascade's transaction.
type rowScanner interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// existsAnywhere reports whether a transmission has a row in any of the seven tables.
//
// This is deliberately not "does a manifest exist": chunks routinely arrive before the manifest
// does and are stored and counted while the receiver waits for it (see pipeline.Receiver's
// handling of early chunks), so decoded_chunks, missing_chunks, or ack_state can be the only
// tables that know a transmission exists at all. Checking manifests alone would tell a caller
// "no such transmission" about one that is mid-transfer and has real rows and real objects on
// disk — which is exactly the bug this function exists to avoid.
func existsAnywhere(ctx context.Context, q rowScanner, id uuid.UUID) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, existsQuery, id).Scan(&exists)
	return exists, err
}

// Exists reports whether this receiver knows anything at all about a transmission.
//
// It exists so a caller — the delete handler, specifically — can decide whether a transmission
// is known *before* doing anything irreversible, such as deleting the objects the transmission's
// rows point at. Deciding that only inside DeleteCascade's own transaction would still leave a
// caller that acts on the id first (or in parallel) with the wrong answer, and the delete
// handler must never destroy an object on a path that ends in 404.
func (r *Transmissions) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	return existsAnywhere(ctx, r.pool, id)
}

// DeleteCascade removes every row a transmission owns, across the seven tables that carry its
// id, in one transaction.
//
// It is explicit rather than a foreign-key cascade because there is no foreign key to lean on:
// unlike capture_sessions and captured_frames, every one of these tables carries a bare
// transmission_id with no constraint at all. Nothing but this method's own consistency says a
// manifest row and a decoded_chunks row for the same id belong together, so nothing but this
// method can take them all out atomically.
//
// Existence is checked with the same any-of-seven-tables query Exists uses, inside this
// transaction, so a caller that skipped its own check (or lost a race with one) still gets a
// correct ErrNotFound rather than silently deleting nothing.
func (r *Transmissions) DeleteCascade(ctx context.Context, id uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	exists, err := existsAnywhere(ctx, tx, id)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: transmission %s", ErrNotFound, id)
	}

	for _, table := range deletedByTransmission {
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE transmission_id = $1`, table), id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func page(limit int) int {
	if limit <= 0 || limit > 1000 {
		return 100
	}
	return limit
}
