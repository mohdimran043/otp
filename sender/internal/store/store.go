// Package store is the sender's data access layer: one type per table, with the queries the
// pipeline and the API need and nothing else.
//
// It is hand-written SQL rather than generated or reflected, and the reason is that the
// interesting queries here are not row-at-a-time CRUD. Claiming work, finding the next
// unacknowledged chunk in priority order, counting a transmission's progress — those are the
// ones that matter, and they are clearer written out than assembled by a builder.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opticaltransport/otp/sender/internal/db"
)

// ErrNotFound means no row matched.
var ErrNotFound = errors.New("store: not found")

// Store holds every repository, so a caller takes one dependency rather than eight.
type Store struct {
	pool *db.Pool

	Files         *Files
	Transmissions *Transmissions
	Chunks        *Chunks
	Frames        *Frames
	Sessions      *Sessions
	Callbacks     *Callbacks
	Stats         *Stats
	SenderKeys    *SenderKeys

	// DisplaySettings is what an operator changed through the UI, kept so it survives a restart.
	DisplaySettings *DisplaySettings
}

// New returns a store over a connection pool.
func New(pool *db.Pool) *Store {
	return &Store{
		pool:          pool,
		Files:         &Files{pool: pool},
		Transmissions: &Transmissions{pool: pool},
		Chunks:        &Chunks{pool: pool},
		Frames:        &Frames{pool: pool},
		Sessions:      &Sessions{pool: pool},
		Callbacks:     &Callbacks{pool: pool},
		Stats:         &Stats{pool: pool},
		SenderKeys:    &SenderKeys{pool: pool},

		DisplaySettings: &DisplaySettings{pool: pool},
	}
}

// Pool exposes the underlying pool for the health check and for packages that own their own
// tables, like the job engine.
func (s *Store) Pool() *db.Pool { return s.pool }

// File is an uploaded file.
type File struct {
	ID          uuid.UUID  `json:"id"`
	Filename    string     `json:"filename"`
	StoredPath  string     `json:"stored_path"`
	SizeBytes   int64      `json:"size_bytes"`
	SHA256      []byte     `json:"sha256"`
	ContentType string     `json:"content_type"`
	UploadedBy  *uuid.UUID `json:"uploaded_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Files is the uploaded-file repository.
type Files struct{ pool *db.Pool }

const fileColumns = `id, filename, stored_path, size_bytes, sha256, content_type,
	uploaded_by, created_at, updated_at`

func scanFile(row pgx.Row) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.Filename, &f.StoredPath, &f.SizeBytes, &f.SHA256,
		&f.ContentType, &f.UploadedBy, &f.CreatedAt, &f.UpdatedAt)
	return f, err
}

// Create records an uploaded file.
func (r *Files) Create(ctx context.Context, f File) (File, error) {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	return scanFile(r.pool.QueryRow(ctx, `
		INSERT INTO files (id, filename, stored_path, size_bytes, sha256, content_type, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+fileColumns,
		f.ID, f.Filename, f.StoredPath, f.SizeBytes, f.SHA256, f.ContentType, f.UploadedBy))
}

