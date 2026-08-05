package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/compress"
	"github.com/opticaltransport/otp/shared/fec"
	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/receiver/internal/store"
)

// Receiver runs the capture-and-decode loop.
type Receiver struct {
	store   *store.Store
	objects objectstore.Store
	source  Source
	cfg     *config.Watcher
	log     *zap.Logger

	// acks is the shared volume both applications reach: the receiver writes acknowledgements into
	// it and the sender reads them out. It is a separate store from the receiver's own because the
	// two have different lifetimes and, in a real deployment, different mount points.
	acks objectstore.Store

	// deliver posts a merged file to a callback URL. It is a field rather than a direct call so a
	// test can observe delivery without standing up a network listener, and so the SSRF checks
	// live in one place.
	deliver *Deliverer

	// session is the capture session frames are attributed to.
	session uuid.UUID

	// started records when the first frame of a transmission was seen, so the result record can
	// report throughput measured rather than estimated.
	started map[uuid.UUID]time.Time

	// finished remembers which transmissions have already been merged and reported, so a
	// retransmission arriving after completion does not restart the whole merge.
	finished map[uuid.UUID]bool
}

// New returns a receiver.
func New(st *store.Store, objects, acks objectstore.Store, source Source, cfg *config.Watcher, log *zap.Logger) *Receiver {
	current := cfg.Current()
	return &Receiver{
		store:    st,
		objects:  objects,
		acks:     acks,
		source:   source,
		cfg:      cfg,
		log:      log.Named("receiver"),
		deliver:  NewDeliverer(current.Callback, log),
		started:  map[uuid.UUID]time.Time{},
		finished: map[uuid.UUID]bool{},
	}
}

// Session is the capture session this receiver is recording under.
func (r *Receiver) Session() uuid.UUID { return r.session }

// Run captures and decodes until the context is done.
func (r *Receiver) Run(ctx context.Context) error {
	session, err := r.store.Sessions.Create(ctx, r.source.Name())
	if err != nil {
		return err
	}
	r.session = session.ID
	r.log.Info("capture session started",
		zap.String("session", session.ID.String()),
		zap.String("source", r.source.Name()))

	defer func() {
		// The session is closed with the context that shut it down rather than the one that is
		// already cancelled, so the closing write actually lands.
		closing := context.WithoutCancel(ctx)
		if err := r.store.Sessions.Finish(closing, session.ID, "stopped", ""); err != nil {
			r.log.Warn("could not close the capture session", zap.Error(err))
		}
	}()

	idle := r.cfg.Current().Capture.IdleInterval
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		capture, err := r.source.Next(ctx)
		switch {
		case errors.Is(err, ErrNoFrame):
			// Nothing on the channel. An idle channel is the normal state between transmissions, so
			// this waits rather than reporting anything.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(idle):
			}
			continue
		case err != nil:
			r.log.Warn("capture failed", zap.Error(err))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(idle):
			}
			continue
		}

		if err := r.handle(ctx, capture); err != nil {
			r.log.Error("could not process a captured frame",
				zap.Int64("sequence", capture.Sequence), zap.Error(err))
		}
	}
}

