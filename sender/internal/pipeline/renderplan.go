package pipeline

import (
	"context"
	"runtime"
	"sync/atomic"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Rendering a transmission's frames, several at a time.
//
// Encoding one frame is a chunk read from the object store, a grid drawn cell by cell, a PNG compressed and
// an object written back. It is expensive, it is mostly CPU with two round trips of I/O around it, and it
// shares nothing with any other frame. Rendered one at a time it was comfortably the slowest stage in the
// system, and it degraded in the direction that hurts: a smaller grid carries less payload per frame, so it
// needs more frames for the same file, and the geometry that most needs help produced the most work.
//
// The order in which frames exist is genuinely sequential — frame numbers ascend, and the manifest is
// re-emitted at a fixed interval among them — but that is arithmetic over a list and costs nothing. So the
// plan is built serially and the expensive part runs across the cores, writing into a pre-sized slice by
// index, which keeps the output order identical to the serial version's.

// plannedFrame is one frame to render, decided before any of them are.
type plannedFrame struct {
	// number is the frame's position in the displayed sequence, and its index in the output slice.
	number int

	// manifest marks the frame that describes the transfer rather than carrying a chunk of it.
	manifest bool

	// chunk is what this frame carries, for the frames that carry one.
	chunk   store.Chunk
	flags   protocol.Flags
	chunkID *uuid.UUID
}

// planFrames decides which frames exist and in what order.
//
// Every decision that depends on position is made here: the frame numbers, where the manifest repeats, and
// the flags that mark the last chunk and the end of the stream. The renderer that follows needs no order of
// its own, which is what lets it run in any order at all.
func planFrames(tx store.Transmission, manifest protocol.Manifest, sessionID uuid.UUID,
	chunks []store.Chunk, sourceChunks, interval int,
) []plannedFrame {
	// One manifest at the head, one per interval among the chunks, and one frame per chunk.
	plan := make([]plannedFrame, 0, len(chunks)+len(chunks)/max(interval, 1)+2)

	number := 0
	emit := func() {
		plan = append(plan, plannedFrame{number: number, manifest: true})
		number++
	}

	// The manifest leads, so a receiver watching from the start knows what is coming before it arrives.
	emit()

	for i, c := range chunks {
		// Re-emitted among the chunks rather than only first, so a camera that came online mid-transfer can
		// join the stream instead of waiting for the next transmission. For a file that takes an hour to
		// display, that is the difference between a working installation and an unusable one.
		if interval > 0 && number%interval == 0 {
			emit()
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

		id := c.ID
		plan = append(plan, plannedFrame{
			number:  number,
			chunk:   c,
			flags:   flags,
			chunkID: &id,
		})
		number++
	}
	return plan
}

// renderWorkers is how many frames are encoded at once.
//
// One per core less one, matching the receiver's decode pool and for the same reason: this is per-frame work
// that shares nothing, so it scales with cores almost exactly, and leaving one over keeps the machine
// answering its API while a large transfer renders. Never below one, because a single-core host still has to
// make progress.
func renderWorkers() int {
	if n := runtime.NumCPU() - 1; n > 1 {
		return n
	}
	return 1
}

// renderPlan encodes every planned frame, several at a time, into out.
//
// out is indexed by the frame's own number, so the result is byte-identical to rendering them in order —
// the parallelism is in when each frame is encoded, never in what ends up where.
func (p *Pipeline) renderPlan(ctx context.Context, jc *jobs.Context, encoder encoding.Encoder,
	layout protocol.Layout, tx store.Transmission, manifest protocol.Manifest, sessionID uuid.UUID,
	sourceChunks int, plan []plannedFrame, out []store.Frame,
) error {
	if len(plan) == 0 {
		return nil
	}

	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(renderWorkers())

	// Progress is reported by whichever worker finishes, so it counts completions rather than position:
	// with several in flight there is no "current" frame, and a counter that went backwards would be worse
	// than a coarse one.
	var done atomic.Int64

	for i := range plan {
		planned := plan[i]
		group.Go(func() error {
			frame, err := p.frameFor(ctx, tx, manifest, sessionID, sourceChunks, planned)
			if err != nil {
				return err
			}
			record, err := p.renderFrame(ctx, encoder, layout, tx, frame,
				planned.number, planned.chunkID, planned.manifest)
			if err != nil {
				return err
			}
			out[planned.number] = record

			if n := done.Add(1); n%64 == 0 {
				jc.Progress(ctx, 50+int(45*n)/len(plan), "rendered %d of %d frames", n, len(plan))
			}
			return nil
		})
	}
	return group.Wait()
}

// frameFor builds the protocol frame one planned entry carries.
//
// The manifest is rebuilt per occurrence rather than shared. It is cheap beside encoding a grid, and a
// shared frame would have to be copied before its number was set anyway — at which point the copy is the
// only thing saved and aliasing an encrypted payload across goroutines is the risk taken for it.
func (p *Pipeline) frameFor(ctx context.Context, tx store.Transmission, manifest protocol.Manifest,
	sessionID uuid.UUID, sourceChunks int, planned plannedFrame,
) (*protocol.Frame, error) {
	header := protocol.Header{
		TransmissionID: tx.ID,
		SessionID:      sessionID,
		FrameNumber:    uint32(planned.number),
		TotalChunks:    uint32(sourceChunks),
	}

	if planned.manifest {
		frame, err := protocol.NewManifestFrame(header, manifest)
		if err != nil {
			return nil, jobs.Permanent(err)
		}
		return frame, nil
	}

	payload, err := objectstore.GetBytes(ctx, p.objects, planned.chunk.StoredPath, int64(tx.ChunkSize)+1)
	if err != nil {
		return nil, err
	}

	header.Flags = planned.flags
	header.CompressionID = manifest.CompressionID
	header.FECID = manifest.FEC.ID
	header.ChunkNumber = uint32(planned.chunk.ESI)

	if tx.EncryptionID != int(protocol.EncryptionNone) {
		frame, err := protocol.NewEncryptedFrame(tx.EncryptionKey, uint8(tx.EncryptionID), header, payload)
		if err != nil {
			return nil, jobs.Permanent(err)
		}
		return frame, nil
	}
	return protocol.NewFrame(header, payload), nil
}
