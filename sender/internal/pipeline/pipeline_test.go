package pipeline_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/png"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/opticaltransport/otp/shared/compress"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/fec"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/pipeline"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// harness is a whole sender back end minus its HTTP layer: a database, an object store, the
// job engine, and the pipeline registered on it.
type harness struct {
	store   *store.Store
	jobs    *jobs.Store
	objects objectstore.Store
	engine  *jobs.Engine
	line    *pipeline.Pipeline
	cfg     config.Config
}

func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()

	pool := testdb.New(t)

	cfg := config.Default()
	cfg.Database.URL = testdb.URLFor(t, pool)
	cfg.Ack.Secret = "test acknowledgement secret"
	cfg.Auth.JWTSecret = "a test jwt secret long enough to sign"
	cfg.Storage.Root = t.TempDir()
	cfg.Jobs.PollInterval = 20 * time.Millisecond
	cfg.Jobs.BackoffBase = 20 * time.Millisecond
	cfg.Jobs.BackoffMax = 100 * time.Millisecond
	cfg.Jobs.ClaimTimeout = 60 * time.Second
	// A smaller grid than the default keeps the test quick: fewer pixels per frame, and the
	// pipeline's behaviour does not depend on the size.
	cfg.Optical.GridWidth = 96
	cfg.Optical.GridHeight = 96
	cfg.Optical.CellPixels = 4
	if tune != nil {
		tune(&cfg)
	}
	require.NoError(t, cfg.Validate())

	objects, err := objectstore.Open(context.Background(), cfg.Storage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, objects.Close()) })

	log := zaptest.NewLogger(t)
	watcher := config.NewWatcher("", cfg)
	st := store.New(pool)
	js := jobs.NewStore(pool)
	engine := jobs.NewEngine(js, watcher, log)
	line := pipeline.New(st, js, objects, watcher, log)
	line.Register(engine)

	h := &harness{store: st, jobs: js, objects: objects, engine: engine, line: line, cfg: cfg}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.NoError(t, engine.Start(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return h
}

// upload stores a file and creates the transmission that will carry it, exactly as the API
// handler will.
func (h *harness) upload(t *testing.T, name string, body []byte) (store.File, store.Transmission) {
	t.Helper()
	return h.uploadTuned(t, name, body, nil)
}

// uploadTuned is upload with a hook to override fields on the transmission row before it is
// created. It exists for cases the API decides per transfer — encryption and grid geometry
// among them — where a test has to set the row directly because there is no API layer in
// this harness to make that decision for it.
func (h *harness) uploadTuned(t *testing.T, name string, body []byte, tune func(*store.Transmission)) (store.File, store.Transmission) {
	t.Helper()
	ctx := context.Background()

	id := uuid.New()
	key, err := objectstore.Key("files", id.String())
	require.NoError(t, err)
	require.NoError(t, h.objects.Put(ctx, key, bytes.NewReader(body)))

	sum := sha256.Sum256(body)
	file, err := h.store.Files.Create(ctx, store.File{
		ID:         id,
		Filename:   name,
		StoredPath: key,
		SizeBytes:  int64(len(body)),
		SHA256:     sum[:],
	})
	require.NoError(t, err)

	row := store.Transmission{
		FileID:           file.ID,
		Encoder:          h.cfg.Optical.Encoder,
		BitDepth:         h.cfg.Optical.BitDepth,
		Compression:      h.cfg.Optical.Compression,
		CompressionLevel: h.cfg.Optical.Level,
		FECCodec:         h.cfg.Optical.FEC.Codec,
		FECDataShards:    h.cfg.Optical.FEC.DataShards,
		FECParityShards:  h.cfg.Optical.FEC.ParityShards,
		GridWidth:        h.cfg.Optical.GridWidth,
		GridHeight:       h.cfg.Optical.GridHeight,
		CellPixels:       h.cfg.Optical.CellPixels,
		QuietZone:        h.cfg.Optical.QuietZone,
		Encrypted:        h.cfg.Optical.EncryptionKeyHex != "",
		OriginalSize:     int64(len(body)),
	}
	if tune != nil {
		tune(&row)
	}

	tx, err := h.store.Transmissions.Create(ctx, row)
	require.NoError(t, err)
	return file, tx
}

// prepareAndWait runs the whole chain and waits for the transmission to be ready.
func (h *harness) prepareAndWait(t *testing.T, txID uuid.UUID) store.Transmission {
	t.Helper()
	ctx := context.Background()

	chain, err := h.line.Prepare(ctx, txID)
	require.NoError(t, err)
	require.Len(t, chain, 5, "compress, chunk, error-code, render, finalize")

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		tx, err := h.store.Transmissions.Get(ctx, txID)
		require.NoError(t, err)
		if tx.Status == store.TxReady {
			return tx
		}
		if tx.Status == store.TxFailed {
			t.Fatalf("the transmission failed: %s", tx.Error)
		}

		// A failed job says far more about what went wrong than a timeout does.
		all, err := h.jobs.List(ctx, jobs.Filter{TransmissionID: &txID})
		require.NoError(t, err)
		for _, job := range all {
			if job.Status == jobs.StatusFailed {
				t.Fatalf("%s failed: %s", job.Type, job.Error)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the pipeline did not finish in time")
	return store.Transmission{}
}

// testPayload is compressible but not trivially so, like a real file.
func testPayload(size int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	out := make([]byte, 0, size)
	phrases := []string{
		"the receiver writes captured frames to disk before decoding them, always. ",
		"acknowledgements travel through shared storage and never optically. ",
		"a chunk is sized so that exactly one chunk fits in exactly one frame. ",
	}
	for len(out) < size {
		out = append(out, phrases[r.Intn(len(phrases))]...)
		// Some incompressible filler, so the ratio is realistic rather than absurd.
		var noise [16]byte
		r.Read(noise[:])
		out = append(out, noise[:]...)
	}
	return out[:size]
}

// TestPipelineProducesDecodableFrames is the test the whole sender exists to pass: the frames
// it renders must be readable by the same decoder a receiver will use, and the payloads must
// reassemble into the file that was uploaded.
//
// It decodes with the package-level dispatcher rather than the encoder it rendered with,
// because that is what a receiver does — it is told nothing about the frame and has to learn
// the encoding from the header.
func TestPipelineProducesDecodableFrames(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	original := testPayload(48<<10, 1)
	file, tx := h.upload(t, "quarterly-report.tar", original)
	ready := h.prepareAndWait(t, tx.ID)

	require.Positive(t, ready.ChunkCount)
	require.Positive(t, ready.FrameCount)
	require.Positive(t, ready.CompressedSize)
	require.Less(t, ready.CompressedSize, int64(len(original)), "the payload should compress")

	frames, err := h.store.Frames.List(ctx, tx.ID)
	require.NoError(t, err)
	require.Len(t, frames, ready.FrameCount)

	chunks, err := h.store.Chunks.List(ctx, tx.ID)
	require.NoError(t, err)

	// Decode every frame and collect the source chunks back.
	recovered := map[int][]byte{}
	var manifest protocol.Manifest
	manifests := 0

	for _, record := range frames {
		body, err := objectstore.GetBytes(ctx, h.objects, record.StoredPath, 16<<20)
		require.NoError(t, err)
		img, err := png.Decode(bytes.NewReader(body))
		require.NoError(t, err)

		frame, err := encoding.Decode(img, protocol.LocateOptions{})
		require.NoError(t, err, "frame %d must decode", record.FrameNumber)
		require.Equal(t, tx.ID, frame.Header.TransmissionID)

		if frame.Header.Flags.Has(protocol.FlagManifest) {
			manifests++
			manifest, err = protocol.ParseManifest(frame)
			require.NoError(t, err)
			continue
		}
		if frame.Header.Flags.Has(protocol.FlagParity) {
			continue
		}
		recovered[int(frame.Header.ChunkNumber)] = frame.Payload
	}

	require.Positive(t, manifests, "the manifest must be emitted")
	require.Equal(t, file.Filename, manifest.Filename)
	require.Equal(t, uint64(len(original)), manifest.OriginalSize)
	require.Equal(t, [32]byte(file.SHA256), manifest.OriginalSHA256)

	// Reassemble the compressed stream from the decoded chunks, decompress it, and check the
	// result against what was uploaded. This is the whole round trip, minus the optics.
	sourceChunks := 0
	for _, c := range chunks {
		if !c.IsParity {
			sourceChunks++
		}
	}
	require.Equal(t, sourceChunks, len(recovered), "every source chunk must have arrived")

	var stream bytes.Buffer
	for esi := 0; esi < sourceChunks; esi++ {
		payload, ok := recovered[esi]
		require.True(t, ok, "chunk %d is missing", esi)
		stream.Write(payload)
	}
	require.Equal(t, int(manifest.CompressedSize), stream.Len())

	codec, err := compress.ByID(manifest.CompressionID)
	require.NoError(t, err)
	got, err := compress.UnBytes(codec, stream.Bytes(), int64(manifest.OriginalSize))
	require.NoError(t, err)
	require.Equal(t, original, got, "the reassembled file must be byte-identical")
}

// TestPipelineSurvivesTheOpticalChannel is the same round trip with the frames degraded as a
// camera would degrade them. It is the difference between "the sender renders valid images"
// and "the sender renders images a camera can read".
func TestPipelineSurvivesTheOpticalChannel(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		// A cell size the operating envelope shows is comfortable for a real capture.
		c.Optical.CellPixels = 8
	})
	ctx := context.Background()

	original := testPayload(8<<10, 2)
	_, tx := h.upload(t, "small.bin", original)
	h.prepareAndWait(t, tx.ID)

	frames, err := h.store.Frames.List(ctx, tx.ID)
	require.NoError(t, err)
	require.NotEmpty(t, frames)

	decoded := 0
	for _, record := range frames {
		body, err := objectstore.GetBytes(ctx, h.objects, record.StoredPath, 16<<20)
		require.NoError(t, err)
		img, err := png.Decode(bytes.NewReader(body))
		require.NoError(t, err)

		frame, err := encoding.Decode(simulate.Typical.Apply(img), protocol.LocateOptions{})
		require.NoError(t, err, "frame %d must survive a normal installation", record.FrameNumber)
		require.Equal(t, tx.ID, frame.Header.TransmissionID)
		decoded++
	}
	require.Equal(t, len(frames), decoded)
}

// TestChunkSizeMatchesFrameCapacity pins the calculation the whole pipeline turns on: a chunk
// is sized so that exactly one chunk fits in exactly one frame, with nothing spare.
func TestChunkSizeMatchesFrameCapacity(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	_, tx := h.upload(t, "sized.bin", testPayload(32<<10, 3))
	ready := h.prepareAndWait(t, tx.ID)

	layout, err := protocol.NewLayoutQuiet(ready.GridWidth, ready.GridHeight, ready.CellPixels, ready.QuietZone)
	require.NoError(t, err)
	encoder, err := encoding.ByName(ready.Encoder)
	require.NoError(t, err)
	capacity, err := encoder.EstimateCapacity(layout, uint8(ready.BitDepth))
	require.NoError(t, err)

	require.Equal(t, capacity.PayloadBytes, ready.ChunkSize,
		"an unencrypted chunk should fill the frame exactly")

	// And every chunk actually fits.
	chunks, err := h.store.Chunks.List(ctx, tx.ID)
	require.NoError(t, err)
	for _, c := range chunks {
		require.LessOrEqual(t, c.SizeBytes, capacity.PayloadBytes,
			"chunk %d is larger than a frame carries", c.ESI)
	}
}

// TestEncryptedTransmission covers the confidential path, including that the chunk size
// shrinks to leave room for the nonce and tag — otherwise the last bytes of every chunk would
// not fit in the frame.
//
// The cipher and key are set on the transmission row, not on the process configuration: that
// is where the real API decides them (at transfer creation), and the pipeline itself now reads
// only the row. cfg.Optical.EncryptionKeyHex is deliberately left unset here, so a pipeline
// that still consulted it would leave this test's frames unencrypted rather than pass by
// accident.
func TestEncryptedTransmission(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	keyHex := strings.Repeat("5a", 32)
	key, err := hex.DecodeString(keyHex)
	require.NoError(t, err)

	original := testPayload(16<<10, 4)
	_, tx := h.uploadTuned(t, "secret.bin", original, func(tx *store.Transmission) {
		tx.Encrypted = true
		tx.EncryptionID = int(protocol.EncryptionAES256GCM)
		tx.EncryptionKey = key
	})
	ready := h.prepareAndWait(t, tx.ID)

	layout, err := protocol.NewLayoutQuiet(ready.GridWidth, ready.GridHeight, ready.CellPixels, ready.QuietZone)
	require.NoError(t, err)
	encoder, err := encoding.ByName(ready.Encoder)
	require.NoError(t, err)
	capacity, err := encoder.EstimateCapacity(layout, uint8(ready.BitDepth))
	require.NoError(t, err)
	require.Equal(t, capacity.PayloadBytes-protocol.EncryptionOverhead, ready.ChunkSize,
		"an encrypted chunk must leave room for its nonce and tag")

	frames, err := h.store.Frames.List(ctx, tx.ID)
	require.NoError(t, err)

	recovered := map[int][]byte{}
	for _, record := range frames {
		body, err := objectstore.GetBytes(ctx, h.objects, record.StoredPath, 16<<20)
		require.NoError(t, err)
		img, err := png.Decode(bytes.NewReader(body))
		require.NoError(t, err)

		frame, err := encoding.Decode(img, protocol.LocateOptions{})
		require.NoError(t, err)
		if frame.Header.Flags.Has(protocol.FlagManifest) || frame.Header.Flags.Has(protocol.FlagParity) {
			continue
		}

		require.True(t, frame.Header.Flags.Has(protocol.FlagEncrypted))
		require.Equal(t, uint8(protocol.EncryptionAES256GCM), frame.Header.EncryptionID)

		// Without the key the payload is unreadable, which is the point.
		require.NotContains(t, string(frame.Payload), "the receiver writes")

		payload, err := protocol.OpenFrame([][]byte{key}, frame)
		require.NoError(t, err)
		recovered[int(frame.Header.ChunkNumber)] = payload
	}

	var stream bytes.Buffer
	for esi := 0; esi < len(recovered); esi++ {
		stream.Write(recovered[esi])
	}
	codec, err := compress.ByName(ready.Compression)
	require.NoError(t, err)
	got, err := compress.UnBytes(codec, stream.Bytes(), int64(len(original)))
	require.NoError(t, err)
	require.Equal(t, original, got)
}

// TestPipelineEncryptsPerTransfer covers the cipher choice living on the transmission row
// rather than in process configuration: two transfers on the same sender can use different
// ciphers and keys, so chunkSizeFor and render must read tx.EncryptionID/tx.EncryptionKey
// rather than a single global key.
func TestPipelineEncryptsPerTransfer(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	key := bytes.Repeat([]byte{5}, protocol.KeySize)
	wrongKey := bytes.Repeat([]byte{6}, protocol.KeySize)

	original := testPayload(8<<10, 11)
	_, tx := h.uploadTuned(t, "per-transfer.bin", original, func(tx *store.Transmission) {
		tx.Encrypted = true
		tx.EncryptionID = int(protocol.EncryptionChaCha20Poly1305)
		tx.EncryptionKey = key
	})
	h.prepareAndWait(t, tx.ID)

	frames, err := h.store.Frames.List(ctx, tx.ID)
	require.NoError(t, err)

	dataFrames := 0
	for _, record := range frames {
		body, err := objectstore.GetBytes(ctx, h.objects, record.StoredPath, 16<<20)
		require.NoError(t, err)
		img, err := png.Decode(bytes.NewReader(body))
		require.NoError(t, err)

		frame, err := encoding.Decode(img, protocol.LocateOptions{})
		require.NoError(t, err)
		if frame.Header.Flags.Has(protocol.FlagManifest) || frame.Header.Flags.Has(protocol.FlagParity) {
			continue
		}
		dataFrames++

		require.Equal(t, uint8(protocol.EncryptionChaCha20Poly1305), frame.Header.EncryptionID)
		require.True(t, frame.Header.Flags.Has(protocol.FlagEncrypted))

		payload, err := protocol.OpenFrame([][]byte{key}, frame)
		require.NoError(t, err)
		require.NotEmpty(t, payload)

		_, err = protocol.OpenFrame([][]byte{wrongKey}, frame)
		require.Error(t, err, "the wrong key must not open a frame it did not seal")
	}
	require.Positive(t, dataFrames)
}

// TestPipelineChunksSmallerWhenEncrypted pins the size relationship an encrypted transfer
// depends on: a chunk still has to fit in one frame after it grows by a nonce and a tag, so
// the encrypted chunk size must be exactly that much smaller than the plaintext one — not
// approximately, since a chunk one byte too large would fail to encode at render time rather
// than fail cleanly at chunk time.
func TestPipelineChunksSmallerWhenEncrypted(t *testing.T) {
	h := newHarness(t, nil)

	body := testPayload(8<<10, 12)

	_, plain := h.upload(t, "plain.bin", body)
	plainReady := h.prepareAndWait(t, plain.ID)

	_, encrypted := h.uploadTuned(t, "encrypted.bin", body, func(tx *store.Transmission) {
		tx.Encrypted = true
		tx.EncryptionID = int(protocol.EncryptionChaCha20Poly1305)
		tx.EncryptionKey = bytes.Repeat([]byte{7}, protocol.KeySize)
	})
	encryptedReady := h.prepareAndWait(t, encrypted.ID)

	require.Equal(t, plainReady.ChunkSize-protocol.EncryptionOverhead, encryptedReady.ChunkSize,
		"an encrypted chunk must be smaller by exactly the nonce and tag it carries")
}

// TestPipelineRendersPerTransferGrid checks that the grid a transfer renders at comes from its
// own row, not from whatever the sender process happens to be configured with. Encoding
// profiles are chosen per transfer through the API, and a sender juggling several transfers at
// once must not let one transfer's geometry leak into another's frames.
func TestPipelineRendersPerTransferGrid(t *testing.T) {
	h := newHarness(t, nil) // cfg.Optical.GridWidth/Height are the harness default, 96x96.
	ctx := context.Background()

	_, tx := h.uploadTuned(t, "big-grid.bin", testPayload(4<<10, 13), func(tx *store.Transmission) {
		tx.GridWidth = 192
		tx.GridHeight = 192
	})
	require.NotEqual(t, h.cfg.Optical.GridWidth, tx.GridWidth,
		"the test needs the row and the process config to disagree")
	h.prepareAndWait(t, tx.ID)

	frames, err := h.store.Frames.List(ctx, tx.ID)
	require.NoError(t, err)
	require.NotEmpty(t, frames)

	for _, record := range frames {
		body, err := objectstore.GetBytes(ctx, h.objects, record.StoredPath, 16<<20)
		require.NoError(t, err)
		img, err := png.Decode(bytes.NewReader(body))
		require.NoError(t, err)

		frame, err := encoding.Decode(img, protocol.LocateOptions{})
		require.NoError(t, err, "frame %d must decode", record.FrameNumber)
		require.Equal(t, uint16(192), frame.Header.GridWidth)
		require.Equal(t, uint16(192), frame.Header.GridHeight)
	}
}

// TestParityShardsRecoverLostChunks checks the error correction the pipeline added actually
// works on what it produced — that the shard geometry, the padding, and the identifier numbering
// all line up with what a receiver's decoder expects.
//
// It reconstructs the way a receiver has to: the chunks on the wire are numbered across the whole
// transmission, so each has to be translated into its block and its number inside that block
// before a codec will accept it. That translation is shared code precisely so this test and the
// receiver cannot drift apart.
func TestParityShardsRecoverLostChunks(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Optical.FEC.Codec = "reed-solomon"
		c.Optical.FEC.DataShards = 8
		c.Optical.FEC.ParityShards = 4
		c.Optical.CellPixels = 4
		// Incompressible input, so the chunk count is set by the payload size rather than by how
		// well it happens to compress — several blocks have to exist for blocking to be tested.
		c.Optical.Compression = "none"
	})
	ctx := context.Background()

	_, tx := h.upload(t, "protected.bin", testPayload(48<<10, 5))
	ready := h.prepareAndWait(t, tx.ID)

	chunks, err := h.store.Chunks.List(ctx, tx.ID)
	require.NoError(t, err)

	bodies := map[int][]byte{}
	source, parity := 0, 0
	for _, c := range chunks {
		body, err := objectstore.GetBytes(ctx, h.objects, c.StoredPath, int64(ready.ChunkSize)+1)
		require.NoError(t, err)
		// Shards within a block are equal length, and only the final source chunk is short, so it
		// is padded for coding exactly as the sender padded it. The receiver knows each chunk's
		// real length from the manifest, so the padding never reaches the file.
		padded := make([]byte, ready.ChunkSize)
		copy(padded, body)
		bodies[c.ESI] = padded

		if c.IsParity {
			parity++
		} else {
			source++
		}
	}
	require.Positive(t, parity, "parity shards should have been generated")

	blocking := fec.NewBlocking(source, ready.FECDataShards, ready.FECParityShards)
	require.Greater(t, blocking.Blocks(), 1, "the test needs more than one block to be meaningful")
	require.Equal(t, blocking.Blocks()*ready.FECParityShards, parity)

	// The block assignment the sender recorded must match what the shared arithmetic computes,
	// since the receiver has only the arithmetic.
	for _, c := range chunks {
		var block int
		var err error
		if c.IsParity {
			block, _, err = blocking.ParityShard(c.ESI)
		} else {
			block, _, err = blocking.SourceShard(c.ESI)
		}
		require.NoError(t, err, "chunk %d", c.ESI)
		require.Equal(t, block, c.BlockIndex,
			"chunk %d was recorded in block %d but belongs to %d", c.ESI, c.BlockIndex, block)
	}

	codec, err := fec.ByName(ready.FECCodec)
	require.NoError(t, err)

	// Lose two source shards from every block and rebuild each from its own parity.
	for block := 0; block < blocking.Blocks(); block++ {
		size := blocking.BlockSize(block)
		if size < 3 {
			continue // Nothing to lose two of.
		}

		lost := map[int]bool{0: true, 1: true}
		var received []fec.Shard
		for i := 0; i < size; i++ {
			if lost[i] {
				continue
			}
			chunk, err := blocking.SourceChunk(block, i)
			require.NoError(t, err)
			received = append(received, fec.Shard{ESI: uint32(i), Data: bodies[chunk]})
		}
		for i := 0; i < ready.FECParityShards; i++ {
			chunk, err := blocking.ParityChunk(block, i)
			require.NoError(t, err)
			_, inBlock, err := blocking.ParityShard(chunk)
			require.NoError(t, err)
			received = append(received, fec.Shard{ESI: uint32(inBlock), Data: bodies[chunk]})
		}

		rebuilt, err := codec.Decode(received, size, ready.FECParityShards)
		require.NoError(t, err, "block %d should rebuild from its parity", block)

		for i := range lost {
			chunk, err := blocking.SourceChunk(block, i)
			require.NoError(t, err)
			require.Equal(t, bodies[chunk], rebuilt[i],
				"lost chunk %d (block %d shard %d) should have been reconstructed", chunk, block, i)
		}
	}
}

