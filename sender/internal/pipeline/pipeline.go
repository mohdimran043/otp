// Package pipeline turns an uploaded file into displayable frames.
//
// The stages are compress, chunk, error-code, and render, each a registered job type, and the
// order is not arbitrary. Compression comes first because it is the only stage that reduces
// the work every later stage does — the channel carries a fixed number of bytes per frame, so
// halving the byte count halves the transmission. Chunking comes after it because the chunk
// size is derived from what one frame can carry, which means chunks can only be cut from the
// stream that will actually be sent. Error coding comes after chunking because it works on
// whole shards. Rendering comes last because a frame is one chunk, and until the chunks exist
// there is nothing to render.
//
// Every stage writes its output to the object store and its bookkeeping to Postgres, and
// nothing is passed between stages in memory. That is what makes the pipeline restartable: a
// sender that dies during rendering has its compressed stream and its chunks still on disk,
// and picks up where it stopped rather than starting a fifty-gigabyte file again.
package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash/crc32"
	"image/png"
	"io"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/compress"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/fec"
	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Job types, which are wire values in the sense that matters here: they are written into job
// rows that outlive a deployment, so renaming one strands whatever was already queued.
const (
	TypeCompress = "compress"
	TypeChunk    = "chunk"
	TypeFEC      = "fec_encode"
	TypeRender   = "frame_generate"
	TypeFinalize = "prepare_finalize"
)

// Pipeline holds what every stage needs.
type Pipeline struct {
	store   *store.Store
	jobs    *jobs.Store
	objects objectstore.Store
	cfg     *config.Watcher
	log     *zap.Logger
}

// New returns a pipeline.
func New(st *store.Store, js *jobs.Store, objects objectstore.Store, cfg *config.Watcher, log *zap.Logger) *Pipeline {
	return &Pipeline{store: st, jobs: js, objects: objects, cfg: cfg, log: log.Named("pipeline")}
}

// Register adds every stage to an engine.
func (p *Pipeline) Register(engine *jobs.Engine) {
	engine.Register(jobs.HandlerFunc{JobType: TypeCompress, Fn: p.compress})
	engine.Register(jobs.HandlerFunc{JobType: TypeChunk, Fn: p.chunk})
	engine.Register(jobs.HandlerFunc{JobType: TypeFEC, Fn: p.fecEncode})
	engine.Register(jobs.HandlerFunc{JobType: TypeRender, Fn: p.render})
	engine.Register(jobs.HandlerFunc{JobType: TypeFinalize, Fn: p.finalize})
}

// Prepare enqueues the whole chain for a transmission and marks it as being prepared.
func (p *Pipeline) Prepare(ctx context.Context, transmissionID uuid.UUID) ([]jobs.Job, error) {
	tx, err := p.store.Transmissions.Get(ctx, transmissionID)
	if err != nil {
		return nil, err
	}
	if err := p.store.Transmissions.SetStatus(ctx, transmissionID, store.TxPreparing, ""); err != nil {
		return nil, err
	}

	attempts := p.cfg.Current().Jobs.MaxAttempts
	id := transmissionID
	specs := []jobs.Spec{
		{Type: TypeCompress, TransmissionID: &id, FileID: &tx.FileID},
		{Type: TypeChunk, TransmissionID: &id, FileID: &tx.FileID},
		{Type: TypeFEC, TransmissionID: &id, FileID: &tx.FileID},
		{Type: TypeRender, TransmissionID: &id, FileID: &tx.FileID},
		{Type: TypeFinalize, TransmissionID: &id, FileID: &tx.FileID},
	}
	return p.jobs.EnqueueChain(ctx, specs, attempts)
}

// transmissionOf loads the job's transmission, failing permanently if it has gone.
//
// A job whose transmission was deleted cannot be made to work by retrying it, so it is
// reported as permanent rather than spending five attempts discovering the same thing.
func (p *Pipeline) transmissionOf(ctx context.Context, jc *jobs.Context) (store.Transmission, error) {
	if jc.Job.TransmissionID == nil {
		return store.Transmission{}, jobs.Permanent(fmt.Errorf("pipeline: %s has no transmission", jc.Job.Type))
	}
	tx, err := p.store.Transmissions.Get(ctx, *jc.Job.TransmissionID)
	if err != nil {
		return store.Transmission{}, jobs.Permanent(err)
	}
	return tx, nil
}

