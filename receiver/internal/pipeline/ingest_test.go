package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"image"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/receiver/internal/store"
	"github.com/opticaltransport/otp/receiver/internal/testdb"
)

// Ingest is the receiver's other front door: a frame archive replayed from a USB stick rather
// than photographed off a screen. These tests exercise it end to end, against a real database
// and a real encoder, because the point of Ingest is that nothing downstream can tell an
// uploaded frame from a captured one — which is only proven by actually running one through.

// noFrameSource is a capture source that never has anything on it. Ingest tests want the
// applier loop running so injected frames have somewhere to be received, but no camera or
// file channel competing with them.
type noFrameSource struct{}

func (noFrameSource) Name() string { return "stub" }
func (noFrameSource) Next(ctx context.Context) (Capture, error) {
	select {
	case <-ctx.Done():
		return Capture{}, ctx.Err()
	case <-time.After(2 * time.Millisecond):
	}
	return Capture{}, ErrNoFrame
}
func (noFrameSource) Close() error { return nil }

// ingestHarness runs a real receiver against a real, migrated database, with the applier loop
// live so Ingest has something to hand frames to.
type ingestHarness struct {
	t       *testing.T
	r       *Receiver
	st      *store.Store
	objects objectstore.Store
	acks    objectstore.Store
	cfg     config.Config
	cancel  context.CancelFunc
	done    chan error
}

func newIngestHarness(t *testing.T) *ingestHarness {
	t.Helper()
	pool := testdb.New(t)
	st := store.New(pool)

	objects, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = objects.Close() })

	acks, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = acks.Close() })

	cfg := config.Default()
	cfg.Capture.IdleInterval = 2 * time.Millisecond
	cfg.Capture.DecodeWorkers = 2
	cfg.Ack.Secret = "ingest test ack secret, long enough to sign with"

	watcher := config.NewWatcher("", cfg)
	r := New(st, objects, acks, noFrameSource{}, watcher, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	h := &ingestHarness{t: t, r: r, st: st, objects: objects, acks: acks, cfg: cfg, cancel: cancel, done: done}
	t.Cleanup(h.stop)

	// Run does its setup (creating the session, standing up the channels) before the applier
	// loop can receive anything; give it a moment rather than racing Ingest against startup.
	require.Eventually(t, func() bool { return r.running.Load() }, time.Second, time.Millisecond)
	return h
}

func (h *ingestHarness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("receiver did not stop")
	}
}

// oneChunkTransmission builds every frame of the smallest possible transfer: a manifest and a
// single data frame, encoded exactly the way the sender would render them.
type oneChunkTransmission struct {
	transmissionID uuid.UUID
	manifestImage  image.Image
	dataImage      image.Image
	payload        []byte
	filename       string
}

func buildOneChunkTransmission(t *testing.T, encrypt bool, key []byte) oneChunkTransmission {
	t.Helper()

	payload := []byte("hello, sneakernet")
	sum := sha256.Sum256(payload)
	manifest := protocol.Manifest{
		Filename:       "hello.txt",
		OriginalSize:   uint64(len(payload)),
		OriginalSHA256: sum,
		CompressedSize: uint64(len(payload)),
		ChunkCount:     1,
		ChunkSize:      uint32(len(payload)),
		CompressionID:  0,
	}

	layout, err := protocol.NewLayoutQuiet(128, 128, 4, 2)
	require.NoError(t, err)
	enc, err := encoding.ByName("color16")
	require.NoError(t, err)
	depth := enc.DefaultBitDepth()

	txID := uuid.New()

	manifestFrame, err := protocol.NewManifestFrame(protocol.Header{
		TransmissionID: txID,
		FrameNumber:    0,
		TotalChunks:    1,
	}, manifest)
	require.NoError(t, err)
	manifestImg, err := enc.Encode(manifestFrame, layout, depth)
	require.NoError(t, err)

	dataHeader := protocol.Header{
		TransmissionID: txID,
		FrameNumber:    1,
		ChunkNumber:    0,
		TotalChunks:    1,
		Flags:          protocol.FlagLastChunk | protocol.FlagEndOfStream,
	}

	var dataFrame *protocol.Frame
	if encrypt {
		dataFrame, err = protocol.NewEncryptedFrame(key, protocol.EncryptionChaCha20Poly1305, dataHeader, payload)
	} else {
		dataFrame = protocol.NewFrame(dataHeader, payload)
	}
	require.NoError(t, err)
	dataImg, err := enc.Encode(dataFrame, layout, depth)
	require.NoError(t, err)

	return oneChunkTransmission{
		transmissionID: txID,
		manifestImage:  manifestImg,
		dataImage:      dataImg,
		payload:        payload,
		filename:       "hello.txt",
	}
}