// Get returns one file.
func (r *Files) Get(ctx context.Context, id uuid.UUID) (File, error) {
	f, err := scanFile(r.pool.QueryRow(ctx, `SELECT `+fileColumns+` FROM files WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, fmt.Errorf("%w: file %s", ErrNotFound, id)
	}
	return f, err
}

// List returns files, newest first.
func (r *Files) List(ctx context.Context, limit, offset int) ([]File, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+fileColumns+` FROM files ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
		page(limit), max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Delete removes a file and, by the cascade, everything derived from it.
func (r *Files) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM files WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: file %s", ErrNotFound, id)
	}
	return nil
}

// TransmissionStatus is where a transmission is in its life.
type TransmissionStatus string

// Transmission statuses.
const (
	TxPending      TransmissionStatus = "pending"
	TxPreparing    TransmissionStatus = "preparing"
	TxReady        TransmissionStatus = "ready"
	TxTransmitting TransmissionStatus = "transmitting"
	TxPaused       TransmissionStatus = "paused"
	TxCompleted    TransmissionStatus = "completed"
	TxFailed       TransmissionStatus = "failed"
	TxCancelled    TransmissionStatus = "cancelled"
)

// Transmission is one file being sent, with the profile it is being sent under.
//
// The profile fields are copied onto the row rather than only referenced, because a
// transmission must stay self-describing. Profiles are edited and deleted while
// transmissions from them are still running or still in the history, and a record that
// said only "encoding profile 4" would become unreadable the moment profile 4 changed.
type Transmission struct {
	ID     uuid.UUID          `json:"id"`
	FileID uuid.UUID          `json:"file_id"`
	Status TransmissionStatus `json:"status"`

	Priority string `json:"priority"`

	CompressionProfile *uuid.UUID `json:"compression_profile,omitempty"`
	EncodingProfile    *uuid.UUID `json:"encoding_profile,omitempty"`

	Encoder          string `json:"encoder"`
	BitDepth         int    `json:"bit_depth"`
	Compression      string `json:"compression"`
	CompressionLevel int    `json:"compression_level"`
	FECCodec         string `json:"fec_codec"`
	FECDataShards    int    `json:"fec_data_shards"`
	FECParityShards  int    `json:"fec_parity_shards"`
	GridWidth        int    `json:"grid_width"`
	GridHeight       int    `json:"grid_height"`
	CellPixels       int    `json:"cell_pixels"`
	QuietZone        int    `json:"quiet_zone"`
	Encrypted        bool   `json:"encrypted"`
	EncryptionID     int    `json:"encryption_id"`
	EncryptionKey    []byte `json:"-"`

	// CallbackURL is where the receiver delivers the merged file. It is recorded here because it
	// travels in the manifest, and the manifest is built from this row.
	CallbackURL string `json:"callback_url,omitempty"`

	OriginalSize   int64 `json:"original_size"`
	CompressedSize int64 `json:"compressed_size"`
	ChunkSize      int   `json:"chunk_size"`
	ChunkCount     int   `json:"chunk_count"`
	FrameCount     int   `json:"frame_count"`

	AckedChunks   int `json:"acked_chunks"`
	Retransmits   int `json:"retransmits"`
	DroppedFrames int `json:"dropped_frames"`

	Error       string     `json:"error,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Progress is the fraction of chunks acknowledged, in 0..1.
func (t Transmission) Progress() float64 {
	if t.ChunkCount == 0 {
		return 0
	}
	return float64(t.AckedChunks) / float64(t.ChunkCount)
}

// Transmissions is the transmission repository.
type Transmissions struct{ pool *db.Pool }

const txColumns = `id, file_id, status, priority, compression_profile, encoding_profile,
	encoder, bit_depth, compression, compression_level, fec_codec, fec_data_shards,
	fec_parity_shards, grid_width, grid_height, cell_pixels, quiet_zone, encrypted,
	encryption_id, encryption_key,
	original_size, compressed_size, chunk_size, chunk_count, frame_count,
	acked_chunks, retransmits, dropped_frames, callback_url, error, started_at, completed_at,
	created_at, updated_at`

func scanTransmission(row pgx.Row) (Transmission, error) {
	var t Transmission
	err := row.Scan(&t.ID, &t.FileID, &t.Status, &t.Priority, &t.CompressionProfile,
		&t.EncodingProfile, &t.Encoder, &t.BitDepth, &t.Compression, &t.CompressionLevel,
		&t.FECCodec, &t.FECDataShards, &t.FECParityShards, &t.GridWidth, &t.GridHeight,
		&t.CellPixels, &t.QuietZone, &t.Encrypted, &t.EncryptionID, &t.EncryptionKey,
		&t.OriginalSize, &t.CompressedSize,
		&t.ChunkSize, &t.ChunkCount, &t.FrameCount, &t.AckedChunks, &t.Retransmits,
		&t.DroppedFrames, &t.CallbackURL, &t.Error, &t.StartedAt, &t.CompletedAt,
		&t.CreatedAt, &t.UpdatedAt)
	return t, err
}

// Create records a transmission.
func (r *Transmissions) Create(ctx context.Context, t Transmission) (Transmission, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.Status == "" {
		t.Status = TxPending
	}
	if t.Priority == "" {
		t.Priority = "normal"
	}
	return scanTransmission(r.pool.QueryRow(ctx, `
		INSERT INTO transmissions (id, file_id, status, priority, compression_profile,
			encoding_profile, encoder, bit_depth, compression, compression_level, fec_codec,
			fec_data_shards, fec_parity_shards, grid_width, grid_height, cell_pixels,
			quiet_zone, encrypted, encryption_id, encryption_key, original_size, callback_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		RETURNING `+txColumns,
		t.ID, t.FileID, t.Status, t.Priority, t.CompressionProfile, t.EncodingProfile,
		t.Encoder, t.BitDepth, t.Compression, t.CompressionLevel, t.FECCodec,
		t.FECDataShards, t.FECParityShards, t.GridWidth, t.GridHeight, t.CellPixels,
		t.QuietZone, t.Encrypted, t.EncryptionID, t.EncryptionKey, t.OriginalSize, t.CallbackURL))
}

// Get returns one transmission.
func (r *Transmissions) Get(ctx context.Context, id uuid.UUID) (Transmission, error) {
	t, err := scanTransmission(r.pool.QueryRow(ctx,
		`SELECT `+txColumns+` FROM transmissions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transmission{}, fmt.Errorf("%w: transmission %s", ErrNotFound, id)
	}
	return t, err
}

// List returns transmissions, newest first, optionally filtered by status.
func (r *Transmissions) List(ctx context.Context, statuses []TransmissionStatus, limit, offset int) ([]Transmission, error) {
	query := `SELECT ` + txColumns + ` FROM transmissions`
	args := []any{}
	if len(statuses) > 0 {
		values := make([]string, len(statuses))
		for i, s := range statuses {
			values[i] = string(s)
		}
		args = append(args, values)
		query += ` WHERE status = ANY ($1)`
	}
	args = append(args, page(limit), max(offset, 0))
	query += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Transmission
	for rows.Next() {
		t, err := scanTransmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetStatus moves a transmission to a new status, recording the timestamps that go with it.
func (r *Transmissions) SetStatus(ctx context.Context, id uuid.UUID, status TransmissionStatus, reason string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE transmissions SET
			status = $2,
			error = $3,
			started_at = CASE WHEN $2 = 'transmitting' AND started_at IS NULL THEN now() ELSE started_at END,
			completed_at = CASE WHEN $2 IN ('completed', 'failed', 'cancelled') THEN now() ELSE completed_at END,
			updated_at = now()
		WHERE id = $1`, id, status, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: transmission %s", ErrNotFound, id)
	}
	return nil
}

// SetSizes records what compression and chunking produced.
func (r *Transmissions) SetSizes(ctx context.Context, id uuid.UUID, compressed int64, chunkSize, chunkCount int) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE transmissions SET compressed_size = $2, chunk_size = $3, chunk_count = $4,
		                         updated_at = now()
		WHERE id = $1`, id, compressed, chunkSize, chunkCount)
	return err
}

// SetFrameCount records how many frames were rendered.
func (r *Transmissions) SetFrameCount(ctx context.Context, id uuid.UUID, frames int) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE transmissions SET frame_count = $2, updated_at = now() WHERE id = $1`, id, frames)
	return err
}

// RecountAcked recomputes the acknowledged-chunk total from the chunks themselves.
//
// Deriving it rather than incrementing a counter is what makes it correct under retransmission.
// Acknowledgements arrive more than once for the same chunk — an acknowledgement and a retransmission
// cross in flight routinely — and a counter incremented per acknowledgement would climb past the chunk
// count and report a transmission as more than finished.
//
// Only source chunks are counted. Parity shards are acknowledged too, and counting them would do the same
// thing by a different route: chunk_count is the number of source chunks, so including parity made the
// figure exceed it and progress read as more than complete. Parity is scaffolding — it never appears in
// the file — so it does not belong in a measure of how much of the file has arrived.
func (r *Transmissions) RecountAcked(ctx context.Context, id uuid.UUID) (int, error) {
	var acked int
	err := r.pool.QueryRow(ctx, `
		UPDATE transmissions SET
			acked_chunks = (
				SELECT count(*) FROM chunks
				WHERE transmission_id = $1 AND acked AND NOT is_parity
			),
			updated_at = now()
		WHERE id = $1
		RETURNING acked_chunks`, id).Scan(&acked)
	return acked, err
}

// AddCounters increments the running totals the operator UI displays.
func (r *Transmissions) AddCounters(ctx context.Context, id uuid.UUID, retransmits, dropped int) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE transmissions SET retransmits = retransmits + $2, dropped_frames = dropped_frames + $3,
		                         updated_at = now()
		WHERE id = $1`, id, retransmits, dropped)
	return err
}