// Object keys. They are built through objectstore.Key so the identifiers cannot compose into
// something that escapes its namespace.
func uploadKey(fileID uuid.UUID) (string, error) {
	return objectstore.Key("files", fileID.String())
}

func compressedKey(txID uuid.UUID) (string, error) {
	return objectstore.Key("transmissions", txID.String(), "compressed.bin")
}

func chunkKey(txID uuid.UUID, esi int) (string, error) {
	return objectstore.Key("transmissions", txID.String(), "chunks", fmt.Sprintf("%08d.bin", esi))
}

func frameKey(txID uuid.UUID, frame int) (string, error) {
	return objectstore.Key("transmissions", txID.String(), "frames", fmt.Sprintf("%08d.png", frame))
}

// compress runs the uploaded file through the configured compressor.
//
// It streams rather than buffering, which is the whole reason the compressor interface is
// stream-shaped: the files this platform exists for do not fit in memory, and a stage that
// read one into a slice would fail on exactly the uploads it was built to handle.
func (p *Pipeline) compress(ctx context.Context, jc *jobs.Context) error {
	tx, err := p.transmissionOf(ctx, jc)
	if err != nil {
		return err
	}

	codec, err := compress.ByName(tx.Compression)
	if err != nil {
		return jobs.Permanent(err)
	}

	source, err := uploadKey(tx.FileID)
	if err != nil {
		return jobs.Permanent(err)
	}
	destination, err := compressedKey(tx.ID)
	if err != nil {
		return jobs.Permanent(err)
	}

	jc.Progress(ctx, 5, "compressing with %s", codec.Name())

	reader, err := p.objects.Get(ctx, source)
	if err != nil {
		return err
	}
	defer reader.Close()

	// The compressed stream is written through a pipe so that neither side is buffered
	// whole: the compressor writes as the store reads.
	pr, pw := io.Pipe()
	go func() {
		err := codec.Compress(pw, reader, tx.CompressionLevel)
		pw.CloseWithError(err)
	}()

	if err := p.objects.Put(ctx, destination, pr); err != nil {
		return err
	}

	info, err := p.objects.Stat(ctx, destination)
	if err != nil {
		return err
	}

	original := tx.OriginalSize
	if original == 0 {
		if file, err := p.store.Files.Get(ctx, tx.FileID); err == nil {
			original = file.SizeBytes
		}
	}
	ratio := 1.0
	if original > 0 {
		ratio = float64(info.Size) / float64(original)
	}

	jc.Infof(ctx, "%s compressed %d bytes to %d (%.1f%%)",
		codec.Name(), original, info.Size, ratio*100)
	if err := p.store.Stats.Record(ctx, "compression_ratio", ratio, &tx.ID); err != nil {
		jc.Warnf(ctx, "could not record the compression ratio: %s", err)
	}

	// The chunk count is not known yet, so only the size is recorded here; the chunking
	// stage fills in the rest.
	return p.store.Transmissions.SetSizes(ctx, tx.ID, info.Size, tx.ChunkSize, 0)
}

// chunkSizeFor derives how many payload bytes one frame carries at a transmission's geometry.
//
// This is the calculation that ties the pipeline to the protocol: a chunk is sized so that
// exactly one chunk fits in exactly one frame. Deriving it rather than configuring it means
// an operator who changes the grid or the encoder gets chunks that still fit, instead of a
// transmission that fails at the first render.
func chunkSizeFor(tx store.Transmission, key []byte) (int, protocol.Layout, encoding.Encoder, error) {
	layout, err := protocol.NewLayoutQuiet(tx.GridWidth, tx.GridHeight, tx.CellPixels, tx.QuietZone)
	if err != nil {
		return 0, protocol.Layout{}, nil, err
	}
	encoder, err := encoding.ByName(tx.Encoder)
	if err != nil {
		return 0, protocol.Layout{}, nil, err
	}
	capacity, err := encoder.EstimateCapacity(layout, uint8(tx.BitDepth))
	if err != nil {
		return 0, protocol.Layout{}, nil, err
	}

	size := capacity.PayloadBytes
	if len(key) > 0 {
		// Encryption adds a nonce and a tag to every payload, and the sum still has to fit in
		// one frame — so the chunk has to be smaller by exactly that much.
		size -= protocol.EncryptionOverhead
	}
	if size <= 0 {
		return 0, protocol.Layout{}, nil, fmt.Errorf(
			"pipeline: a %dx%d grid carries %d payload bytes, too few to chunk into",
			tx.GridWidth, tx.GridHeight, capacity.PayloadBytes)
	}
	return size, layout, encoder, nil
}

