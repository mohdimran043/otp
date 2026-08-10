// Package harness runs a whole sender in one process, for tests and for the loopback demonstration.
//
// It exists because of a constraint worth keeping: the sender and the receiver must not import each
// other. They share a protocol, a directory, and nothing else, which is what lets either be
// restarted or replaced without the other noticing — and a test that reached from one into the other
// would quietly make that untrue.
//
// So each application exposes a harness of its own, over its own internals, and the end-to-end tests
// import the two harnesses. The application code stays internal; what is exported here is the
// operator's view of it — start it, hand it a file, ask what happened.
package harness

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/ackwatch"
	"github.com/opticaltransport/otp/sender/internal/api"
	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/db"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/optical"
	"github.com/opticaltransport/otp/sender/internal/pipeline"
	"github.com/opticaltransport/otp/sender/internal/scheduler"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Options configure a sender.
type Options struct {
	// DatabaseURL is the sender's own database.
	DatabaseURL string

	// StorageRoot is where uploads, chunks, and rendered frames go.
	StorageRoot string

	// FrameDir is the display: the directory a receiver's camera watches.
	FrameDir string

	// AckDir is the shared volume acknowledgements arrive in.
	AckDir string

	// AckSecret signs them, and must match the receiver's.
	AckSecret string

	// The optical profile. Every field is optional and falls back to the harness default, which is a
	// small grid at a high frame rate — fast enough that a test is not dominated by the display.
	//
	// These are named fields rather than a callback over the configuration type, because the
	// configuration is internal to this module: a caller in another module could not name it. Naming
	// the knobs instead makes this an operator-level surface, which is what a harness should be.
	Encoder          string
	BitDepth         int
	Compression      string
	CompressionLevel int
	FECCodec         string
	FECDataShards    int
	FECParityShards  int
	GridWidth        int
	GridHeight       int
	CellPixels       int
	ManifestInterval int
	EncryptionKeyHex string

	// FPS, WindowSize, AckTimeout, and MaxRetries govern the display loop and how patient it is.
	FPS        float64
	WindowSize int
	AckTimeout time.Duration
	MaxRetries int

	Log *zap.Logger
}