// handle processes one captured frame.
func (r *Receiver) handle(ctx context.Context, capture Capture) error {
	// Persisted before decoding, always. A frame that cannot be read is the primary evidence for
	// why a capture is going badly, and it is also the only thing a replay can work from.
	key, sum, err := r.storeCapture(ctx, r.session, capture)
	if err != nil {
		return err
	}

	cfg := r.cfg.Current()
	frame, geometry, decodeErr := decodeFrame(capture.Image, cfg.LocateOptions())
	finder, timing, contrast := decodeQuality(geometry)

	// The confidence floors are applied here rather than inside the decoder, because how much
	// confidence is enough is a judgement about this installation rather than a property of the
	// protocol. A frame whose fiducials barely matched may still satisfy its checksums by luck, and
	// an operator watching a marginal camera would rather discard it and have the chunk resent than
	// accept a read nobody is confident in.
	if decodeErr == nil && geometry != nil {
		switch {
		case finder < cfg.Decoder.MinFinderScore:
			decodeErr = fmt.Errorf("fiducial match %.2f is below the %.2f floor",
				finder, cfg.Decoder.MinFinderScore)
			frame = nil
		case timing < cfg.Decoder.MinTimingScore:
			decodeErr = fmt.Errorf("timing match %.2f is below the %.2f floor",
				timing, cfg.Decoder.MinTimingScore)
			frame = nil
		}
	}

	record := store.CapturedFrame{
		SessionID:   r.session,
		Sequence:    capture.Sequence,
		StoredPath:  key,
		SHA256:      sum,
		Decoded:     decodeErr == nil,
		FinderScore: finder,
		TimingScore: timing,
		Contrast:    contrast,
	}

	if decodeErr != nil {
		record.DecodeError = decodeErr.Error()
		if err := r.store.Frames.Record(ctx, record); err != nil {
			return err
		}
		if err := r.store.Sessions.Count(ctx, r.session, 1, 0, 1); err != nil {
			return err
		}

		// A frame that could not be read cannot be acknowledged, because nothing in it says which
		// chunk it was carrying. The sender notices the absence when its acknowledgement times out
		// and sends the chunk again — which is the only mechanism that can work here, and the
		// reason the sender does not wait for a negative acknowledgement it may never get.
		r.log.Debug("frame could not be read", zap.Int64("sequence", capture.Sequence),
			zap.Float64("finder_score", finder), zap.Error(decodeErr))
		return nil
	}

	transmission := frame.Header.TransmissionID
	record.TransmissionID = &transmission
	frameNumber := int64(frame.Header.FrameNumber)
	record.FrameNumber = &frameNumber
	record.IsManifest = frame.Header.Flags.Has(protocol.FlagManifest)
	record.IsParity = frame.Header.Flags.Has(protocol.FlagParity)
	if !record.IsManifest {
		chunkNumber := int64(frame.Header.ChunkNumber)
		record.ChunkNumber = &chunkNumber
	}
	if geometry != nil {
		record.BitErrorRate = bandErrorRate(geometry)
	}

	if err := r.store.Frames.Record(ctx, record); err != nil {
		return err
	}
	if err := r.store.Sessions.Count(ctx, r.session, 1, 1, 0); err != nil {
		return err
	}
	if _, seen := r.started[transmission]; !seen {
		r.started[transmission] = capture.CapturedAt
		if err := r.store.Sessions.Bind(ctx, r.session, transmission); err != nil {
			return err
		}
	}

	if record.IsManifest {
		return r.handleManifest(ctx, frame)
	}
	return r.handleChunk(ctx, frame, record.BitErrorRate)
}

// handleManifest records what a transmission says about itself.
func (r *Receiver) handleManifest(ctx context.Context, frame *protocol.Frame) error {
	manifest, err := protocol.ParseManifest(frame)
	if err != nil {
		// A manifest that will not parse is not fatal: it arrives repeatedly, so the next copy may
		// be readable. What must not happen is acting on a broken one.
		r.log.Warn("manifest frame did not parse", zap.Error(err))
		return nil
	}

	if err := r.store.Manifests.Upsert(ctx, store.Manifest{
		TransmissionID: frame.Header.TransmissionID,
		Filename:       manifest.Filename,
		OriginalSize:   int64(manifest.OriginalSize),
		OriginalSHA256: manifest.OriginalSHA256[:],
		CompressedSize: int64(manifest.CompressedSize),
		ChunkCount:     int(manifest.ChunkCount),
		ChunkSize:      int(manifest.ChunkSize),
		CompressionID:  int(manifest.CompressionID),
		FECID:          int(manifest.FEC.ID),
		FECDataShards:  int(manifest.FEC.DataShards),
		FECParity:      int(manifest.FEC.ParityShards),
		ShardSize:      int(manifest.FEC.ShardSize),
		CallbackURL:    manifest.CallbackURL,
	}); err != nil {
		return err
	}

	r.log.Debug("manifest received",
		zap.String("transmission", frame.Header.TransmissionID.String()),
		zap.String("file", manifest.Filename),
		zap.Uint32("chunks", manifest.ChunkCount))
	return nil
}