// chunk cuts the compressed stream into frame-sized pieces.
func (p *Pipeline) chunk(ctx context.Context, jc *jobs.Context) error {
	tx, err := p.transmissionOf(ctx, jc)
	if err != nil {
		return err
	}
	cfg := p.cfg.Current()

	size, _, _, err := chunkSizeFor(tx, cfg.EncryptionKey())
	if err != nil {
		return jobs.Permanent(err)
	}

	source, err := compressedKey(tx.ID)
	if err != nil {
		return jobs.Permanent(err)
	}
	reader, err := p.objects.Get(ctx, source)
	if err != nil {
		return err
	}
	defer reader.Close()

	jc.Progress(ctx, 10, "cutting %d-byte chunks", size)

	// Whatever a previous attempt wrote is removed first. Every stage here has to be safe to
	// run twice — a job is retried on any transient failure and reclaimed if its worker dies —
	// and this stage inserts rows keyed by an identifier it assigns, so without this a retry
	// fails on the unique constraint and takes the whole transmission with it. Deleting rather
	// than upserting also handles the case where the previous run produced *more* chunks than
	// this one will, which an upsert would leave behind.
	if err := p.store.Chunks.DeleteFor(ctx, tx.ID); err != nil {
		return err
	}

	var chunks []store.Chunk
	buf := make([]byte, size)
	esi := 0
	total := int64(0)

	for {
		// ReadFull rather than Read, so a short read from the store does not produce a chunk
		// smaller than the rest of the stream. Only the final chunk may be short.
		n, err := io.ReadFull(reader, buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			key, keyErr := chunkKey(tx.ID, esi)
			if keyErr != nil {
				return jobs.Permanent(keyErr)
			}
			if err := p.objects.Put(ctx, key, bytes.NewReader(data)); err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			// The block a chunk belongs to is recorded here rather than left for the error-coding
			// stage, because the receiver needs it for source chunks too: it has to group its
			// arrivals into blocks before any of them can be handed to a decoder.
			block := 0
			if tx.FECDataShards > 0 {
				block = esi / tx.FECDataShards
			}
			chunks = append(chunks, store.Chunk{
				ID:             uuid.New(),
				TransmissionID: tx.ID,
				ESI:            esi,
				BlockIndex:     block,
				SizeBytes:      n,
				CRC32:          int64(crc32.ChecksumIEEE(data)),
				SHA256:         sum[:],
				StoredPath:     key,
			})
			esi++
			total += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		if esi%256 == 0 {
			jc.Progress(ctx, 10, "cut %d chunks", esi)
		}
	}

	if len(chunks) == 0 {
		// An empty file is a legitimate thing to send, and it is one empty chunk: a
		// transmission with no chunks at all would have nothing for the receiver to
		// acknowledge and would never complete.
		key, err := chunkKey(tx.ID, 0)
		if err != nil {
			return jobs.Permanent(err)
		}
		if err := p.objects.Put(ctx, key, bytes.NewReader(nil)); err != nil {
			return err
		}
		sum := sha256.Sum256(nil)
		chunks = append(chunks, store.Chunk{
			ID:             uuid.New(),
			TransmissionID: tx.ID,
			ESI:            0,
			SizeBytes:      0,
			CRC32:          int64(crc32.ChecksumIEEE(nil)),
			SHA256:         sum[:],
			StoredPath:     key,
		})
	}

	if err := p.store.Chunks.InsertMany(ctx, chunks); err != nil {
		return err
	}
	if err := p.store.Transmissions.SetSizes(ctx, tx.ID, total, size, len(chunks)); err != nil {
		return err
	}

	jc.Infof(ctx, "cut %d bytes into %d chunks of %d", total, len(chunks), size)
	return nil
}

// fecEncode adds parity shards.
//
// The chunks are grouped into blocks of the configured source-shard count and each block is
// coded independently, which is what keeps the work bounded: one block's parity depends only
// on that block, so a transmission of ten thousand chunks does not need a ten-thousand-shard
// code. The parity shards take identifiers after the source ones, in the numbering the
// receiver's decoder expects.
func (p *Pipeline) fecEncode(ctx context.Context, jc *jobs.Context) error {
	tx, err := p.transmissionOf(ctx, jc)
	if err != nil {
		return err
	}

	codec, err := fec.ByName(tx.FECCodec)
	if err != nil {
		return jobs.Permanent(err)
	}
	if tx.FECParityShards == 0 || codec.ID() == fec.IDNone {
		jc.Infof(ctx, "no error correction configured, nothing to add")
		return nil
	}
	if err := codec.Validate(tx.FECDataShards, tx.FECParityShards); err != nil {
		return jobs.Permanent(err)
	}

	// A retried attempt must not treat its own parity as source data, so its output is cleared
	// before its input is read.
	if err := p.store.Chunks.DeleteParityFor(ctx, tx.ID); err != nil {
		return err
	}

	source, err := p.store.Chunks.List(ctx, tx.ID)
	if err != nil {
		return err
	}
	var data []store.Chunk
	for _, c := range source {
		if !c.IsParity {
			data = append(data, c)
		}
	}
	if len(data) == 0 {
		return jobs.Permanent(fmt.Errorf("pipeline: no chunks to error-code"))
	}

	// Shards within a block must be the same length, and only the final chunk of the stream is
	// short — so it is padded up for the purpose of coding. The receiver knows each chunk's real
	// length from its own row, so the padding never reaches the file.
	shardSize := tx.ChunkSize
	if shardSize <= 0 {
		for _, c := range data {
			if c.SizeBytes > shardSize {
				shardSize = c.SizeBytes
			}
		}
	}

	// The numbering comes from the shared Blocking type rather than a local counter, so the
	// identifiers the receiver computes and the ones the sender assigns are the same arithmetic
	// rather than two implementations of it.
	blocking := fec.NewBlocking(len(data), tx.FECDataShards, tx.FECParityShards)
	var parity []store.Chunk

	for blockIndex := 0; blockIndex < blocking.Blocks(); blockIndex++ {
		start := blockIndex * tx.FECDataShards
		end := start + blocking.BlockSize(blockIndex)
		block := data[start:end]

		shards := make([][]byte, len(block))
		for i, c := range block {
			body, err := objectstore.GetBytes(ctx, p.objects, c.StoredPath, int64(shardSize)+1)
			if err != nil {
				return err
			}
			padded := make([]byte, shardSize)
			copy(padded, body)
			shards[i] = padded
		}

		repair, err := codec.Encode(shards, tx.FECParityShards)
		if err != nil {
			return err
		}
		for i, shard := range repair {
			esi, err := blocking.ParityChunk(blockIndex, i)
			if err != nil {
				return jobs.Permanent(err)
			}
			key, err := chunkKey(tx.ID, esi)
			if err != nil {
				return jobs.Permanent(err)
			}
			if err := p.objects.Put(ctx, key, bytes.NewReader(shard)); err != nil {
				return err
			}
			sum := sha256.Sum256(shard)
			parity = append(parity, store.Chunk{
				ID:             uuid.New(),
				TransmissionID: tx.ID,
				ESI:            esi,
				BlockIndex:     blockIndex,
				IsParity:       true,
				SizeBytes:      len(shard),
				CRC32:          int64(crc32.ChecksumIEEE(shard)),
				SHA256:         sum[:],
				StoredPath:     key,
			})
		}
		jc.Progress(ctx, 10+40*end/len(data), "error-coded %d of %d chunks", end, len(data))
	}

	if err := p.store.Chunks.InsertMany(ctx, parity); err != nil {
		return err
	}
	jc.Infof(ctx, "%s added %d parity shards across %d blocks",
		codec.Name(), len(parity), blocking.Blocks())
	return nil
}

// render draws every chunk as a frame, and re-emits the manifest periodically.
func (p *Pipeline) render(ctx context.Context, jc *jobs.Context) error {
	tx, err := p.transmissionOf(ctx, jc)
	if err != nil {
		return err
	}
	cfg := p.cfg.Current()
	key := cfg.EncryptionKey()

	_, layout, encoder, err := chunkSizeFor(tx, key)
	if err != nil {
		return jobs.Permanent(err)
	}

	file, err := p.store.Files.Get(ctx, tx.FileID)
	if err != nil {
		return jobs.Permanent(err)
	}
	chunks, err := p.store.Chunks.List(ctx, tx.ID)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return jobs.Permanent(fmt.Errorf("pipeline: nothing to render"))
	}

	sourceChunks := 0
	for _, c := range chunks {
		if !c.IsParity {
			sourceChunks++
		}
	}

	manifest := protocol.Manifest{
		Filename:       file.Filename,
		OriginalSize:   uint64(file.SizeBytes),
		OriginalSHA256: [32]byte(file.SHA256),
		CompressedSize: uint64(tx.CompressedSize),
		ChunkCount:     uint32(sourceChunks),
		ChunkSize:      uint32(tx.ChunkSize),
		CompressionID:  compressionID(tx.Compression),
		FEC: protocol.FECParams{
			ID:           fecID(tx.FECCodec),
			DataShards:   uint16(tx.FECDataShards),
			ParityShards: uint16(tx.FECParityShards),
			ShardSize:    uint32(tx.ChunkSize),
		},
		// The URL a caller supplied on the API. It rides in the manifest because the receiver is the
		// side that ends up with a merged, verified file to deliver, and the optical channel is the
		// only path from here to there.
		CallbackURL: tx.CallbackURL,
	}
	if err := manifest.Validate(); err != nil {
		return jobs.Permanent(err)
	}

	// As with the other stages, a previous attempt's frames are replaced rather than added to.
	if err := p.store.Frames.DeleteFor(ctx, tx.ID); err != nil {
		return err
	}

	sessionID := uuid.New()
	interval := cfg.Optical.ManifestInterval
	frameNumber := 0
	var frames []store.Frame

	// The manifest is re-emitted every N frames rather than only sent first, so a receiver
	// whose camera came online mid-transmission can join the stream instead of waiting for
	// the next one. For a file that takes an hour to display, that is the difference between
	// a working installation and an unusable one.
	emitManifest := func() error {
		header := protocol.Header{
			TransmissionID: tx.ID,
			SessionID:      sessionID,
			FrameNumber:    uint32(frameNumber),
			TotalChunks:    uint32(sourceChunks),
		}
		frame, err := protocol.NewManifestFrame(header, manifest)
		if err != nil {
			return jobs.Permanent(err)
		}
		record, err := p.renderFrame(ctx, encoder, layout, tx, frame, frameNumber, nil, true)
		if err != nil {
			return err
		}
		frames = append(frames, record)
		frameNumber++
		return nil
	}

	if err := emitManifest(); err != nil {
		return err
	}

	for i, c := range chunks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if interval > 0 && frameNumber%interval == 0 {
			if err := emitManifest(); err != nil {
				return err
			}
		}

		payload, err := objectstore.GetBytes(ctx, p.objects, c.StoredPath, int64(tx.ChunkSize)+1)
		if err != nil {
			return err
		}

		flags := protocol.Flags(0)
		if c.IsParity {
			flags |= protocol.FlagParity
		}
		if i == len(chunks)-1 {
			flags |= protocol.FlagEndOfStream
		}
		if !c.IsParity && c.ESI == sourceChunks-1 {
			flags |= protocol.FlagLastChunk
		}

		header := protocol.Header{
			Flags:          flags,
			CompressionID:  manifest.CompressionID,
			FECID:          manifest.FEC.ID,
			TransmissionID: tx.ID,
			SessionID:      sessionID,
			FrameNumber:    uint32(frameNumber),
			ChunkNumber:    uint32(c.ESI),
			TotalChunks:    uint32(sourceChunks),
		}

		var frame *protocol.Frame
		if len(key) > 0 {
			frame, err = protocol.NewEncryptedFrame(key, protocol.EncryptionNone, header, payload)
			if err != nil {
				return jobs.Permanent(err)
			}
		} else {
			frame = protocol.NewFrame(header, payload)
		}

		chunkID := c.ID
		record, err := p.renderFrame(ctx, encoder, layout, tx, frame, frameNumber, &chunkID, false)
		if err != nil {
			return err
		}
		frames = append(frames, record)
		frameNumber++

		if i%64 == 0 {
			jc.Progress(ctx, 50+45*i/len(chunks), "rendered %d of %d frames", frameNumber, len(chunks))
		}
	}

	if err := p.store.Frames.InsertMany(ctx, frames); err != nil {
		return err
	}
	if err := p.store.Transmissions.SetFrameCount(ctx, tx.ID, len(frames)); err != nil {
		return err
	}
	jc.Infof(ctx, "rendered %d frames at %dx%d pixels",
		len(frames), layout.ImageWidth(), layout.ImageHeight())
	return nil
}