// Chunk is one unit of the compressed stream, as it will be carried by one frame.
type Chunk struct {
	ID             uuid.UUID  `json:"id"`
	TransmissionID uuid.UUID  `json:"transmission_id"`
	ESI            int        `json:"esi"`
	BlockIndex     int        `json:"block_index"`
	IsParity       bool       `json:"is_parity"`
	SizeBytes      int        `json:"size_bytes"`
	CRC32          int64      `json:"crc32"`
	SHA256         []byte     `json:"sha256"`
	StoredPath     string     `json:"stored_path"`
	Acked          bool       `json:"acked"`
	AckedAt        *time.Time `json:"acked_at,omitempty"`
	RetryCount     int        `json:"retry_count"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Chunks is the chunk repository.
type Chunks struct{ pool *db.Pool }

const chunkColumns = `id, transmission_id, esi, block_index, is_parity, size_bytes,
	crc32, sha256, stored_path, acked, acked_at, retry_count, created_at`

func scanChunk(row pgx.Row) (Chunk, error) {
	var c Chunk
	err := row.Scan(&c.ID, &c.TransmissionID, &c.ESI, &c.BlockIndex, &c.IsParity,
		&c.SizeBytes, &c.CRC32, &c.SHA256, &c.StoredPath, &c.Acked, &c.AckedAt,
		&c.RetryCount, &c.CreatedAt)
	return c, err
}

// InsertMany writes a batch of chunks in one round trip.
//
// A transmission has thousands of chunks, and inserting them one at a time would spend most
// of the chunking stage waiting on network latency rather than doing work. CopyFrom is the
// bulk path pgx offers over the Postgres copy protocol.
func (r *Chunks) InsertMany(ctx context.Context, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	rows := make([][]any, len(chunks))
	for i, c := range chunks {
		if c.ID == uuid.Nil {
			c.ID = uuid.New()
		}
		rows[i] = []any{c.ID, c.TransmissionID, c.ESI, c.BlockIndex, c.IsParity,
			c.SizeBytes, c.CRC32, c.SHA256, c.StoredPath}
	}
	_, err := r.pool.CopyFrom(ctx,
		pgx.Identifier{"chunks"},
		[]string{"id", "transmission_id", "esi", "block_index", "is_parity", "size_bytes",
			"crc32", "sha256", "stored_path"},
		pgx.CopyFromRows(rows))
	return err
}

// List returns a transmission's chunks in identifier order.
func (r *Chunks) List(ctx context.Context, transmissionID uuid.UUID) ([]Chunk, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+chunkColumns+` FROM chunks WHERE transmission_id = $1 ORDER BY esi`, transmissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Chunk
	for rows.Next() {
		c, err := scanChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Pending returns the unacknowledged chunks of a transmission, oldest first, up to limit.
// It is the scheduler's window query.
func (r *Chunks) Pending(ctx context.Context, transmissionID uuid.UUID, limit int) ([]Chunk, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+chunkColumns+` FROM chunks
		WHERE transmission_id = $1 AND NOT acked
		ORDER BY esi LIMIT $2`, transmissionID, page(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Chunk
	for rows.Next() {
		c, err := scanChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteFor removes every chunk of a transmission.
//
// It exists so the chunking stage can be re-run. A job is retried on any transient failure and
// reclaimed if its worker dies, so every stage has to be safe to run twice — and a stage that
// inserts rows is only safe to run twice if it first removes what its previous attempt wrote.
// The alternative, an upsert, would leave behind chunks from a longer previous run whenever the
// stream got shorter.
func (r *Chunks) DeleteFor(ctx context.Context, transmissionID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM chunks WHERE transmission_id = $1`, transmissionID)
	return err
}

