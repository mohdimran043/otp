// Package harness runs a whole receiver in one process, for tests and for the loopback
// demonstration.
//
// It is the receiver's counterpart to the sender's harness, and exists for the same reason: the two
// applications must not import each other, so each exposes its own operator-level view and the
// end-to-end tests import both.
package harness

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/simulate"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/db"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/receiver/internal/pipeline"
	"github.com/opticaltransport/otp/receiver/internal/store"
)

// Options configure a receiver.
type Options struct {
	// DatabaseURL is the receiver's own database.
	DatabaseURL string

	// StorageRoot is where captured frames, chunks, and merged files go.
	StorageRoot string

	// FrameDir is the camera: the directory the sender's display writes into.
	FrameDir string

	// AckDir is the shared volume acknowledgements are written to.
	AckDir string

	// AckSecret signs them, and must match the sender's.
	AckSecret string

	// AllowedCallbackHosts is where merged files may be delivered. Naming the hosts rather than
	// disabling the check means a test exercises the same path a deployment does.
	AllowedCallbackHosts []string

	// Degrade stands in for the optics, applied to every frame before it is decoded.
	Degrade simulate.Profile

	// Drop stands in for frames the camera missed: a tear, a hand, a refresh caught mid-scan. It is
	// how loss is injected on purpose.
	Drop func(sequence int64) bool

	// EncryptionKeyHex decrypts payloads and must match the sender's.
	EncryptionKeyHex string

	// MinFinderScore and MinTimingScore are the confidence floors below which a located frame is
	// discarded unread. Zero leaves the defaults in place.
	MinFinderScore float64
	MinTimingScore float64

	Log *zap.Logger
}

// Receiver is a running receiver.
type Receiver struct {
	cfg     *config.Watcher
	pool    *db.Pool
	store   *store.Store
	objects objectstore.Store
	acks    objectstore.Store
	source  *pipeline.FileSource
	rec     *pipeline.Receiver

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start brings up a receiver and begins capturing.
func Start(ctx context.Context, opts Options) (*Receiver, error) {
	if opts.Log == nil {
		opts.Log = zap.NewNop()
	}

	cfg := config.Default()
	cfg.Database.URL = opts.DatabaseURL
	cfg.Storage.Root = opts.StorageRoot
	cfg.Capture.Dir = opts.FrameDir
	cfg.Capture.Consume = true

	// A small, fixed number of decoders rather than one per core.
	//
	// The default is per-core because that is right for a deployment, and wrong for a test: every loopback in
	// the suite would start nineteen CPU-bound goroutines on a twenty-core machine, several suites run in the
	// same container as two databases, and the result was a suite where a different test timed out on each
	// run. Concurrency is not what these tests are checking — losslessness is — and a fixed figure makes them
	// say the same thing twice.
	cfg.Capture.DecodeWorkers = 4
	cfg.Capture.IdleInterval = 10 * time.Millisecond
	cfg.Ack.Dir = opts.AckDir
	cfg.Ack.Secret = opts.AckSecret
	cfg.Auth.JWTSecret = "harness jwt secret long enough to sign with"
	cfg.Callback.AllowedHosts = opts.AllowedCallbackHosts
	cfg.Callback.Timeout = 10 * time.Second

	if opts.EncryptionKeyHex != "" {
		cfg.Decoder.EncryptionKeyHex = opts.EncryptionKeyHex
	}
	if opts.MinFinderScore > 0 {
		cfg.Decoder.MinFinderScore = opts.MinFinderScore
	}
	if opts.MinTimingScore > 0 {
		cfg.Decoder.MinTimingScore = opts.MinTimingScore
	}
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
	// The acknowledgement channel is its own store rooted at the shared volume, which is what it is in
	// a real deployment: a different mount, reachable by both applications, with its own lifetime.
	acks, err := objectstore.NewFilesystem(cfg.Ack.Dir)
	if err != nil {
		pool.Close()
		return nil, err
	}

	source, err := pipeline.NewFileSource(pipeline.FileSourceOptions{
		Dir:     cfg.Capture.Dir,
		Degrade: opts.Degrade,
		Drop:    opts.Drop,
		Consume: cfg.Capture.Consume,
	})
	if err != nil {
		pool.Close()
		return nil, err
	}

	watcher := config.NewWatcher("", cfg)
	st := store.New(pool)
	rec := pipeline.New(st, objects, acks, source, watcher, opts.Log)

	r := &Receiver{
		cfg:     watcher,
		pool:    pool,
		store:   st,
		objects: objects,
		acks:    acks,
		source:  source,
		rec:     rec,
	}

	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_ = rec.Run(runCtx)
	}()

	return r, nil
}