// Sender is a running sender.
type Sender struct {
	cfg     *config.Watcher
	pool    *db.Pool
	store   *store.Store
	jobs    *jobs.Store
	objects objectstore.Store
	engine  *jobs.Engine
	line    *pipeline.Pipeline
	// The display, wrapped as the real sender wraps it. Not a bare FileSink: the display sequence is
	// assigned by Live, and a harness that skipped it would not exercise the arrangement a deployment uses —
	// which is exactly where concurrent transfers once overwrote each other's frames.
	sink   *optical.Live
	acks   *ackwatch.Watcher
	server *httptest.Server
	log    *zap.Logger

	// displays tracks the display loops started for each transmission, so Stop can wait for them
	// rather than leaving goroutines writing frames into a directory a test is about to delete.
	displays sync.WaitGroup

	statsMu sync.Mutex
	stats   map[uuid.UUID]scheduler.Stats

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start brings up a sender: migrations, storage, the job engine, the acknowledgement watcher, and the
// HTTP API.
func Start(ctx context.Context, opts Options) (*Sender, error) {
	if opts.Log == nil {
		opts.Log = zap.NewNop()
	}

	cfg := config.Default()
	cfg.Database.URL = opts.DatabaseURL
	cfg.Storage.Root = opts.StorageRoot
	cfg.Display.Dir = opts.FrameDir
	cfg.Ack.Dir = opts.AckDir
	cfg.Ack.Secret = opts.AckSecret
	cfg.Auth.JWTSecret = "harness jwt secret long enough to sign with"

	// Intervals a test can wait on rather than production ones. The frame rate is the important one:
	// the whole transfer runs at it, so a realistic ten frames a second would make every test minutes
	// long without testing anything a fast one does not.
	cfg.Display.FPS = 200
	cfg.Display.WindowSize = 32
	cfg.Ack.PollInterval = 50 * time.Millisecond
	cfg.Ack.Timeout = 2 * time.Second
	cfg.Ack.MaxRetries = 60
	cfg.Jobs.PollInterval = 20 * time.Millisecond
	cfg.Jobs.BackoffBase = 20 * time.Millisecond
	cfg.Jobs.BackoffMax = 200 * time.Millisecond
	cfg.Optical.GridWidth = 96
	cfg.Optical.GridHeight = 96
	cfg.Optical.CellPixels = 6
	cfg.Optical.ManifestInterval = 24

	applyOverrides(&cfg, opts)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	objects, err := objectstore.Open(ctx, cfg.Storage)
	if err != nil {
		pool.Close()
		return nil, err
	}
	channel, err := optical.NewFileSink(cfg.Display.Dir, cfg.Display.RetainFrames)
	if err != nil {
		pool.Close()
		return nil, err
	}

	watcher := config.NewWatcher("", cfg)
	st := store.New(pool)
	js := jobs.NewStore(pool)
	engine := jobs.NewEngine(js, watcher, opts.Log)
	line := pipeline.New(st, js, objects, watcher, opts.Log)
	line.Register(engine)
	acks := ackwatch.New(st, watcher, opts.Log)

	s := &Sender{
		cfg:     watcher,
		pool:    pool,
		store:   st,
		jobs:    js,
		objects: objects,
		engine:  engine,
		line:    line,
		sink:    optical.NewLive(channel),
		acks:    acks,
		log:     opts.Log,
		stats:   map[uuid.UUID]scheduler.Stats{},
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	server := api.New(api.Options{
		Store:    st,
		Jobs:     js,
		Objects:  objects,
		Pipeline: line,
		Acks:     acks,
		Config:   watcher,
		Log:      opts.Log,
		Transmit: func(_ context.Context, id uuid.UUID) { s.display(runCtx, id) },
	})
	s.server = httptest.NewServer(server.Routes())

	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		_ = engine.Start(runCtx)
	}()
	go func() {
		defer s.wg.Done()
		_ = acks.Run(runCtx)
	}()

	return s, nil
}

// applyOverrides copies the options a caller set over the defaults, leaving the rest alone.
func applyOverrides(cfg *config.Config, opts Options) {
	if opts.Encoder != "" {
		cfg.Optical.Encoder = opts.Encoder
	}
	// A bit depth of zero already means "whichever depth the encoder prefers", so it cannot be
	// distinguished from unset — and that is the right default either way.
	cfg.Optical.BitDepth = opts.BitDepth
	if opts.Compression != "" {
		cfg.Optical.Compression = opts.Compression
	}
	if opts.CompressionLevel > 0 {
		cfg.Optical.Level = opts.CompressionLevel
	}
	if opts.FECCodec != "" {
		cfg.Optical.FEC.Codec = opts.FECCodec
		// The shard counts belong to the codec that was named, so they are taken together: a codec set
		// without them would otherwise inherit counts chosen for a different code.
		cfg.Optical.FEC.DataShards = opts.FECDataShards
		cfg.Optical.FEC.ParityShards = opts.FECParityShards
	}
	if opts.GridWidth > 0 {
		cfg.Optical.GridWidth = opts.GridWidth
	}
	if opts.GridHeight > 0 {
		cfg.Optical.GridHeight = opts.GridHeight
	}
	if opts.CellPixels > 0 {
		cfg.Optical.CellPixels = opts.CellPixels
	}
	if opts.ManifestInterval > 0 {
		cfg.Optical.ManifestInterval = opts.ManifestInterval
	}
	if opts.EncryptionKeyHex != "" {
		cfg.Optical.EncryptionKeyHex = opts.EncryptionKeyHex
	}
	if opts.FPS > 0 {
		cfg.Display.FPS = opts.FPS
	}
	if opts.WindowSize > 0 {
		cfg.Display.WindowSize = opts.WindowSize
	}
	if opts.AckTimeout > 0 {
		cfg.Ack.Timeout = opts.AckTimeout
	}
	if opts.MaxRetries > 0 {
		cfg.Ack.MaxRetries = opts.MaxRetries
	}
}

// display waits for a transmission to be prepared and then shows it until every chunk is
// acknowledged.
func (s *Sender) display(ctx context.Context, id uuid.UUID) {
	s.displays.Add(1)
	go func() {
		defer s.displays.Done()

		if err := s.WaitForReady(ctx, id, 10*time.Minute); err != nil {
			s.log.Warn("transmission never became ready", zap.Error(err))
			return
		}

		// A scheduler per transmission, because its retransmission timers describe one transfer. Sharing
		// one across transmissions would have a slow transfer's timers govern a fast one's.
		sched := scheduler.New(s.store, s.objects, s.sink, s.cfg, s.log)
		stats, err := sched.Run(ctx, id)

		s.statsMu.Lock()
		s.stats[id] = stats
		s.statsMu.Unlock()

		if err != nil && ctx.Err() == nil {
			s.log.Warn("display stopped", zap.String("transmission", id.String()), zap.Error(err))
		}
	}()
}

// URL is the API's base address.
func (s *Sender) URL() string { return s.server.URL }

// WaitForReady blocks until a transmission has been prepared, or fails if preparation failed.
func (s *Sender) WaitForReady(ctx context.Context, id uuid.UUID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := s.store.Transmissions.Get(ctx, id)
		if err != nil {
			return err
		}
		switch tx.Status {
		case store.TxReady, store.TxTransmitting, store.TxCompleted:
			return nil
		case store.TxFailed, store.TxCancelled:
			return fmt.Errorf("harness: transmission %s is %s: %s", id, tx.Status, tx.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("harness: transmission %s was not prepared within %s", id, timeout)
}

// WaitForResult blocks until the receiver has reported on a transmission.
func (s *Sender) WaitForResult(ctx context.Context, id uuid.UUID, timeout time.Duration) (protocol.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.acks.WaitForResult(ctx, id)
}

// Transfer describes a transmission as the sender sees it.
type Transfer struct {
	ID             uuid.UUID
	Status         string
	Filename       string
	CallbackURL    string
	OriginalSize   int64
	CompressedSize int64
	ChunkCount     int
	ChunkSize      int
	FrameCount     int
	AckedChunks    int
	Retransmits    int
	Error          string
}

// Transfer returns a transmission's current state.
func (s *Sender) Transfer(ctx context.Context, id uuid.UUID) (Transfer, error) {
	tx, err := s.store.Transmissions.Get(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	file, err := s.store.Files.Get(ctx, tx.FileID)
	if err != nil {
		return Transfer{}, err
	}
	return Transfer{
		ID:             tx.ID,
		Status:         string(tx.Status),
		Filename:       file.Filename,
		CallbackURL:    tx.CallbackURL,
		OriginalSize:   tx.OriginalSize,
		CompressedSize: tx.CompressedSize,
		ChunkCount:     tx.ChunkCount,
		ChunkSize:      tx.ChunkSize,
		FrameCount:     tx.FrameCount,
		AckedChunks:    tx.AckedChunks,
		Retransmits:    tx.Retransmits,
		Error:          tx.Error,
	}, nil
}

// UnackedChunks returns the chunk numbers the sender still believes are outstanding.
//
// It is the sender's own view of loss, and the figure a zero-loss claim rests on from this side: a
// completed transfer has none.
func (s *Sender) UnackedChunks(ctx context.Context, id uuid.UUID) ([]int, error) {
	chunks, err := s.store.Chunks.List(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, c := range chunks {
		if !c.Acked && !c.IsParity {
			out = append(out, c.ESI)
		}
	}
	return out, nil
}

// CallbackRecord is what the sender knows about a callback it was asked for.
type CallbackRecord struct {
	URL        string
	Event      string
	Status     string
	LastStatus *int
	LastError  string
	Delivered  bool
	Attempts   int
}

// Callbacks returns what became of a transmission's callback, as reported back by the receiver.
func (s *Sender) Callbacks(ctx context.Context, id uuid.UUID) ([]CallbackRecord, error) {
	records, err := s.store.Callbacks.ForTransmission(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]CallbackRecord, len(records))
	for i, c := range records {
		out[i] = CallbackRecord{
			URL:        c.URL,
			Event:      c.Event,
			Status:     c.Status,
			LastStatus: c.LastStatus,
			LastError:  c.LastError,
			Delivered:  c.DeliveredAt != nil,
			Attempts:   c.Attempts,
		}
	}
	return out, nil
}

// DisplayStats returns what a completed display run achieved, if it has finished.
func (s *Sender) DisplayStats(id uuid.UUID) (scheduler.Stats, bool) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	stats, ok := s.stats[id]
	return stats, ok
}

// LateDisplay is a frame that went to the display after its chunk had already been acknowledged.
type LateDisplay struct {
	ChunkNumber int
	FrameNumber int
	After       time.Duration
}

// LateDisplays returns the frames displayed more than slack after their chunk was acknowledged.
//
// This is the direct measurement of "an acknowledged chunk is never shown again", and it is worth
// measuring this way rather than by counting frames. The display deliberately never idles: while a window
// is waiting on acknowledgements it repeats the oldest *outstanding* frame, so a high frame count is
// expected behaviour rather than evidence of anything. What must not happen is a frame being shown after
// its chunk is known to have arrived, because that is channel time spent on nothing.
//
// The slack exists because the two events are not simultaneous. The receiver writes an acknowledgement,
// and the sender reads it on its next poll — so a frame displayed in that gap is not a failure to skip, it
// is a frame displayed before the news arrived. Anything well beyond the poll interval is.
func (s *Sender) LateDisplays(ctx context.Context, id uuid.UUID, slack time.Duration) ([]LateDisplay, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.esi, f.frame_number, f.last_displayed - c.acked_at
		FROM chunks c
		JOIN encoded_frames f ON f.chunk_id = c.id
		WHERE c.transmission_id = $1
		  AND c.acked
		  AND c.acked_at IS NOT NULL
		  AND f.last_displayed IS NOT NULL
		  AND f.last_displayed > c.acked_at + $2::interval
		ORDER BY c.esi`, id, slack.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LateDisplay
	for rows.Next() {
		var late LateDisplay
		var after time.Duration
		if err := rows.Scan(&late.ChunkNumber, &late.FrameNumber, &after); err != nil {
			return nil, err
		}
		late.After = after
		out = append(out, late)
	}
	return out, rows.Err()
}

// WaitForDisplay blocks until the display loop for a transmission has finished.
//
// It is needed because the receiver's report and the sender's display do not end at the same moment: the
// receiver writes its verdict as soon as the last chunk merges, while the sender notices that everything
// is acknowledged on its next frame. A caller that read the display's figures the instant the result
// arrived would usually find them missing.
func (s *Sender) WaitForDisplay(ctx context.Context, id uuid.UUID, timeout time.Duration) (scheduler.Stats, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return scheduler.Stats{}, err
		}
		if stats, ok := s.DisplayStats(id); ok {
			return stats, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return scheduler.Stats{}, fmt.Errorf("harness: the display for %s did not finish within %s", id, timeout)
}

// FramesShown is how many frames have gone to the display, including repeats.
func (s *Sender) FramesShown() int64 { return s.sink.Shown() }

// Stop shuts the sender down and waits for its loops to finish.
func (s *Sender) Stop() {
	s.cancel()
	s.engine.Stop()
	s.displays.Wait()
	s.wg.Wait()
	s.server.Close()
	_ = s.objects.Close()
	s.pool.Close()
}
