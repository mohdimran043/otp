package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"runtime"
	"sync"
	"sync/atomic"
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

	// keys caches the decryption key ring. The applier consults it for every encrypted
	// frame, and a database read per frame at 25 fps is a self-inflicted load problem —
	// so it is refreshed at most every few seconds, only ever from the single-threaded
	// applier, and a key added on the settings page is in use within one refresh.
	//
	// No lock: keys and keysFetched are touched only from the applier goroutine (via
	// handleChunk), which processes frames one at a time.
	keys        [][]byte
	keysFetched time.Time

	// injected carries frames arriving by upload rather than capture into the single-threaded
	// applier. It has no buffer: an uploader is a synchronous HTTP request holding its own
	// reply channel, so there is nothing to gain by queuing more than one and every reason not
	// to silently accept uploads a stopped applier will never drain.
	injected chan injectedFrame

	// running is true for exactly the span of one Run call, so Ingest can fail fast rather
	// than block forever when nothing is there to receive from injected.
	running atomic.Bool

	// runDone is closed when Run returns, which is what lets an Ingest caught mid-shutdown —
	// running observed true a moment before Run tears down — get an error instead of hanging
	// on a channel nothing will ever read from or reply to again.
	runDone chan struct{}

	// ingestSequence numbers frames arriving through Ingest, offset by ingestSequenceBase so
	// they never collide with a capture source's own sequence numbers within a session.
	ingestSequence atomic.Int64
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
		injected: make(chan injectedFrame),
		runDone:  make(chan struct{}),
	}
}

// Session is the capture session this receiver is recording under.
func (r *Receiver) Session() uuid.UUID { return r.session }