// TestIngestFullRoundTrip is the happy path: a manifest and a data frame, uploaded rather than
// captured, reassemble into exactly the file the sender started with.
func TestIngestFullRoundTrip(t *testing.T) {
	h := newIngestHarness(t)
	tx := buildOneChunkTransmission(t, false, nil)
	ctx := context.Background()

	manifestResult, err := h.r.Ingest(ctx, tx.manifestImage, nil)
	require.NoError(t, err)
	require.True(t, manifestResult.Decoded)
	require.True(t, manifestResult.IsManifest)
	require.NotNil(t, manifestResult.TransmissionID)
	require.Equal(t, tx.transmissionID, *manifestResult.TransmissionID)

	dataResult, err := h.r.Ingest(ctx, tx.dataImage, nil)
	require.NoError(t, err)
	require.True(t, dataResult.Decoded)
	require.False(t, dataResult.IsManifest)
	require.NotNil(t, dataResult.ChunkNumber)
	require.Equal(t, int64(0), *dataResult.ChunkNumber)

	require.Eventually(t, func() bool {
		merged, err := h.st.Merged.Get(ctx, tx.transmissionID)
		return err == nil && merged.Verified
	}, 5*time.Second, 10*time.Millisecond)

	merged, err := h.st.Merged.Get(ctx, tx.transmissionID)
	require.NoError(t, err)
	require.True(t, merged.Verified)

	body, err := objectstore.GetBytes(ctx, h.objects, merged.StoredPath, merged.SizeBytes+1)
	require.NoError(t, err)
	require.Equal(t, tx.payload, body)
}

// TestIngestEncryptedNeedsKeyring covers the encrypted case: the chunk's own checksums pass —
// the frame decoded fine — but the payload will not open without the key, so nothing completes
// and the sender is told to resend. Once the key is loaded, a re-ingest of the same data frame
// finishes the transfer.
func TestIngestEncryptedNeedsKeyring(t *testing.T) {
	h := newIngestHarness(t)
	key := bytes.Repeat([]byte{0x42}, protocol.KeySize)
	tx := buildOneChunkTransmission(t, true, key)
	ctx := context.Background()

	_, err := h.r.Ingest(ctx, tx.manifestImage, nil)
	require.NoError(t, err)

	dataResult, err := h.r.Ingest(ctx, tx.dataImage, nil)
	require.NoError(t, err)
	require.True(t, dataResult.Decoded, "the frame's own checksums pass; only the payload fails to open")
	require.NotNil(t, dataResult.ChunkNumber)

	// Give the applier a moment, then confirm nothing merged and the ack channel says why.
	time.Sleep(50 * time.Millisecond)
	_, err = h.st.Merged.Get(ctx, tx.transmissionID)
	require.Error(t, err, "an undecryptable chunk must not be treated as complete")

	acks, err := h.acks.List(ctx, protocol.AckDir(tx.transmissionID))
	require.NoError(t, err)
	require.NotEmpty(t, acks, "a decrypt failure is still acknowledged, so the sender knows to retry")

	found := false
	for _, obj := range acks {
		data, err := objectstore.GetBytes(ctx, h.acks, obj.Key, 1<<20)
		require.NoError(t, err)
		ack, err := protocol.ParseAck([]byte(h.cfg.Ack.Secret), data)
		require.NoError(t, err)
		if ack.Status == protocol.AckCRCFailed {
			found = true
		}
	}
	require.True(t, found, "the receiver must report the chunk as needing to be resent, not silently drop it")

	// Load the key. The keyring is cached for a few seconds — the same caching the capture path
	// relies on to stay affordable at frame rate — so the key is not visible on the very next
	// call; retrying the ingest is exactly how an operator would experience it too, reloading
	// the archive once the key is in place.
	_, err = h.st.DecoderKeys.Add(ctx, key, "test key")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		if _, err := h.r.Ingest(ctx, tx.dataImage, nil); err != nil {
			return false
		}
		merged, err := h.st.Merged.Get(ctx, tx.transmissionID)
		return err == nil && merged.Verified
	}, 6*time.Second, 250*time.Millisecond)
}