// handleChunk stores a chunk and acknowledges it.
//
// The chunk has already been verified twice by the time it arrives here: the frame's footer carries
// a CRC32 and a SHA-256 over the payload, and the decoder refuses a frame that fails either. So a
// chunk reaching this function is intact, and the acknowledgement says so — which is precisely what
// the sender needs to stop resending it.
func (r *Receiver) handleChunk(ctx context.Context, frame *protocol.Frame, errorRate float64) error {
	transmission := frame.Header.TransmissionID
	chunkNumber := int(frame.Header.ChunkNumber)
	isParity := frame.Header.Flags.Has(protocol.FlagParity)

	payload, err := protocol.OpenFrame(r.cfg.Current().EncryptionKey(), frame)
	if err != nil {
		// A payload that will not decrypt is not a channel problem — the frame's own checksums
		// passed — so it is reported rather than acknowledged, and the sender will try again.
		r.log.Warn("chunk did not decrypt", append(zapFrame(frame), zap.Error(err))...)
		return r.emitAck(ctx, frame, protocol.AckCRCFailed, errorRate)
	}

	key := fmt.Sprintf("chunks/%s/%08d.bin", transmission, chunkNumber)
	if err := r.objects.Put(ctx, key, bytes.NewReader(payload)); err != nil {
		return err
	}

	sum := sha256.Sum256(payload)
	inserted, err := r.store.Chunks.Insert(ctx, store.Chunk{
		TransmissionID: transmission,
		ChunkNumber:    chunkNumber,
		IsParity:       isParity,
		SizeBytes:      len(payload),
		CRC32:          int64(crc32.ChecksumIEEE(payload)),
		SHA256:         sum[:],
		StoredPath:     key,
	})
	if err != nil {
		return err
	}

	// A chunk that had already arrived is acknowledged as a duplicate rather than as new. The
	// distinction is what stops the sender resending something the receiver holds: an
	// acknowledgement and a retransmission crossing in flight is routine, and reporting the second
	// arrival as fresh would make the counters lie about how much was actually transferred.
	status := protocol.AckOK
	if !inserted {
		status = protocol.AckDuplicate
	}
	if err := r.emitAck(ctx, frame, status, errorRate); err != nil {
		return err
	}

	if inserted && !isParity {
		return r.maybeComplete(ctx, transmission)
	}
	return nil
}

// emitAck writes a signed acknowledgement to the shared channel.
//
// It is written under a temporary name and renamed into place, because the sender polls the
// directory: without the rename it would eventually read a file mid-write, fail to verify the
// truncated record, and discard an acknowledgement that was perfectly good.
func (r *Receiver) emitAck(ctx context.Context, frame *protocol.Frame, status protocol.AckStatus, errorRate float64) error {
	cfg := r.cfg.Current()
	transmission := frame.Header.TransmissionID

	sequence, err := r.store.Acks.Next(ctx, transmission)
	if err != nil {
		return err
	}

	record := protocol.Ack{
		Sequence:       sequence,
		TransmissionID: transmission,
		SessionID:      r.session,
		FrameNumber:    frame.Header.FrameNumber,
		ChunkNumber:    frame.Header.ChunkNumber,
		Status:         status,
		TimestampMS:    uint64(time.Now().UnixMilli()),
		BitErrorRate:   errorRate,
	}
	data, err := protocol.SignAck([]byte(cfg.Ack.Secret), record)
	if err != nil {
		return err
	}

	return r.writeAck(ctx, protocol.AckPath(transmission, sequence), data)
}

// writeAck writes a record to the shared acknowledgement channel.
//
// No temporary-file dance is needed here because the object store's Put is already atomic in both
// backends — the filesystem one writes beside the target and renames, and an S3 put is not visible
// until it completes. That matters: the sender polls this directory, and a record it read
// half-written would fail to verify and be discarded, losing an acknowledgement that was perfectly
// good.
func (r *Receiver) writeAck(ctx context.Context, key string, data []byte) error {
	return r.acks.Put(ctx, key, bytes.NewReader(data))
}