// TestEmptyFileIsTransmittable covers the degenerate case. A platform that could not send a
// zero-byte file would fail in the field rather than in a test.
func TestEmptyFileIsTransmittable(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		// No parity: a single empty chunk has nothing to code across.
		c.Optical.FEC.Codec = "none"
		c.Optical.FEC.DataShards = 0
		c.Optical.FEC.ParityShards = 0
	})
	ctx := context.Background()

	_, tx := h.upload(t, "empty.bin", nil)
	ready := h.prepareAndWait(t, tx.ID)

	require.Equal(t, 1, ready.ChunkCount, "an empty transmission is one empty chunk")
	require.Positive(t, ready.FrameCount)

	frames, err := h.store.Frames.List(ctx, tx.ID)
	require.NoError(t, err)
	for _, record := range frames {
		body, err := objectstore.GetBytes(ctx, h.objects, record.StoredPath, 16<<20)
		require.NoError(t, err)
		img, err := png.Decode(bytes.NewReader(body))
		require.NoError(t, err)
		_, err = encoding.Decode(img, protocol.LocateOptions{})
		require.NoError(t, err)
	}
}

// TestPipelineIsRestartable covers the reason every stage persists its output: a sender that
// dies mid-pipeline must resume rather than begin again. Re-running the chain over work
// already done has to converge on the same result.
func TestPipelineIsRestartable(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	_, tx := h.upload(t, "resumed.bin", testPayload(16<<10, 6))
	first := h.prepareAndWait(t, tx.ID)

	// Re-run the whole chain, as a reclaimed job would.
	chain, err := h.line.Prepare(ctx, tx.ID)
	require.NoError(t, err)
	require.NotEmpty(t, chain)

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		again, err := h.store.Transmissions.Get(ctx, tx.ID)
		require.NoError(t, err)
		if again.Status == store.TxReady {
			require.Equal(t, first.ChunkSize, again.ChunkSize,
				"a re-run must derive the same chunk size")
			require.Equal(t, first.CompressedSize, again.CompressedSize,
				"compression must be deterministic")
			return
		}
		if again.Status == store.TxFailed {
			t.Fatalf("the re-run failed: %s", again.Error)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the re-run did not finish in time")
}

// TestFailedStageStopsTheChain covers a transmission configured with something impossible.
// The stage that cannot work must fail permanently and take the rest of the chain with it,
// rather than leaving later jobs pending for ever.
func TestFailedStageStopsTheChain(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	_, tx := h.upload(t, "broken.bin", testPayload(4<<10, 7))

	// An encoder no encoder answers to: the configuration validator would have refused this,
	// so the only way to reach it is a row written before a downgrade — exactly the
	// mixed-version case worth covering.
	_, err := h.store.Pool().Exec(ctx,
		`UPDATE transmissions SET encoder = 'holographic' WHERE id = $1`, tx.ID)
	require.NoError(t, err)

	chain, err := h.line.Prepare(ctx, tx.ID)
	require.NoError(t, err)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		last, err := h.jobs.Get(ctx, chain[len(chain)-1].ID)
		require.NoError(t, err)
		if last.Status == jobs.StatusFailed {
			require.Contains(t, last.Error, "depends on")

			// And the stage that actually broke says why.
			all, err := h.jobs.List(ctx, jobs.Filter{TransmissionID: &tx.ID})
			require.NoError(t, err)
			var reasons []string
			for _, job := range all {
				if job.Status == jobs.StatusFailed {
					reasons = append(reasons, job.Type+": "+job.Error)
				}
			}
			require.Contains(t, strings.Join(reasons, "; "), "unknown encoder",
				"the failure should name the real cause: %v", reasons)
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the chain did not fail in time")
}

// TestManifestIsReEmitted checks a receiver joining late can still learn what it needs. For a
// file that takes an hour to display, a manifest sent only once would make a camera that came
// online a minute late useless until the next transmission.
func TestManifestIsReEmitted(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Optical.ManifestInterval = 4
		c.Optical.CellPixels = 4
	})
	ctx := context.Background()

	_, tx := h.upload(t, "long.bin", testPayload(24<<10, 8))
	h.prepareAndWait(t, tx.ID)

	frames, err := h.store.Frames.List(ctx, tx.ID)
	require.NoError(t, err)

	var manifestFrames []int
	for _, f := range frames {
		if f.IsManifest {
			manifestFrames = append(manifestFrames, f.FrameNumber)
		}
	}
	require.Greater(t, len(manifestFrames), 1, "the manifest must be re-emitted, not sent once")
	require.Equal(t, 0, manifestFrames[0], "the first frame is a manifest")

	// The gap between manifests must be bounded by the configured interval, or a late
	// receiver's wait is unbounded in practice.
	for i := 1; i < len(manifestFrames); i++ {
		gap := manifestFrames[i] - manifestFrames[i-1]
		require.LessOrEqual(t, gap, 8, "manifests %d and %d are too far apart",
			manifestFrames[i-1], manifestFrames[i])
	}
}