// TestIngestWhenNotRunningErrors is the failure mode that must never be a hang: a receiver
// whose Run has not started (or has already stopped) must fail an Ingest call outright.
func TestIngestWhenNotRunningErrors(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)
	objects, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = objects.Close() })
	acks, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = acks.Close() })

	watcher := config.NewWatcher("", config.Default())
	r := New(st, objects, acks, noFrameSource{}, watcher, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := r.Ingest(ctx, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil)
	require.Error(t, err)
	require.Equal(t, IngestResult{}, result)
}

// TestIngestRacesShutdownWithoutHanging is the probe for the shutdown-vs-ingest race the design
// explicitly accepts and handles rather than prevents: running observed true a moment before
// Run tears down. TestIngestWhenNotRunningErrors only covers "never started" — this covers "was
// running, is stopping right now". It fires cancel() and Ingest at each other repeatedly, with
// no synchronisation between them beyond a shared start gate, and requires every attempt to
// resolve — with an error or a result, never silence — within a bounded deadline. Run with
// -race: it was exactly this kind of concurrent Run/Ingest interleaving that caught the
// r.session ordering bug fixed in Run (running is now set only after r.session is assigned),
// and this test exists so that class of regression has a permanent, repeated trigger rather
// than relying on the round-trip tests to happen to schedule it that way.
func TestIngestRacesShutdownWithoutHanging(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)
	objects, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = objects.Close() })
	acks, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = acks.Close() })

	cfg := config.Default()
	cfg.Capture.IdleInterval = time.Millisecond
	cfg.Capture.DecodeWorkers = 1
	watcher := config.NewWatcher("", cfg)

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))

	const iterations = 200
	for i := 0; i < iterations; i++ {
		r := New(st, objects, acks, noFrameSource{}, watcher, zap.NewNop())

		runCtx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- r.Run(runCtx) }()

		// Wait for Run to actually be under way before racing its shutdown — otherwise most
		// iterations would just be "Ingest before running", which is already covered.
		require.Eventually(t, func() bool { return r.running.Load() }, time.Second, time.Microsecond)

		// reqCtx stands in for an HTTP request's own context: independent of runCtx, and not
		// cancelled by Run shutting down. Only runDone can unblock an Ingest call caught by this
		// race — which is exactly what the fix under test is.
		reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			cancel()
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = r.Ingest(reqCtx, img, nil)
		}()
		close(start)

		settled := make(chan struct{})
		go func() {
			wg.Wait()
			close(settled)
		}()
		select {
		case <-settled:
		case <-time.After(3 * time.Second):
			t.Fatalf("iteration %d: cancel() and Ingest did not both return — the shutdown race hung", i)
		}
		reqCancel()

		select {
		case <-runDone:
		case <-time.After(3 * time.Second):
			t.Fatalf("iteration %d: Run did not stop after cancel", i)
		}
	}
}