// maybeComplete merges a transmission once every chunk it needs is available.
func (r *Receiver) maybeComplete(ctx context.Context, transmission uuid.UUID) error {
	if r.finished[transmission] {
		return nil
	}

	manifest, err := r.store.Manifests.Get(ctx, transmission)
	if errors.Is(err, store.ErrNotFound) {
		// Chunks can arrive before the manifest does, since a receiver may have joined mid-stream.
		// They are stored and counted; the merge waits for the manifest that describes them.
		return nil
	}
	if err != nil {
		return err
	}

	have, err := r.store.Chunks.Have(ctx, transmission)
	if err != nil {
		return err
	}

	var missing []int
	for i := 0; i < manifest.ChunkCount; i++ {
		if !have[i] {
			missing = append(missing, i)
		}
	}

	if len(missing) > 0 {
		// Recorded so an operator can see what is outstanding and for how long. Recovery from
		// parity is attempted before giving up, since a chunk rebuilt from parity is a chunk that
		// never has to be sent again.
		if err := r.store.Chunks.SetMissing(ctx, transmission, missing); err != nil {
			return err
		}
		recovered, err := r.recover(ctx, manifest, missing)
		if err != nil {
			r.log.Warn("recovery from parity failed",
				zap.String("transmission", transmission.String()), zap.Error(err))
			return nil
		}
		if recovered == 0 {
			return nil
		}
		// Recovery filled some gaps; ask again with what is now available.
		return r.maybeComplete(ctx, transmission)
	}

	if err := r.store.Chunks.SetMissing(ctx, transmission, nil); err != nil {
		return err
	}
	return r.complete(ctx, manifest)
}

// recover rebuilds missing chunks from parity, and returns how many it recovered.
//
// It works block by block because that is how the parity was generated: each block is coded
// independently, so a block with too many gaps is unrecoverable while its neighbours are fine.
// The block arithmetic comes from the shared package, so the grouping the receiver computes is the
// grouping the sender used rather than a second implementation of the same idea.
func (r *Receiver) recover(ctx context.Context, manifest store.Manifest, missing []int) (int, error) {
	if manifest.FECID == 0 || manifest.FECParity == 0 || manifest.FECDataShards == 0 {
		return 0, nil
	}

	codec, err := fec.ByID(uint8(manifest.FECID))
	if err != nil {
		return 0, err
	}
	blocking := fec.NewBlocking(manifest.ChunkCount, manifest.FECDataShards, manifest.FECParity)

	stored, err := r.store.Chunks.List(ctx, manifest.TransmissionID)
	if err != nil {
		return 0, err
	}
	byNumber := map[int]store.Chunk{}
	for _, c := range stored {
		byNumber[c.ChunkNumber] = c
	}

	// Which blocks have gaps at all: recovering a block that is already complete would be work for
	// nothing.
	gaps := map[int][]int{}
	for _, chunk := range missing {
		block, inBlock, err := blocking.SourceShard(chunk)
		if err != nil {
			return 0, err
		}
		gaps[block] = append(gaps[block], inBlock)
	}

	shardSize := manifest.ShardSize
	if shardSize == 0 {
		shardSize = manifest.ChunkSize
	}

	recovered := 0
	for block, lost := range gaps {
		size := blocking.BlockSize(block)
		if size <= 0 {
			continue
		}

		var received []fec.Shard
		for i := 0; i < size; i++ {
			chunk, err := blocking.SourceChunk(block, i)
			if err != nil {
				return recovered, err
			}
			c, ok := byNumber[chunk]
			if !ok {
				continue
			}
			body, err := r.readObject(ctx, c.StoredPath, int64(shardSize)+1)
			if err != nil {
				return recovered, err
			}
			padded := make([]byte, shardSize)
			copy(padded, body)
			received = append(received, fec.Shard{ESI: uint32(i), Data: padded})
		}
		for i := 0; i < manifest.FECParity; i++ {
			chunk, err := blocking.ParityChunk(block, i)
			if err != nil {
				return recovered, err
			}
			c, ok := byNumber[chunk]
			if !ok {
				continue
			}
			_, inBlock, err := blocking.ParityShard(chunk)
			if err != nil {
				return recovered, err
			}
			body, err := r.readObject(ctx, c.StoredPath, int64(shardSize)+1)
			if err != nil {
				return recovered, err
			}
			received = append(received, fec.Shard{ESI: uint32(inBlock), Data: body})
		}

		if len(received) < codec.ShardsNeeded(size) {
			// Not enough of the block arrived to rebuild it. The sender will resend what is missing;
			// this is not an error, it is the normal state of a transfer in progress.
			continue
		}

		rebuilt, err := codec.Decode(received, size, manifest.FECParity)
		if err != nil {
			continue
		}

		for _, inBlock := range lost {
			chunk, err := blocking.SourceChunk(block, inBlock)
			if err != nil {
				return recovered, err
			}

			// The final chunk of a stream is shorter than a shard, and the manifest is what says by
			// how much: the padding the sender added for coding must not reach the file.
			body := rebuilt[inBlock]
			if length := r.chunkLength(manifest, chunk); length < len(body) {
				body = body[:length]
			}

			key := fmt.Sprintf("chunks/%s/%08d.bin", manifest.TransmissionID, chunk)
			if err := r.objects.Put(ctx, key, bytes.NewReader(body)); err != nil {
				return recovered, err
			}
			sum := sha256.Sum256(body)
			inserted, err := r.store.Chunks.Insert(ctx, store.Chunk{
				TransmissionID: manifest.TransmissionID,
				ChunkNumber:    chunk,
				BlockIndex:     block,
				SizeBytes:      len(body),
				CRC32:          int64(crc32.ChecksumIEEE(body)),
				SHA256:         sum[:],
				StoredPath:     key,
				Recovered:      true,
			})
			if err != nil {
				return recovered, err
			}
			if inserted {
				recovered++
				r.log.Info("chunk recovered from parity",
					zap.String("transmission", manifest.TransmissionID.String()),
					zap.Int("chunk", chunk), zap.Int("block", block))

				// Reported as recovered rather than as received, so the sender stops resending it and
				// an operator can see how much the error correction is earning.
				if err := r.emitRecoveredAck(ctx, manifest.TransmissionID, chunk); err != nil {
					return recovered, err
				}
			}
		}
	}
	return recovered, nil
}