// Run captures and decodes until the context is done.
//
// It is single-use per Receiver: runDone is closed in this method's own defer, and a channel
// cannot be closed twice, so calling Run again on the same Receiver after it has returned once
// panics. Build a new Receiver for a new run.
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

	// running and runDone bracket the span in which the applier loop below can actually
	// receive from injected. Set only now, rather than at the very top of Run — r.session is
	// plain, unsynchronised state that prepare reads for every frame it stores, captured or
	// injected, and the atomic Store here is what an Ingest call's atomic Load synchronises
	// with. Setting the flag before the assignment above would let a fast Ingest call race it:
	// the store happening-before the load is exactly what publishes r.session safely, and only
	// once it has actually been written.
	//
	// This defer is registered *after* the session-close one above so that, by LIFO order, it
	// runs *first* on the way out: the moment the applier loop actually exits, runDone closes
	// and running goes false immediately, rather than after the session-close defer's own DB
	// round-trip. An Ingest call in flight with its own (uncancelled) context would otherwise
	// have no way to notice shutdown until that write finished — blocking across a database
	// call it has nothing to do with, for as long as that call takes.
	r.running.Store(true)
	defer func() {
		r.running.Store(false)
		close(r.runDone)
	}()

	workers := r.decodeWorkers()
	r.log.Info("decoding concurrently", zap.Int("workers", workers))

	// One reader, several decoders, one applier.
	//
	// Decoding is what the receiver spends its time on — hundreds of milliseconds a frame at a dense
	// geometry — and it is embarrassingly parallel: a frame is decoded from its own pixels and nothing
	// else. Serialising it meant a display running faster than one core could decode was simply wasted,
	// with the surplus frames photographed and discarded.
	//
	// What is *not* parallel is everything after the decode. Chunk rows, acknowledgements, and the merge
	// that fires when the last chunk lands are shared, and the maps tracking which transmissions have
	// started and finished are plain maps. Keeping the applier single-threaded means none of that needed
	// to change or acquire a lock: it is exactly as sequential as it was, and only the expensive middle
	// fans out.
	//
	// The queues are small on purpose. A deep buffer would let the reader run far ahead of the applier and
	// fill memory with decoded frames, and there is no value in it — a frame read long ago is off the
	// display anyway.
	work := make(chan Capture, workers)
	results := make(chan prepared, workers)

	var decoders sync.WaitGroup
	for range workers {
		decoders.Add(1)
		go func() {
			defer decoders.Done()
			for capture := range work {
				select {
				case results <- r.prepare(ctx, capture):
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Several readers, not one.
	//
	// Reading a frame is not free: it is a directory listing, a file read, a PNG decode, and — on the
	// simulated channel — a lens and sensor model over every pixel. Only the first of those needs to be
	// serialised, and the source releases its lock as soon as it has claimed a frame, so the rest overlaps.
	// With one reader it did not, and the whole receiver waited behind it however many decode workers were
	// idle.
	//
	// The readers close work when they have all finished; the decoders then drain and exit, and closing
	// results ends the applier. Shutting down in that order means no goroutine is left writing to a closed
	// channel.
	// One reader, deliberately, after trying more than one and putting it back.
	//
	// Several readers looked right: the source releases its lock as soon as it has claimed a frame, so the
	// file read, the PNG decode and — on the simulated channel — the optics model could all overlap. What
	// happened instead was that the end-to-end suite became unstable, with a different test timing out on
	// each run. Nineteen decoders and several readers is more CPU-bound goroutines than the machine has
	// cores, and oversubscribing it made every individual frame slower without making more of them finish.
	//
	// A different test failing each time is a reason to stop and put the change back, not to tune it. The
	// decode fan-out is where the measured gain is — 8 MB of incompressible payload at 1.46 MB/s — and it
	// does not need this.
	//
	// The lock is still released early, which costs nothing and is correct regardless: only claiming a frame
	// off the channel needs mutual exclusion.
	readerCount := 1
	var readers sync.WaitGroup
	for range readerCount {
		readers.Add(1)
		go func() {
			defer readers.Done()
			idle := r.cfg.Current().Capture.IdleInterval
			for {
				if ctx.Err() != nil {
					return
				}

				capture, err := r.source.Next(ctx)
				switch {
				case errors.Is(err, ErrNoFrame):
					// Nothing on the channel. An idle channel is the normal state between transmissions, so
					// this waits rather than reporting anything.
					select {
					case <-ctx.Done():
						return
					case <-time.After(idle):
					}
					continue
				case err != nil:
					r.log.Warn("capture failed", zap.Error(err))
					select {
					case <-ctx.Done():
						return
					case <-time.After(idle):
					}
					continue
				}

				select {
				case work <- capture:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		readers.Wait()
		close(work)
	}()

	go func() {
		decoders.Wait()
		close(results)
	}()

	// A select over both channels rather than two loops, because the single-threaded applier is
	// the whole point: a frame imported through the API and a frame read off the optical channel
	// contend for the same chunk rows, acknowledgements, and merge logic, and keeping them on one
	// goroutine is what let every downstream piece stay exactly as sequential as it always was.
	for {
		select {
		case p, ok := <-results:
			if !ok {
				return nil
			}
			if err := r.apply(ctx, p); err != nil {
				r.log.Error("could not process a captured frame",
					zap.Int64("sequence", p.capture.Sequence), zap.Error(err))
			}
		case inj := <-r.injected:
			err := r.apply(ctx, inj.p)
			inj.reply <- ingestReply{result: resultOf(inj.p), err: err}
		}
	}
}

// injectedFrame is a frame arriving by upload rather than capture, carrying a reply channel
// because the uploader is a synchronous HTTP request that wants the verdict.
type injectedFrame struct {
	p     prepared
	reply chan ingestReply
}

// ingestReply is what Ingest hands back to its caller.
type ingestReply struct {
	result IngestResult
	err    error
}

// IngestResult is the outcome of running one uploaded image through the pipeline.
type IngestResult struct {
	Decoded        bool       `json:"decoded"`
	IsManifest     bool       `json:"is_manifest"`
	TransmissionID *uuid.UUID `json:"transmission_id,omitempty"`
	ChunkNumber    *int64     `json:"chunk_number,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// resultOf reports what a prepared frame decoded to, for the caller of Ingest. It looks only at
// the decode itself — not at what apply subsequently did with it — because apply's own failure is
// returned separately as an error, and what happened deeper in the pipeline (a chunk that failed
// to decrypt, say) is not a decode failure: the frame's own checksums passed.
func resultOf(p prepared) IngestResult {
	if p.err != nil {
		return IngestResult{Error: p.err.Error()}
	}
	if p.decodeErr != nil {
		return IngestResult{Error: p.decodeErr.Error()}
	}
	if p.frame == nil {
		return IngestResult{Error: "pipeline: the image did not decode to a frame"}
	}

	result := IngestResult{
		Decoded:    true,
		IsManifest: p.frame.Header.Flags.Has(protocol.FlagManifest),
	}
	transmission := p.frame.Header.TransmissionID
	result.TransmissionID = &transmission
	if !result.IsManifest {
		chunk := int64(p.frame.Header.ChunkNumber)
		result.ChunkNumber = &chunk
	}
	return result
}

// ingestSequenceBase keeps uploaded frames' sequence numbers out of the range any capture
// source will reach, so the two never collide within a session.
const ingestSequenceBase = int64(1) << 40

// ErrNotRunning means Ingest was called while the receiver's applier was not actually able to
// receive from the injection channel — Run has not been started yet, or it has already
// stopped. It is a sentinel rather than a bare string so a caller processing many frames in one
// request — the import endpoint's zip loop, most notably — can tell "the whole pipeline just
// went away" from "this one frame was bad" with errors.Is, and stop asking rather than turning
// an outage into hundreds of identical, misleading per-entry failures.
var ErrNotRunning = errors.New("pipeline: the receiver is not running")

// Ingest runs one uploaded image through the same store-decode-apply path a captured frame
// takes. prepare fans out safely (it touches nothing shared), and the apply is handed to Run's
// applier over a channel — so imported frames interleave with camera frames without a single
// new lock, and everything downstream (acks, merge, delivery) cannot tell the difference. That
// indistinguishability is the point: a frame archive imported from a USB stick is a transport,
// not a parser, and it earns that only by walking the exact path a camera frame would.
func (r *Receiver) Ingest(ctx context.Context, img image.Image, raw []byte) (IngestResult, error) {
	if !r.running.Load() {
		return IngestResult{}, ErrNotRunning
	}

	capture := Capture{
		Sequence:   r.ingestSequence.Add(1) + ingestSequenceBase,
		Image:      img,
		Raw:        raw,
		CapturedAt: time.Now().UTC(),
	}
	p := r.prepare(ctx, capture)

	inj := injectedFrame{p: p, reply: make(chan ingestReply, 1)}
	select {
	case r.injected <- inj:
	case <-ctx.Done():
		return IngestResult{}, ctx.Err()
	case <-r.runDone:
		// running was observed true a moment ago, but Run has since torn down and nothing will
		// ever read this off the channel. Reported as ErrNotRunning rather than left to block:
		// an HTTP handler waiting on this is a request an operator is staring at, and a caller
		// making several of these calls needs to be able to tell this apart from a per-frame
		// failure the same way it would the check above.
		return IngestResult{}, fmt.Errorf("%w: stopped while the frame was being submitted", ErrNotRunning)
	}

	select {
	case reply := <-inj.reply:
		return reply.result, reply.err
	case <-ctx.Done():
		return IngestResult{}, ctx.Err()
	case <-r.runDone:
		// The frame was handed off, but Run stopped before the applier could get to it or before
		// it could send the reply back — the same race, caught at the other end of the round trip.
		return IngestResult{}, fmt.Errorf("%w: stopped before the frame was applied", ErrNotRunning)
	}
}

// decodeWorkers is how many frames are decoded at once.
//
// Configuration wins; otherwise one per core less one, so the reader, the applier, and Postgres are not
// competing with a full set of decoders for the last core. At least one, obviously — a receiver that
// decoded nothing would capture frames and throw them away.
func (r *Receiver) decodeWorkers() int {
	if configured := r.cfg.Current().Capture.DecodeWorkers; configured > 0 {
		return configured
	}
	return max(1, runtime.NumCPU()-1)
}

// prepared is a captured frame that has been persisted and decoded, on its way to being applied.
//
// It exists because processing a frame has two halves with completely different properties. Persisting and
// decoding are per-frame work that touches nothing shared — a distinct object key, a decode over one
// image — and decoding is by far the most expensive thing the receiver does. Everything after it mutates
// state that several frames contend for: chunk rows, acknowledgements, the merge that fires when the last
// chunk lands.
//
// So the first half fans out across cores and the second half stays strictly serial. That split is what
// makes the receiver keep up with a fast display, and it is also why it needs no new locks: every shared
// map and every ordering assumption in the second half is exactly as single-threaded as it was before.
type prepared struct {
	capture Capture

	// key and sum are where the capture was stored and its hash.
	key string
	sum []byte

	frame    *protocol.Frame
	geometry *protocol.Geometry

	finder, timing, contrast float64

	// decodeErr is why the frame could not be read, or nil. A frame that failed is still applied — it is
	// recorded as an unreadable capture, which is the receiver's primary diagnostic.
	decodeErr error

	// err is a failure of the preparation itself, such as the capture not being storable. Distinct from
	// decodeErr: one is the channel being bad, the other is this process being unable to do its job.
	err error
}

// prepare persists and decodes one captured frame. It is safe to run on many frames at once.
func (r *Receiver) prepare(ctx context.Context, capture Capture) prepared {
	// Persisted before decoding, always. A frame that cannot be read is the primary evidence for
	// why a capture is going badly, and it is also the only thing a replay can work from.
	key, sum, err := r.storeCapture(ctx, r.session, capture)
	if err != nil {
		return prepared{capture: capture, err: err}
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

	return prepared{
		capture: capture, key: key, sum: sum,
		frame: frame, geometry: geometry,
		finder: finder, timing: timing, contrast: contrast,
		decodeErr: decodeErr,
	}
}

// apply records a prepared frame and acts on it. It must run one frame at a time.
func (r *Receiver) apply(ctx context.Context, p prepared) error {
	if p.err != nil {
		return p.err
	}
	capture, key, sum := p.capture, p.key, p.sum
	frame, geometry := p.frame, p.geometry
	finder, timing, contrast := p.finder, p.timing, p.contrast
	decodeErr := p.decodeErr

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

// keyring is every key this receiver holds: the configured one, then the loaded ones.
func (r *Receiver) keyring(ctx context.Context) [][]byte {
	if time.Since(r.keysFetched) < 3*time.Second {
		return r.keys
	}
	ring := [][]byte{}
	if k := r.cfg.Current().EncryptionKey(); len(k) > 0 {
		ring = append(ring, k)
	}
	if stored, err := r.store.DecoderKeys.List(ctx); err == nil {
		for _, dk := range stored {
			ring = append(ring, dk.Key)
		}
	} else {
		r.log.Warn("could not list decoder keys", zap.Error(err))
	}
	r.keys, r.keysFetched = ring, time.Now()
	return ring
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

	payload, err := protocol.OpenFrame(r.keyring(ctx), frame)
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

	// A chunk that had already arrived is not acknowledged again.
	//
	// This is the single most expensive line in the system when it is wrong, and it was. Every
	// acknowledgement is a file in a directory the sender re-lists and re-verifies on a poll, and a
	// duplicate acknowledgement carries no information the sender does not already have: it acknowledged
	// that chunk once, stopped displaying it, and cannot un-know it. Writing one anyway meant a transfer
	// produced an acknowledgement per *captured frame* rather than per chunk — measured at 5972 files for
	// 526 chunks, with the shared volume ending up thirty-five times the size of the file being sent.
	//
	// The cost is not the disk. It is that the sender's scan is O(files), so the acknowledgement channel
	// got slower in proportion to how long the transfer had been running — which showed up as a transfer
	// that started at 192 KB/s and was down to 24 KB/s by its fifth run.
	//
	// Duplicates still arrive and are still counted; they are simply not reported. The sender learns
	// nothing from them, and the first acknowledgement already told it everything.
	if inserted {
		if err := r.emitAck(ctx, frame, protocol.AckOK, errorRate); err != nil {
			return err
		}
	} else {
		r.log.Debug("duplicate chunk, not acknowledged again",
			zap.Uint32("chunk", frame.Header.ChunkNumber))
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