// DeleteParityFor removes only the parity shards, which is what the error-coding stage replaces.
func (r *Chunks) DeleteParityFor(ctx context.Context, transmissionID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM chunks WHERE transmission_id = $1 AND is_parity`, transmissionID)
	return err
}

// MarkAcked records that a chunk arrived. It is idempotent, because an acknowledgement for a
// chunk already acknowledged is routine rather than exceptional.
func (r *Chunks) MarkAcked(ctx context.Context, transmissionID uuid.UUID, esi int) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE chunks SET acked = true, acked_at = coalesce(acked_at, now())
		WHERE transmission_id = $1 AND esi = $2`, transmissionID, esi)
	return err
}

// AddRetry counts another attempt at a chunk.
func (r *Chunks) AddRetry(ctx context.Context, transmissionID uuid.UUID, esi int) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE chunks SET retry_count = retry_count + 1
		WHERE transmission_id = $1 AND esi = $2`, transmissionID, esi)
	return err
}

// Frame is one rendered image.
type Frame struct {
	ID             uuid.UUID  `json:"id"`
	TransmissionID uuid.UUID  `json:"transmission_id"`
	ChunkID        *uuid.UUID `json:"chunk_id,omitempty"`
	FrameNumber    int        `json:"frame_number"`
	IsManifest     bool       `json:"is_manifest"`
	Flags          int        `json:"flags"`
	WidthPx        int        `json:"width_px"`
	HeightPx       int        `json:"height_px"`
	PayloadBytes   int        `json:"payload_bytes"`
	StoredPath     string     `json:"stored_path"`
	SHA256         []byte     `json:"sha256"`
	DisplayedCount int        `json:"displayed_count"`
	LastDisplayed  *time.Time `json:"last_displayed,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Frames is the rendered-frame repository.