// chunkLength is how long a given source chunk should be, from the manifest alone.
func (r *Receiver) chunkLength(manifest store.Manifest, chunk int) int {
	if manifest.ChunkSize <= 0 {
		return 0
	}
	if chunk < manifest.ChunkCount-1 {
		return manifest.ChunkSize
	}
	// The last chunk holds whatever the stream had left.
	remainder := int(manifest.CompressedSize) - chunk*manifest.ChunkSize
	if remainder < 0 {
		return 0
	}
	if remainder > manifest.ChunkSize {
		return manifest.ChunkSize
	}
	return remainder
}

// emitRecoveredAck tells the sender a chunk was rebuilt rather than received.
func (r *Receiver) emitRecoveredAck(ctx context.Context, transmission uuid.UUID, chunk int) error {
	frame := &protocol.Frame{Header: protocol.Header{
		TransmissionID: transmission,
		ChunkNumber:    uint32(chunk),
	}}
	return r.emitAck(ctx, frame, protocol.AckRecovered, 0)
}

// complete merges, verifies, delivers, and reports.
//
// The order is the whole point of the receiver. Merge, then verify against the hash the manifest
// declared, and only then deliver — a receiver that posted an unverified file would turn a silent
// optical error into corrupt data in somebody else's system. The report to the sender comes last,
// and carries what actually happened rather than an assumption.
func (r *Receiver) complete(ctx context.Context, manifest store.Manifest) error {
	transmission := manifest.TransmissionID
	if r.finished[transmission] {
		return nil
	}

	merged, err := r.merge(ctx, manifest)
	if err != nil {
		return err
	}

	arrived, recovered, err := r.store.Chunks.Counts(ctx, transmission)
	if err != nil {
		return err
	}

	session, err := r.store.Sessions.Get(ctx, r.session)
	if err != nil {
		return err
	}

	result := protocol.Result{
		TransmissionID:  transmission,
		Filename:        manifest.Filename,
		Size:            uint64(merged.SizeBytes),
		SHA256:          hex.EncodeToString(merged.SHA256),
		Verified:        merged.Verified,
		Error:           merged.VerifyError,
		ChunksExpected:  uint32(manifest.ChunkCount),
		ChunksReceived:  uint32(arrived),
		ChunksRecovered: uint32(recovered),
		FramesCaptured:  uint64(session.FramesCaptured),
		FramesFailed:    uint64(session.FramesFailed),
		StartedMS:       uint64(r.startedAt(transmission).UnixMilli()),
		CompletedMS:     uint64(time.Now().UnixMilli()),
		CallbackURL:     manifest.CallbackURL,
	}

	if !merged.Verified {
		// A file that did not match its hash is not delivered anywhere. The failure is reported to
		// the sender instead, which is the only party that can do anything about it.
		r.log.Error("merged file did not match the manifest",
			zap.String("transmission", transmission.String()),
			zap.String("error", merged.VerifyError))
	} else if manifest.CallbackURL != "" {
		status, deliverErr := r.deliverFile(ctx, manifest, merged)
		result.CallbackStatus = status
		result.CallbackDelivered = deliverErr == nil
		if deliverErr != nil {
			result.CallbackError = deliverErr.Error()
			r.log.Error("could not deliver the merged file",
				zap.String("transmission", transmission.String()),
				zap.String("url", manifest.CallbackURL), zap.Error(deliverErr))
		} else {
			r.log.Info("merged file delivered",
				zap.String("transmission", transmission.String()),
				zap.String("url", manifest.CallbackURL), zap.Int("status", status))
		}
	}

	if err := r.report(ctx, result); err != nil {
		return err
	}

	r.finished[transmission] = true
	r.log.Info("transmission complete",
		zap.String("transmission", transmission.String()),
		zap.String("file", manifest.Filename),
		zap.Int64("bytes", merged.SizeBytes),
		zap.Bool("verified", merged.Verified),
		zap.Int("chunks", arrived),
		zap.Int("recovered", recovered),
		zap.Duration("took", result.Duration()),
		zap.Float64("bytes_per_second", result.ThroughputBytesPerSecond()))

	if err := r.store.Stats.Record(ctx, "throughput_bytes_per_second",
		result.ThroughputBytesPerSecond(), &transmission); err != nil {
		r.log.Warn("could not record throughput", zap.Error(err))
	}
	return nil
}

