// Package scheduler decides which frame is on the display next.
//
// It is where the protocol's delivery guarantee actually lives, and the guarantee is worth stating
// precisely: every chunk is displayed repeatedly until the receiver says it arrived. Nothing else in
// the system promises delivery — a frame can be lost to a tear, a hand, a refresh caught mid-scan,
// and none of that is detectable at the moment it happens. What makes the transfer lossless is that
// the sender does not move on. A chunk leaves the queue when an acknowledgement for it arrives, and
// only then.
//
// Two consequences follow, and both are the point rather than side effects.
//
// An acknowledged chunk is never displayed again. That is what turns a fixed sequence of frames into
// a channel that converges: as acknowledgements come in, the set of frames still worth showing
// shrinks, and the display spends its time on what has not arrived instead of cycling through what
// has.
//
// And the display never goes idle. If everything in the current window is waiting on
// acknowledgements that have not come yet, the oldest unacknowledged frame is shown again rather
// than showing nothing — because a camera pointed at a blank screen learns nothing, and the frame
// most likely to be missing is the one that has been outstanding longest.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/optical"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Priority orders what gets displayed first.
type Priority int

// Display priorities.
const (
	// PriorityRetransmit is a chunk whose acknowledgement has timed out. It goes first, always: it
	// is the only thing standing between the transmission and completion, while a fresh chunk is
	// merely more work.
	PriorityRetransmit Priority = iota

	// PriorityFresh is a chunk that has not been displayed yet.
	PriorityFresh

	// PriorityKeepAlive is a repeat of the oldest outstanding frame, shown because there is nothing
	// better to show.
	PriorityKeepAlive
)

// String renders a priority for logs.
func (p Priority) String() string {
	switch p {
	case PriorityRetransmit:
		return "retransmit"
	case PriorityFresh:
		return "fresh"
	default:
		return "keep-alive"
	}
}

// Scheduler displays one transmission's frames until the receiver has all of them.
type Scheduler struct {
	store   *store.Store
	objects objectstore.Store
	sink    optical.Sink
	cfg     *config.Watcher
	log     *zap.Logger

	// sequence is the display counter, shared across everything this sink shows.
	mu       sync.Mutex
	sequence int64

	// sent records when each chunk was last displayed, which is what the acknowledgement timeout is
	// measured against. It is in memory rather than in the database because it describes this run of
	// this process: a restart re-displays everything outstanding, which is correct — it cannot know
	// what the receiver saw while it was gone.
	sent map[int]time.Time

	// attempts counts *deliberate* sends of a chunk: the first display, and each retransmission after an
	// acknowledgement timed out. Keep-alive repeats are excluded on purpose.
	//
	// The distinction is what the retry ceiling depends on. A keep-alive is the display filling time while a
	// window waits — at thirty frames a second against a two-second acknowledgement poll, one chunk can be
	// repeated sixty times before the news of its arrival gets back, and counting those as retries trips the
	// ceiling on a transfer that was working perfectly. Attempts count what was actually tried.
	attempts map[int]int
}

// Stats is what a display run achieved.
type Stats struct {
	// FramesShown is how many frames went to the display, including repeats.
	FramesShown int64

	// Retransmissions is how many of those were repeats of a chunk whose acknowledgement had timed
	// out, and KeepAlives how many were shown only because there was nothing else to show.
	Retransmissions int
	KeepAlives      int

	// ChunksAcked is how many distinct chunks the receiver confirmed.
	ChunksAcked int

	// Complete is whether every chunk was acknowledged.
	Complete bool

	// Duration is how long the display ran, and Bytes how many payload bytes the transmission
	// carried, so throughput can be computed from a completed run rather than estimated.
	Duration time.Duration
	Bytes    int64
}

// Throughput is the transferred bytes per second, or zero if it cannot be computed.
func (s Stats) Throughput() float64 {
	if s.Duration <= 0 {
		return 0
	}
	return float64(s.Bytes) / s.Duration.Seconds()
}

// ErrStalled means a chunk exhausted its retries without being acknowledged.
//
// It is deliberately distinct from a transport failure. A stalled transmission means the channel is
// not working — the camera is misaimed, the display is off, the room is dark — and no amount of
// further retrying will fix it. An operator needs to be told, rather than having the sender loop for
// ever looking busy.
var ErrStalled = errors.New("scheduler: a chunk was not acknowledged within its retry limit")

// ErrCancelled and ErrPaused mean an operator stopped the transfer.
//
// They are distinct from every other way a display run can end, and from each other, because what happens
// next differs. A cancelled transmission is over: its frames will not be shown again and the receiver will
// never be told, because there is nothing to tell it — the sender simply stops, and a receiver holding a
// partial file times out on a transmission that no longer exists. A paused one is resumable: its chunks
// keep their acknowledgements, so resuming shows only what is still outstanding.
//
// Neither is a failure, and reporting them as one would put a transfer an operator deliberately stopped
// into the same bucket as a camera that came unplugged.
var (
	ErrCancelled = errors.New("scheduler: the transfer was cancelled")
	ErrPaused    = errors.New("scheduler: the transfer was paused")
)