type Frames struct{ pool *db.Pool }

const frameColumns = `id, transmission_id, chunk_id, frame_number, is_manifest, flags,
	width_px, height_px, payload_bytes, stored_path, sha256, displayed_count,
	last_displayed, created_at`

func scanFrame(row pgx.Row) (Frame, error) {
	var f Frame
	err := row.Scan(&f.ID, &f.TransmissionID, &f.ChunkID, &f.FrameNumber, &f.IsManifest,
		&f.Flags, &f.WidthPx, &f.HeightPx, &f.PayloadBytes, &f.StoredPath, &f.SHA256,
		&f.DisplayedCount, &f.LastDisplayed, &f.CreatedAt)
	return f, err
}

// InsertMany writes a batch of frames.
func (r *Frames) InsertMany(ctx context.Context, frames []Frame) error {
	if len(frames) == 0 {
		return nil
	}
	rows := make([][]any, len(frames))
	for i, f := range frames {
		if f.ID == uuid.Nil {
			f.ID = uuid.New()
		}
		rows[i] = []any{f.ID, f.TransmissionID, f.ChunkID, f.FrameNumber, f.IsManifest,
			f.Flags, f.WidthPx, f.HeightPx, f.PayloadBytes, f.StoredPath, f.SHA256}
	}
	_, err := r.pool.CopyFrom(ctx,
		pgx.Identifier{"encoded_frames"},
		[]string{"id", "transmission_id", "chunk_id", "frame_number", "is_manifest", "flags",
			"width_px", "height_px", "payload_bytes", "stored_path", "sha256"},
		pgx.CopyFromRows(rows))
	return err
}

// DeleteFor removes every frame of a transmission, so the rendering stage can be re-run.
func (r *Frames) DeleteFor(ctx context.Context, transmissionID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM encoded_frames WHERE transmission_id = $1`, transmissionID)
	return err
}

// List returns a transmission's frames in display order.
func (r *Frames) List(ctx context.Context, transmissionID uuid.UUID) ([]Frame, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+frameColumns+` FROM encoded_frames WHERE transmission_id = $1 ORDER BY frame_number`,
		transmissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Frame
	for rows.Next() {
		f, err := scanFrame(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Status returns just a transmission's status.
//
// A single column rather than the whole row, because the display loop reads this on every frame: it is how
// an operator's decision to stop a transfer reaches a goroutine that is otherwise only listening to its
// own ticker. Making it a database fact rather than an in-process signal is deliberate — the API handler
// and the display loop need not know about each other, and a stop survives whichever of them restarts.
func (r *Transmissions) Status(ctx context.Context, id uuid.UUID) (TransmissionStatus, error) {
	var status TransmissionStatus
	err := r.pool.QueryRow(ctx, `SELECT status FROM transmissions WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: transmission %s", ErrNotFound, id)
	}
	return status, err
}

// CountActive is how many transmissions are being prepared or displayed right now.
//
// It exists to answer one question: may the frame geometry change? The grid and cell size are written into
// every frame header and the chunk size is derived from them, so a transfer that is mid-preparation or
// mid-display has already committed to a shape. Paused counts as active — a paused transfer is one that
// will resume against the geometry its manifest declared.
func (r *Transmissions) CountActive(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM transmissions
		WHERE status = ANY($1)`,
		[]string{string(TxPending), string(TxPreparing), string(TxReady),
			string(TxTransmitting), string(TxPaused)}).Scan(&count)
	return count, err
}

// ListForRetention returns the identifiers of transmissions old enough, and never completed,
// to be swept by the retention job.
//
// "Never completed" is the whole test, not "failed" or "cancelled" specifically: a transfer
// stuck pending, preparing, transmitting, or paused has abandoned just as much storage as one
// that failed outright, and the sender has no other mechanism that will ever revisit it.
// Completed is the one status that means an operator got what they came for, so it is the one
// status this sweep must never touch.
func (r *Transmissions) ListForRetention(ctx context.Context, olderThan time.Time) ([]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id FROM transmissions WHERE created_at < $1 AND status <> $2`,
		olderThan, TxCompleted)
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