// MergedFile is a reassembled file as the receiver holds it.
type MergedFile struct {
	Filename    string
	SizeBytes   int64
	SHA256      string
	Verified    bool
	VerifyError string
	StoredPath  string
}

// MergedFile returns what the receiver reassembled for a transmission.
func (r *Receiver) MergedFile(ctx context.Context, transmission uuid.UUID) (MergedFile, error) {
	f, err := r.store.Merged.Get(ctx, transmission)
	if err != nil {
		return MergedFile{}, err
	}
	return MergedFile{
		Filename:    f.Filename,
		SizeBytes:   f.SizeBytes,
		SHA256:      hex.EncodeToString(f.SHA256),
		Verified:    f.Verified,
		VerifyError: f.VerifyError,
		StoredPath:  f.StoredPath,
	}, nil
}

// MergedBytes returns the reassembled file's contents, so a test can compare it against what was
// uploaded rather than trusting the hash the receiver computed.
func (r *Receiver) MergedBytes(ctx context.Context, transmission uuid.UUID) ([]byte, error) {
	f, err := r.store.Merged.Get(ctx, transmission)
	if err != nil {
		return nil, err
	}
	return objectstore.GetBytes(ctx, r.objects, f.StoredPath, f.SizeBytes+1)
}

// ChunkCounts is how many source chunks arrived and how many of those were rebuilt from parity.
func (r *Receiver) ChunkCounts(ctx context.Context, transmission uuid.UUID) (arrived, recovered int, err error) {
	return r.store.Chunks.Counts(ctx, transmission)
}

// MissingChunks returns the chunks the receiver is still waiting for.
func (r *Receiver) MissingChunks(ctx context.Context, transmission uuid.UUID) ([]int, error) {
	return r.store.Chunks.Missing(ctx, transmission)
}

// CaptureStats describes the optical channel as the receiver experienced it.
type CaptureStats struct {
	FramesCaptured int64
	FramesDecoded  int64
	FramesFailed   int64
}

// CaptureStats returns the current capture session's figures.
func (r *Receiver) CaptureStats(ctx context.Context) (CaptureStats, error) {
	session := r.rec.Session()
	if session == uuid.Nil {
		return CaptureStats{}, fmt.Errorf("harness: the receiver has not started a session yet")
	}
	s, err := r.store.Sessions.Get(ctx, session)
	if err != nil {
		return CaptureStats{}, err
	}
	return CaptureStats{
		FramesCaptured: s.FramesCaptured,
		FramesDecoded:  s.FramesDecoded,
		FramesFailed:   s.FramesFailed,
	}, nil
}

// Manifest is what a transmission told the receiver about itself.
type Manifest struct {
	Filename     string
	OriginalSize int64
	SHA256       string
	ChunkCount   int
	ChunkSize    int
	CallbackURL  string
}

// Manifest returns a transmission's manifest as the receiver recorded it.
func (r *Receiver) Manifest(ctx context.Context, transmission uuid.UUID) (Manifest, error) {
	m, err := r.store.Manifests.Get(ctx, transmission)
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{
		Filename:     m.Filename,
		OriginalSize: m.OriginalSize,
		SHA256:       hex.EncodeToString(m.OriginalSHA256),
		ChunkCount:   m.ChunkCount,
		ChunkSize:    m.ChunkSize,
		CallbackURL:  m.CallbackURL,
	}, nil
}

// Stop shuts the receiver down.
func (r *Receiver) Stop() {
	r.cancel()
	r.wg.Wait()
	_ = r.source.Close()
	_ = r.objects.Close()
	_ = r.acks.Close()
	r.pool.Close()
}