// New returns a scheduler.
func New(st *store.Store, objects objectstore.Store, sink optical.Sink, cfg *config.Watcher, log *zap.Logger) *Scheduler {
	return &Scheduler{
		store:    st,
		objects:  objects,
		sink:     sink,
		cfg:      cfg,
		log:      log.Named("scheduler"),
		sent:     map[int]time.Time{},
		attempts: map[int]int{},
	}
}

// Run displays a transmission until every chunk is acknowledged or the context is done.
func (s *Scheduler) Run(ctx context.Context, transmissionID uuid.UUID) (Stats, error) {
	tx, err := s.store.Transmissions.Get(ctx, transmissionID)
	if err != nil {
		return Stats{}, err
	}
	if tx.FrameCount == 0 {
		return Stats{}, fmt.Errorf("scheduler: transmission %s has no frames to display", transmissionID)
	}

	frames, err := s.store.Frames.List(ctx, transmissionID)
	if err != nil {
		return Stats{}, err
	}

	// The manifest frames are indexed separately, because they are shown on a schedule of their own:
	// they carry no chunk, so nothing acknowledges them, and a receiver that joined late needs one
	// before anything else it captures makes sense.
	var manifests []store.Frame
	byChunk := map[uuid.UUID]store.Frame{}
	for _, f := range frames {
		if f.IsManifest {
			manifests = append(manifests, f)
			continue
		}
		if f.ChunkID != nil {
			byChunk[*f.ChunkID] = f
		}
	}

	cfg := s.cfg.Current()
	if err := s.store.Transmissions.SetStatus(ctx, transmissionID, store.TxTransmitting, ""); err != nil {
		return Stats{}, err
	}

	session, err := s.store.Sessions.Open(ctx, store.DisplaySession{
		TransmissionID: transmissionID,
		Sink:           s.sink.Name(),
		FPS:            cfg.Display.FPS,
		Brightness:     cfg.Display.Brightness,
		Gamma:          cfg.Display.Gamma,
		WindowSize:     cfg.Display.WindowSize,
	})
	if err != nil {
		return Stats{}, err
	}

	stats := Stats{}
	started := time.Now()
	ticker := time.NewTicker(cfg.FrameInterval())
	defer ticker.Stop()

	// The manifest is shown first and then periodically, so a receiver that came online mid-stream
	// can join. Its cadence is counted in frames displayed rather than in time, so it scales with
	// the frame rate instead of drifting relative to it.
	manifestEvery := cfg.Optical.ManifestInterval
	if manifestEvery < 1 {
		manifestEvery = 1
	}
	nextManifest := 0

	// When the last chunk was acknowledged, so the trailer below knows how long it has been showing the
	// manifest to a receiver that has not yet said it managed to merge. Zero until that happens.
	var lastAckAt time.Time

	for {
		select {
		case <-ctx.Done():
			stats.Duration = time.Since(started)
			if err := s.store.Sessions.Close(ctx, session.ID, "stopped", ""); err != nil {
				s.log.Warn("could not close the display session", zap.Error(err))
			}
			return stats, ctx.Err()
		case <-ticker.C:
		}

		// Configuration is re-read on every frame, because the frame rate and the window size are
		// reloadable: an operator turning the rate down while a transmission runs is the main lever
		// they have when the receiver reports it is falling behind.
		cfg = s.cfg.Current()
		ticker.Reset(cfg.FrameInterval())

		// And so is the status, because that is how an operator stops a transfer. A stop is a row
		// update rather than a signal to this goroutine, which means the API handler and this loop need
		// know nothing about each other and a stop outlives whichever of them restarts first. One
		// indexed single-column read per frame is a price worth paying for a stop that actually stops.
		switch status, err := s.store.Transmissions.Status(ctx, transmissionID); {
		case err != nil:
			return stats, err
		case status == store.TxCancelled:
			stats.Duration = time.Since(started)
			stats.ChunksAcked, _ = s.store.Transmissions.RecountAcked(ctx, transmissionID)
			s.closeSession(ctx, session.ID, "cancelled")
			s.log.Info("display cancelled", zap.String("transmission", transmissionID.String()),
				zap.Int64("frames_shown", stats.FramesShown))
			return stats, ErrCancelled
		case status == store.TxPaused:
			stats.Duration = time.Since(started)
			stats.ChunksAcked, _ = s.store.Transmissions.RecountAcked(ctx, transmissionID)
			s.closeSession(ctx, session.ID, "paused")
			s.log.Info("display paused", zap.String("transmission", transmissionID.String()),
				zap.Int64("frames_shown", stats.FramesShown))
			return stats, ErrPaused
		}

		pending, err := s.store.Chunks.Pending(ctx, transmissionID, cfg.Display.WindowSize)
		if err != nil {
			return stats, err
		}

		if len(pending) == 0 {
			// Everything has been acknowledged — but that is not the same as the receiver being able to use it.
			//
			// It also needs the manifest, and the manifest is one frame in a cycle re-emitted every
			// ManifestInterval frames. A small transfer is acknowledged long before the next one is due: five
			// chunks take about two seconds at ten frames a second, against 6.4 seconds between manifests. So
			// stopping here left the receiver holding every chunk and unable to merge, while the sender waited
			// for a completion report that could never come. Over a camera, which necessarily starts watching
			// after the display has begun, that was the normal outcome rather than an edge case.
			//
			// The manifest is the only thing the receiver can still be missing at this point, so it is what
			// goes on the screen while waiting to be told the file arrived.
			if lastAckAt.IsZero() {
				lastAckAt = time.Now()
			}
			complete, err := s.store.Transmissions.Status(ctx, transmissionID)
			if err != nil {
				return stats, err
			}
			if afterLastAck(len(manifests) > 0, complete == store.TxCompleted,
				time.Since(lastAckAt), cfg.Ack.Timeout) == showManifest {
				if err := s.show(ctx, manifests[0], PriorityKeepAlive); err != nil {
					return stats, err
				}
				stats.FramesShown++
				stats.KeepAlives++
				continue
			}

			// That is the only condition that ends a transmission successfully, and it is checked against the
			// chunk rows rather than a counter so a duplicate acknowledgement cannot make it look complete early.
			acked, err := s.store.Transmissions.RecountAcked(ctx, transmissionID)
			if err != nil {
				return stats, err
			}
			stats.ChunksAcked = acked
			stats.Complete = true
			stats.Duration = time.Since(started)
			stats.FramesShown = s.sink.Shown()
			stats.Bytes = tx.OriginalSize

			if err := s.store.Sessions.Close(ctx, session.ID, "completed", ""); err != nil {
				s.log.Warn("could not close the display session", zap.Error(err))
			}
			s.log.Info("every chunk acknowledged",
				zap.String("transmission", transmissionID.String()),
				zap.Int("chunks", acked),
				zap.Int64("frames_shown", stats.FramesShown),
				zap.Int("retransmissions", stats.Retransmissions),
				zap.Duration("took", stats.Duration))
			return stats, nil
		}

		if nextManifest <= 0 && len(manifests) > 0 {
			frame := manifests[0]
			if err := s.show(ctx, frame, PriorityKeepAlive); err != nil {
				return stats, err
			}
			nextManifest = manifestEvery
			continue
		}
		nextManifest--

		choice, priority, err := s.choose(ctx, pending, byChunk, cfg)
		if err != nil {
			return stats, err
		}
		if choice == nil {
			// Nothing to show at all, which happens only if the window holds chunks whose frames are
			// missing from the store. That is a bug rather than a channel condition, so it is reported
			// rather than absorbed by displaying nothing.
			return stats, fmt.Errorf("scheduler: no frame available for any of %d pending chunks", len(pending))
		}

		if err := s.show(ctx, *choice, priority); err != nil {
			return stats, err
		}
		// Recorded after the frame is actually out, so the acknowledgement timeout is measured from when the
		// receiver could first have seen it rather than from when the sender decided to.
		if chunk := s.chunkOf(pending, *choice); chunk != nil {
			s.noteSent(chunk.ESI, priority)
		}
		switch priority {
		case PriorityRetransmit:
			stats.Retransmissions++
			if err := s.store.Transmissions.AddCounters(ctx, transmissionID, 1, 0); err != nil {
				s.log.Warn("could not record a retransmission", zap.Error(err))
			}
		case PriorityKeepAlive:
			stats.KeepAlives++
		}

		// A chunk that has been *sent* too many times without acknowledgement means the channel is not
		// working. Continuing would keep the process looking busy while making no progress, which is the
		// worst thing to present to an operator. Keep-alive repeats do not count, for the reason given on
		// the attempts field.
		if chunk := s.chunkOf(pending, *choice); chunk != nil && s.attemptsOf(chunk.ESI) > cfg.Ack.MaxRetries {
			stats.Duration = time.Since(started)
			reason := fmt.Sprintf("chunk %d was sent %d times without being acknowledged",
				chunk.ESI, s.attemptsOf(chunk.ESI))
			if err := s.store.Sessions.Close(ctx, session.ID, "failed", reason); err != nil {
				s.log.Warn("could not close the display session", zap.Error(err))
			}
			if err := s.store.Transmissions.SetStatus(ctx, transmissionID, store.TxFailed, reason); err != nil {
				s.log.Warn("could not fail the transmission", zap.Error(err))
			}
			return stats, fmt.Errorf("%w: %s", ErrStalled, reason)
		}
	}
}