// renderFrame encodes one frame and stores its image.
func (p *Pipeline) renderFrame(ctx context.Context, encoder encoding.Encoder, layout protocol.Layout,
	tx store.Transmission, frame *protocol.Frame, number int, chunkID *uuid.UUID, isManifest bool,
) (store.Frame, error) {
	img, err := encoder.Encode(frame, layout, uint8(tx.BitDepth))
	if err != nil {
		// A frame that will not encode at this geometry will not encode on a retry either.
		return store.Frame{}, jobs.Permanent(err)
	}

	// PNG rather than a lossy format, because the receiver's decoder is being asked to read
	// individual cells: JPEG artefacts around a cell boundary are exactly the damage the
	// optical channel already contributes, and there is no reason to add more before the
	// frame has even been displayed.
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return store.Frame{}, err
	}

	key, err := frameKey(tx.ID, number)
	if err != nil {
		return store.Frame{}, jobs.Permanent(err)
	}
	if err := p.objects.Put(ctx, key, bytes.NewReader(buf.Bytes())); err != nil {
		return store.Frame{}, err
	}

	sum := sha256.Sum256(buf.Bytes())
	return store.Frame{
		ID:             uuid.New(),
		TransmissionID: tx.ID,
		ChunkID:        chunkID,
		FrameNumber:    number,
		IsManifest:     isManifest,
		Flags:          int(frame.Header.Flags),
		WidthPx:        layout.ImageWidth(),
		HeightPx:       layout.ImageHeight(),
		PayloadBytes:   len(frame.Payload),
		StoredPath:     key,
		SHA256:         sum[:],
	}, nil
}

// finalize marks a transmission ready to display.
func (p *Pipeline) finalize(ctx context.Context, jc *jobs.Context) error {
	tx, err := p.transmissionOf(ctx, jc)
	if err != nil {
		return err
	}
	if tx.FrameCount == 0 {
		return jobs.Permanent(fmt.Errorf("pipeline: no frames were rendered"))
	}

	if err := p.store.Transmissions.SetStatus(ctx, tx.ID, store.TxReady, ""); err != nil {
		return err
	}
	jc.Progress(ctx, 100, "ready to display")
	jc.Infof(ctx, "%d frames ready for %d chunks", tx.FrameCount, tx.ChunkCount)
	return nil
}

// compressionID and fecID translate a configuration name into the wire value the manifest
// carries. The lookup goes through the registries so the two cannot disagree.
func compressionID(name string) uint8 {
	if codec, err := compress.ByName(name); err == nil {
		return codec.ID()
	}
	return compress.IDNone
}

func fecID(name string) uint8 {
	if codec, err := fec.ByName(name); err == nil {
		return codec.ID()
	}
	return fec.IDNone
}