// merge reassembles the file and checks it against the manifest.
func (r *Receiver) merge(ctx context.Context, manifest store.Manifest) (store.MergedFile, error) {
	transmission := manifest.TransmissionID

	stored, err := r.store.Chunks.List(ctx, transmission)
	if err != nil {
		return store.MergedFile{}, err
	}
	byNumber := map[int]store.Chunk{}
	for _, c := range stored {
		if !c.IsParity {
			byNumber[c.ChunkNumber] = c
		}
	}

	// The compressed stream is reassembled in chunk order, and only the source chunks take part:
	// parity shards are scaffolding and never appear in the file.
	var stream bytes.Buffer
	for i := 0; i < manifest.ChunkCount; i++ {
		c, ok := byNumber[i]
		if !ok {
			return store.MergedFile{}, fmt.Errorf("pipeline: chunk %d is missing at merge time", i)
		}
		body, err := r.readObject(ctx, c.StoredPath, int64(manifest.ChunkSize)+1)
		if err != nil {
			return store.MergedFile{}, err
		}
		stream.Write(body)
	}

	if int64(stream.Len()) != manifest.CompressedSize {
		// The lengths disagreeing means the chunking and the manifest do not describe the same
		// stream, which decompression would report as corruption further along and less clearly.
		return r.recordMerge(ctx, manifest, nil, fmt.Sprintf(
			"reassembled %d bytes, the manifest declares %d", stream.Len(), manifest.CompressedSize))
	}

	codec, err := compress.ByID(uint8(manifest.CompressionID))
	if err != nil {
		return r.recordMerge(ctx, manifest, nil, err.Error())
	}

	// The manifest's original size bounds the decompression. It is not an optimisation: the stream
	// came from outside this process's trust boundary, and every codec here can express a small
	// input that expands without limit.
	body, err := compress.UnBytes(codec, stream.Bytes(), manifest.OriginalSize)
	if err != nil {
		return r.recordMerge(ctx, manifest, nil, err.Error())
	}

	if int64(len(body)) != manifest.OriginalSize {
		return r.recordMerge(ctx, manifest, body, fmt.Sprintf(
			"decompressed to %d bytes, the manifest declares %d", len(body), manifest.OriginalSize))
	}

	// The hash is the transmission's actual success criterion. Everything before it — per-frame
	// checksums, per-chunk checksums — can pass while the file is still wrong, because they only
	// say each piece arrived as sent, not that the pieces were the right pieces in the right order.
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], manifest.OriginalSHA256) {
		return r.recordMerge(ctx, manifest, body, fmt.Sprintf(
			"merged file hashes to %s, the manifest declares %s",
			hex.EncodeToString(sum[:]), hex.EncodeToString(manifest.OriginalSHA256)))
	}
	return r.recordMerge(ctx, manifest, body, "")
}