// choose picks the next frame to display.
//
// The order is retransmissions, then fresh chunks, then a repeat. Retransmissions first because an
// outstanding chunk is what the transmission is waiting on, while a fresh one only adds to the
// backlog — a scheduler that displayed new chunks first would keep the window full of work the
// receiver cannot confirm and finish later than one that fills the gaps as they appear.
func (s *Scheduler) choose(ctx context.Context, pending []store.Chunk, byChunk map[uuid.UUID]store.Frame, cfg config.Config) (*store.Frame, Priority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	// A chunk that was displayed and not acknowledged within the timeout is assumed lost. The
	// assumption is necessary rather than lazy: a frame the camera missed produces no report at all,
	// so silence is the only signal there is, and waiting longer than the timeout would mean waiting
	// for something that is never coming.
	for _, chunk := range pending {
		at, displayed := s.sent[chunk.ESI]
		if !displayed || now.Sub(at) < cfg.Ack.Timeout {
			continue
		}
		if frame, ok := byChunk[chunk.ID]; ok {
			return &frame, PriorityRetransmit, nil
		}
	}

	for _, chunk := range pending {
		if _, displayed := s.sent[chunk.ESI]; displayed {
			continue
		}
		if frame, ok := byChunk[chunk.ID]; ok {
			return &frame, PriorityFresh, nil
		}
	}

	if !cfg.Display.KeepAlive {
		return nil, PriorityKeepAlive, nil
	}

	// The oldest outstanding chunk: displayed longest ago, so most likely to be the one that was
	// missed.
	var oldest *store.Chunk
	var oldestAt time.Time
	for i := range pending {
		at, displayed := s.sent[pending[i].ESI]
		if !displayed {
			continue
		}
		if oldest == nil || at.Before(oldestAt) {
			oldest, oldestAt = &pending[i], at
		}
	}
	if oldest != nil {
		if frame, ok := byChunk[oldest.ID]; ok {
			return &frame, PriorityKeepAlive, nil
		}
	}
	return nil, PriorityKeepAlive, nil
}