// GetByNumber returns one frame of a transmission, addressed the way an auditor thinks of it.
//
// By frame number rather than by row id, because that is the number written into the frame's own header
// band and reported by the receiver when a decode fails — so an operator holding a complaint about
// "frame 214" can ask for frame 214 rather than having to find its identifier first.
func (r *Frames) GetByNumber(ctx context.Context, transmissionID uuid.UUID, number int) (Frame, error) {
	f, err := scanFrame(r.pool.QueryRow(ctx,
		`SELECT `+frameColumns+` FROM encoded_frames WHERE transmission_id = $1 AND frame_number = $2`,
		transmissionID, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return Frame{}, fmt.Errorf("%w: transmission %s has no frame %d", ErrNotFound, transmissionID, number)
	}
	return f, err
}

// ForChunk returns the frame carrying a chunk, which is what a retransmission needs.
func (r *Frames) ForChunk(ctx context.Context, chunkID uuid.UUID) (Frame, error) {
	f, err := scanFrame(r.pool.QueryRow(ctx,
		`SELECT `+frameColumns+` FROM encoded_frames WHERE chunk_id = $1 ORDER BY frame_number LIMIT 1`,
		chunkID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Frame{}, fmt.Errorf("%w: no frame carries chunk %s", ErrNotFound, chunkID)
	}
	return f, err
}

// MarkDisplayed counts a frame as shown.
func (r *Frames) MarkDisplayed(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE encoded_frames SET displayed_count = displayed_count + 1, last_displayed = now()
		WHERE id = $1`, id)
	return err
}

// DisplaySession is one run of the display for a transmission.
//
// It is a row rather than in-memory state because it records the settings the frames were shown
// under — frame rate, brightness, gamma, window size — and those are reloadable. An operator who
// turned the rate down halfway through and then asked why the transfer took as long as it did needs
// the session to say what was in force, not what is in force now.
type DisplaySession struct {
	ID             uuid.UUID  `json:"id"`
	TransmissionID uuid.UUID  `json:"transmission_id"`
	Status         string     `json:"status"`
	Sink           string     `json:"sink"`
	FPS            float64    `json:"fps"`
	Brightness     float64    `json:"brightness"`
	Gamma          float64    `json:"gamma"`
	WindowSize     int        `json:"window_size"`
	FramesShown    int64      `json:"frames_shown"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// Sessions is the display-session repository.
type Sessions struct{ pool *db.Pool }

// Open records the start of a display run.
func (r *Sessions) Open(ctx context.Context, s DisplaySession) (DisplaySession, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Gamma <= 0 {
		s.Gamma = 1
	}
	var out DisplaySession
	err := r.pool.QueryRow(ctx, `
		INSERT INTO display_sessions (id, transmission_id, status, sink, fps, brightness, gamma,
			window_size)
		VALUES ($1, $2, 'running', $3, $4, $5, $6, $7)
		RETURNING id, transmission_id, status, sink, fps, brightness, gamma, window_size,
		          frames_shown, started_at, ended_at, error`,
		s.ID, s.TransmissionID, s.Sink, s.FPS, s.Brightness, s.Gamma, s.WindowSize).Scan(
		&out.ID, &out.TransmissionID, &out.Status, &out.Sink, &out.FPS, &out.Brightness,
		&out.Gamma, &out.WindowSize, &out.FramesShown, &out.StartedAt, &out.EndedAt, &out.Error)
	return out, err
}

// Close ends a display run.
func (r *Sessions) Close(ctx context.Context, id uuid.UUID, status, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE display_sessions SET status = $2, error = $3, ended_at = now(), updated_at = now()
		WHERE id = $1`, id, status, reason)
	return err
}

// CountShown records frames displayed.
func (r *Sessions) CountShown(ctx context.Context, id uuid.UUID, frames int64) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE display_sessions SET frames_shown = frames_shown + $2, updated_at = now() WHERE id = $1`,
		id, frames)
	return err
}

// Get returns one display session.
func (r *Sessions) Get(ctx context.Context, id uuid.UUID) (DisplaySession, error) {
	var s DisplaySession
	err := r.pool.QueryRow(ctx, `
		SELECT id, transmission_id, status, sink, fps, brightness, gamma, window_size,
		       frames_shown, started_at, ended_at, error
		FROM display_sessions WHERE id = $1`, id).Scan(&s.ID, &s.TransmissionID, &s.Status,
		&s.Sink, &s.FPS, &s.Brightness, &s.Gamma, &s.WindowSize, &s.FramesShown,
		&s.StartedAt, &s.EndedAt, &s.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return DisplaySession{}, fmt.Errorf("%w: display session %s", ErrNotFound, id)
	}
	return s, err
}

// Callback is an outbound notification the sender owes somebody.
type Callback struct {
	ID             uuid.UUID       `json:"id"`
	TransmissionID *uuid.UUID      `json:"transmission_id,omitempty"`
	URL            string          `json:"url"`
	Event          string          `json:"event"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	LastStatus     *int            `json:"last_status,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	DeliveredAt    *time.Time      `json:"delivered_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Callbacks is the sender's callback repository.
//
// The sender records the callback URL a request supplied, but does not deliver the file itself: the
// receiver does that, because the receiver is the side that ends up holding a merged and verified
// file. What the sender keeps here is the record — which URL was asked for, and what the receiver
// reported back about delivering to it — so a caller can ask the sender what became of their request
// rather than having to ask across the air gap.
type Callbacks struct{ pool *db.Pool }

// Record stores the callback a transmission was created with.
func (r *Callbacks) Record(ctx context.Context, c Callback) (Callback, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if len(c.Payload) == 0 {
		c.Payload = json.RawMessage("{}")
	}
	var out Callback
	err := r.pool.QueryRow(ctx, `
		INSERT INTO callbacks (id, transmission_id, url, event, payload)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, transmission_id, url, event, status, attempts, last_status, last_error,
		          payload, delivered_at, created_at`,
		c.ID, c.TransmissionID, c.URL, c.Event, c.Payload).Scan(&out.ID, &out.TransmissionID,
		&out.URL, &out.Event, &out.Status, &out.Attempts, &out.LastStatus, &out.LastError,
		&out.Payload, &out.DeliveredAt, &out.CreatedAt)
	return out, err
}

// Settle records what the receiver reported about delivering to the URL.
func (r *Callbacks) Settle(ctx context.Context, transmissionID uuid.UUID, delivered bool, status int, reason string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	state := "failed"
	if delivered {
		state = "delivered"
	}
	_, err = r.pool.Exec(ctx, `
		UPDATE callbacks SET status = $2, last_status = $3, last_error = $4, payload = $5,
			attempts = attempts + 1,
			delivered_at = CASE WHEN $2 = 'delivered' THEN now() ELSE delivered_at END,
			updated_at = now()
		WHERE transmission_id = $1`, transmissionID, state, nullableStatus(status), reason, encoded)
	return err
}

// ForTransmission returns a transmission's callbacks.
func (r *Callbacks) ForTransmission(ctx context.Context, transmissionID uuid.UUID) ([]Callback, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, transmission_id, url, event, status, attempts, last_status, last_error,
		       payload, delivered_at, created_at
		FROM callbacks WHERE transmission_id = $1 ORDER BY created_at`, transmissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Callback
	for rows.Next() {
		var c Callback
		if err := rows.Scan(&c.ID, &c.TransmissionID, &c.URL, &c.Event, &c.Status, &c.Attempts,
			&c.LastStatus, &c.LastError, &c.Payload, &c.DeliveredAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func nullableStatus(status int) *int {
	if status == 0 {
		return nil
	}
	return &status
}

// Stats records measurements over time.
type Stats struct{ pool *db.Pool }

// Sample is one recorded measurement.
type Sample struct {
	Metric         string     `json:"metric"`
	Value          float64    `json:"value"`
	TransmissionID *uuid.UUID `json:"transmission_id,omitempty"`
	RecordedAt     time.Time  `json:"recorded_at"`
}

// Record appends a measurement.
//
// Samples are appended rather than a counter being updated, so a chart can show how a figure
// moved rather than only where it ended up — which for throughput and acknowledgement latency
// is the entire question an operator is asking.
func (r *Stats) Record(ctx context.Context, metric string, value float64, transmissionID *uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO statistics (transmission_id, metric, value) VALUES ($1, $2, $3)`,
		transmissionID, metric, value)
	return err
}

// Series returns a metric's samples, oldest first.
func (r *Stats) Series(ctx context.Context, metric string, transmissionID *uuid.UUID, limit int) ([]Sample, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT metric, value, transmission_id, recorded_at FROM statistics
		WHERE metric = $1 AND ($2::uuid IS NULL OR transmission_id = $2)
		ORDER BY recorded_at LIMIT $3`, metric, transmissionID, page(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Sample
	for rows.Next() {
		var s Sample
		if err := rows.Scan(&s.Metric, &s.Value, &s.TransmissionID, &s.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SenderKey is an encryption key the operator has saved, so a transfer can be created against
// one that already exists rather than pasting its hex in again every time.
type SenderKey struct {
	ID        int64     `json:"id"`
	Key       []byte    `json:"-"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// SenderKeys is the saved-key repository.
type SenderKeys struct{ pool *db.Pool }

// List returns every saved key, oldest first — the order they were added, which is the order
// an operator would expect to see them in.
func (r *SenderKeys) List(ctx context.Context) ([]SenderKey, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, key, label, created_at FROM sender_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SenderKey
	for rows.Next() {
		var k SenderKey
		if err := rows.Scan(&k.ID, &k.Key, &k.Label, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Add saves a new key.
func (r *SenderKeys) Add(ctx context.Context, key []byte, label string) (SenderKey, error) {
	var k SenderKey
	err := r.pool.QueryRow(ctx, `
		INSERT INTO sender_keys (key, label) VALUES ($1, $2)
		RETURNING id, key, label, created_at`,
		key, label).Scan(&k.ID, &k.Key, &k.Label, &k.CreatedAt)
	return k, err
}

// Get returns one saved key, for a transfer request that names it by id.
func (r *SenderKeys) Get(ctx context.Context, id int64) (SenderKey, error) {
	var k SenderKey
	err := r.pool.QueryRow(ctx,
		`SELECT id, key, label, created_at FROM sender_keys WHERE id = $1`, id).Scan(
		&k.ID, &k.Key, &k.Label, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SenderKey{}, fmt.Errorf("%w: sender key %d", ErrNotFound, id)
	}
	return k, err
}

// Delete removes a saved key.
func (r *SenderKeys) Delete(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM sender_keys WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: sender key %d", ErrNotFound, id)
	}
	return nil
}

// DisplaySettings holds the display settings an operator changed through the UI.
//
// It exists because applying them was not enough. The settings API mutated the running configuration and
// wrote nothing down, which the reloadable settings tolerated and the display sink did not: the sink is read
// once at startup, so a change took effect on the next restart, and the restart re-read the file and the
// environment and discarded it. The transfer-channel toggle could not work.
//
// Sparse by construction — one row per setting actually changed, keyed by the setting's name. Anything absent
// keeps following sender.yaml and the environment, so the first edit through the UI does not pin every other
// field to whatever it happened to be at the time.
type DisplaySettings struct{ pool *db.Pool }

// All returns every stored setting, ready to lay over a freshly loaded configuration.
//
// An empty map is the normal state, not an error: it means nobody has changed anything and the file and the
// environment are the whole story.
func (r *DisplaySettings) All(ctx context.Context) (map[string]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT key, value FROM display_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

// Set stores the given settings, leaving every other stored setting alone.
//
// One statement rather than a loop, which makes it atomic without a transaction to manage: a change touching
// several settings at once — a geometry change is three — either lands whole or not at all, and half a
// geometry is a configuration nobody asked for.
//
// Upsert, because the key is the identity: an operator changing the same setting twice should leave one row
// holding the newer value rather than two rows disagreeing.
func (r *DisplaySettings) Set(ctx context.Context, settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}

	keys := make([]string, 0, len(settings))
	values := make([]string, 0, len(settings))
	for key, value := range settings {
		keys = append(keys, key)
		values = append(values, value)
	}

	_, err := r.pool.Exec(ctx,
		`INSERT INTO display_settings (key, value, updated_at)
		 SELECT k, v, now() FROM unnest($1::text[], $2::text[]) AS t(k, v)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		keys, values)
	return err
}

// page bounds a limit, so a caller cannot ask for the whole table by accident.
func page(limit int) int {
	if limit <= 0 || limit > 1000 {
		return 100
	}
	return limit
}
