package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"github.com/google/uuid"
	"image"
	"image/png"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/optical"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Showing several frames at once, tiled across the display.
//
// The composition happens here, at the moment of display, and nothing upstream knows about it. Frames
// are encoded, stored and checksummed one at a time exactly as they always were; this only decides
// how many of them are on the screen together and where each one sits. That keeps the encoder, the
// object store and the audit path unchanged, and it means a transmission rendered before lanes
// existed can be displayed with them.
//
// The receiver needs no agreement about any of this. Each lane is an ordinary frame with its own
// fiducials and its own checksums, so a decoder handed a photograph of four of them finds four
// frames — see protocol.LocateAll, which discovers them without being told the tiling, because a
// quad assembled across two lanes fails its descriptor CRC and rejects itself.

// fillLanes repeats the chosen frames until every lane has something to carry.
//
// This is the tail of a transmission: one chunk left and four lanes to put it in. Leaving those lanes
// dark was the original behaviour and it wastes them, because the copies are not redundant to the
// receiver. Each lane sits in a different part of the display and is therefore photographed through a
// different part of the lens, at its own local exposure and its own sub-pixel phase, so four copies of
// one chunk are four independent attempts at reading it within a single photograph — and the merge
// combines them exactly as it would four separate frames. It costs nothing: those lanes had nothing
// else to carry.
//
// The copy on the first line is load-bearing, which is why this is a function with a test rather than
// four lines inline. `append(chosen, ...)` would write into the caller's backing array whenever it had
// spare capacity, quietly replacing chunks the accounting loop is about to read — a corruption that
// would appear only for some slice capacities and only at the end of a transfer.
func fillLanes(chosen []store.Frame, lanes int) []store.Frame {
	if len(chosen) >= lanes || len(chosen) == 0 {
		return chosen
	}
	display := append([]store.Frame(nil), chosen...)
	for i := 0; len(display) < lanes; i++ {
		display = append(display, chosen[i%len(chosen)])
	}
	return display
}

// composeLanes tiles several stored frames into one display image.
//
// Fewer frames than lanes is accepted rather than treated as an error, because the caller may have
// chosen not to fill them — but see fillLanes, which is why that is now unusual. A lane left as
// background is read by a receiver as an absence: it finds no fiducials there and moves on.
func composeLanes(ctx context.Context, objects objectstore.Store, frames []store.Frame, lanes int,
) ([]byte, int, int, error) {
	if len(frames) == 0 {
		return nil, 0, 0, fmt.Errorf("scheduler: nothing to display")
	}

	images := make([]image.Image, 0, len(frames))
	for _, f := range frames {
		body, err := objectstore.GetBytes(ctx, objects, f.StoredPath, 64<<20)
		if err != nil {
			return nil, 0, 0, err
		}
		img, err := png.Decode(bytes.NewReader(body))
		if err != nil {
			return nil, 0, 0, fmt.Errorf("scheduler: frame %d will not decode: %w", f.FrameNumber, err)
		}
		images = append(images, img)
	}

	// A single lane is passed through untouched. Composing one frame into a one-lane tiling would
	// produce the same pixels at the cost of a decode and a re-encode on every displayed frame, and
	// this path runs for every frame of every transfer.
	if lanes <= 1 {
		body, err := objectstore.GetBytes(ctx, objects, frames[0].StoredPath, 64<<20)
		if err != nil {
			return nil, 0, 0, err
		}
		return body, frames[0].WidthPx, frames[0].HeightPx, nil
	}

	// The lane geometry is taken from the frames themselves rather than from configuration. A
	// transmission carries its own grid, and configuration can have moved on since it was rendered —
	// which is precisely the disagreement that has cost this project real debugging time elsewhere.
	first := images[0].Bounds()
	layout := protocol.Layout{
		GridWidth:  first.Dx(),
		GridHeight: first.Dy(),
		CellPixels: 1,
		QuietZone:  0,
	}
	tiled, err := protocol.NewLaneLayout(layout, lanes, 0)
	if err != nil {
		return nil, 0, 0, err
	}

	composed, err := tiled.Compose(images)
	if err != nil {
		return nil, 0, 0, err
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, composed); err != nil {
		return nil, 0, 0, err
	}
	size := tiled.ImageSize()
	return buf.Bytes(), size.X, size.Y, nil
}

// showLanes composes a round's frames and puts them on the display.
//
// The display is told about the round as a whole: one image, one sequence number, and the leading
// frame's identity for the logs and for anything watching over HTTP. The other lanes are on the same
// picture and are found by the receiver from their own fiducials, so there is nothing to describe.
func (s *Scheduler) showLanes(ctx context.Context, frames []store.Frame, lanes int, priority Priority) error {
	body, width, height, err := composeLanes(ctx, s.objects, frames, lanes)
	if err != nil {
		return err
	}

	lead := frames[0]
	// No sequence is passed: the display assigns it. A scheduler runs per transmission and two of them
	// counting separately is how concurrent transfers came to overwrite each other's frames.
	if err := s.sink.Show(ctx, optical.Frame{
		Number:       lead.FrameNumber,
		Transmission: lead.TransmissionID,
		Manifest:     lead.IsManifest,
		WidthPx:      width,
		HeightPx:     height,
		PNG:          body,
	}); err != nil {
		return err
	}

	// Once per distinct frame. A frame repeated into a spare lane went out once as far as the
	// transmission is concerned, and recording it twice would misreport how often it had been shown.
	marked := make(map[uuid.UUID]bool, len(frames))
	for _, f := range frames {
		if marked[f.ID] {
			continue
		}
		marked[f.ID] = true
		if err := s.store.Frames.MarkDisplayed(ctx, f.ID); err != nil {
			s.log.Warn("could not record a display", zap.Error(err))
		}
	}

	s.log.Debug("frames displayed",
		zap.Int("lanes", len(frames)),
		zap.Int("lead_frame", lead.FrameNumber),
		zap.String("priority", priority.String()))
	return nil
}