// show displays one frame and records that it went out.
func (s *Scheduler) show(ctx context.Context, frame store.Frame, priority Priority) error {
	body, err := objectstore.GetBytes(ctx, s.objects, frame.StoredPath, 64<<20)
	if err != nil {
		return err
	}

	// No sequence is passed: the display assigns it. A scheduler runs per transmission and two of them
	// counting separately is how concurrent transfers came to overwrite each other's frames.
	if err := s.sink.Show(ctx, optical.Frame{
		Number:       frame.FrameNumber,
		Transmission: frame.TransmissionID,
		Manifest:     frame.IsManifest,
		WidthPx:      frame.WidthPx,
		HeightPx:     frame.HeightPx,
		PNG:          body,
	}); err != nil {
		return err
	}

	if err := s.store.Frames.MarkDisplayed(ctx, frame.ID); err != nil {
		s.log.Warn("could not record a display", zap.Error(err))
	}

	s.log.Debug("frame displayed",
		zap.Int("frame", frame.FrameNumber),
		zap.String("priority", priority.String()))
	return nil
}

// closeSession records why a display run ended.
func (s *Scheduler) closeSession(ctx context.Context, session uuid.UUID, reason string) {
	// Written with a context that is not the cancelled one, so the closing write actually lands.
	if err := s.store.Sessions.Close(context.WithoutCancel(ctx), session, reason, ""); err != nil {
		s.log.Warn("could not close the display session", zap.Error(err))
	}
}

// noteSent records that a chunk's frame has just been displayed.
//
// The timestamp is updated whatever the reason it was shown, because the acknowledgement timeout is about
// when the receiver last had a chance to see it. The attempt count is not, because it is about how many
// times the sender has genuinely tried.
func (s *Scheduler) noteSent(esi int, priority Priority) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[esi] = time.Now()
	if priority != PriorityKeepAlive {
		s.attempts[esi]++
	}
}

// attemptsOf is how many times a chunk has been deliberately sent.
func (s *Scheduler) attemptsOf(esi int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[esi]
}

// chunkOf finds which pending chunk a frame carries.
func (s *Scheduler) chunkOf(pending []store.Chunk, frame store.Frame) *store.Chunk {
	if frame.ChunkID == nil {
		return nil
	}
	for i := range pending {
		if pending[i].ID == *frame.ChunkID {
			return &pending[i]
		}
	}
	return nil
}