// recordMerge stores a merge outcome, keeping the file even when it failed verification.
func (r *Receiver) recordMerge(ctx context.Context, manifest store.Manifest, body []byte, verifyError string) (store.MergedFile, error) {
	key := fmt.Sprintf("merged/%s/%s", manifest.TransmissionID, manifest.Filename)
	if body != nil {
		if err := r.objects.Put(ctx, key, bytes.NewReader(body)); err != nil {
			return store.MergedFile{}, err
		}
	}

	sum := sha256.Sum256(body)
	return r.store.Merged.Upsert(ctx, store.MergedFile{
		TransmissionID: manifest.TransmissionID,
		Filename:       manifest.Filename,
		StoredPath:     key,
		SizeBytes:      int64(len(body)),
		SHA256:         sum[:],
		Verified:       verifyError == "",
		VerifyError:    verifyError,
	})
}

// deliverFile posts the merged file to the callback URL and records the attempt.
func (r *Receiver) deliverFile(ctx context.Context, manifest store.Manifest, merged store.MergedFile) (int, error) {
	body, err := r.readObject(ctx, merged.StoredPath, merged.SizeBytes+1)
	if err != nil {
		return 0, err
	}

	callback, err := r.store.Callbacks.Enqueue(ctx, store.Callback{
		TransmissionID: &manifest.TransmissionID,
		URL:            manifest.CallbackURL,
		Event:          "file.delivered",
	})
	if err != nil {
		return 0, err
	}

	status, err := r.deliver.Post(ctx, manifest.CallbackURL, Delivery{
		TransmissionID: manifest.TransmissionID,
		Filename:       manifest.Filename,
		SHA256:         hex.EncodeToString(merged.SHA256),
		Body:           body,
	})
	if err != nil {
		if recordErr := r.store.Callbacks.Retry(ctx, callback.ID, statusPointer(status),
			err.Error(), r.cfg.Current().Callback.RetryDelay); recordErr != nil {
			r.log.Warn("could not record the delivery attempt", zap.Error(recordErr))
		}
		return status, err
	}
	if recordErr := r.store.Callbacks.Delivered(ctx, callback.ID, status); recordErr != nil {
		r.log.Warn("could not record the delivery", zap.Error(recordErr))
	}
	return status, nil
}

func statusPointer(status int) *int {
	if status == 0 {
		return nil
	}
	return &status
}

// report writes the receiver's verdict to the acknowledgement channel.
func (r *Receiver) report(ctx context.Context, result protocol.Result) error {
	data, err := protocol.SignResult([]byte(r.cfg.Current().Ack.Secret), result)
	if err != nil {
		return err
	}
	return r.writeAck(ctx, protocol.ResultPath(result.TransmissionID), data)
}

func (r *Receiver) startedAt(transmission uuid.UUID) time.Time {
	if at, ok := r.started[transmission]; ok {
		return at
	}
	return time.Now()
}

// bandErrorRate is the fraction of band bits the decoder had to repair by majority vote.
func bandErrorRate(g *protocol.Geometry) float64 {
	if g == nil {
		return 0
	}
	return g.BandErrorRate
}