// TestEveryEncoderRendersDecodably runs the pipeline under each registered encoding, so a new
// encoder cannot be added to the protocol without the sender being able to drive it.
func TestEveryEncoderRendersDecodably(t *testing.T) {
	for _, encoder := range encoding.All() {
		name := encoder.Name()
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) {
				c.Optical.Encoder = name
				c.Optical.BitDepth = 0 // Whichever depth the encoder prefers.
				c.Optical.CellPixels = 4
				c.Optical.FEC.Codec = "none"
				c.Optical.FEC.DataShards = 0
				c.Optical.FEC.ParityShards = 0
			})
			ctx := context.Background()

			_, tx := h.upload(t, fmt.Sprintf("%s.bin", name), testPayload(8<<10, 9))
			h.prepareAndWait(t, tx.ID)

			frames, err := h.store.Frames.List(ctx, tx.ID)
			require.NoError(t, err)
			require.NotEmpty(t, frames)

			for _, record := range frames {
				body, err := objectstore.GetBytes(ctx, h.objects, record.StoredPath, 16<<20)
				require.NoError(t, err)
				img, err := png.Decode(bytes.NewReader(body))
				require.NoError(t, err)

				frame, err := encoding.Decode(img, protocol.LocateOptions{})
				require.NoError(t, err, "%s frame %d must decode", name, record.FrameNumber)
				require.Equal(t, encoder.ID(), frame.Header.EncoderID)
			}
		})
	}
}
