package pipeline

import (
	"context"
	"github.com/opticaltransport/otp/receiver/internal/config"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Reading every frame a capture holds, rather than the first one.
//
// A tiled display carries several independent frames at once, and the ordinary decode path finds
// exactly one of them: it locates the strongest set of fiducials in the picture and stops. On a
// four-lane display that reads a quarter of what was sent and discards the rest, which is the whole
// gain of tiling thrown away at the last step.
//
// So the capture is asked for every frame it holds. Each is an ordinary frame with its own fiducials,
// header, checksums and photometric reference — nothing about a lane is special once it has been
// found — and each becomes its own prepared result, sequenced and recorded separately, exactly as
// though it had arrived alone.
//
// The extra lanes deliberately share the stored image. One photograph produced them all, and writing
// it once per lane would multiply the object store by the lane count to hold four identical copies.
// The rows differ, because the frames differ; the evidence behind them is one picture.

// maxLanesPerCapture bounds how many frames are looked for in one photograph.
//
// Not a protocol limit but a cost one: every candidate frame costs a crop and a descriptor read, and
// a picture of noise offers many candidates. Sixteen matches the largest tiling the protocol lays
// out, so a legitimate display is never truncated.
const maxLanesPerCapture = 16

// prepareAll reads every frame in one capture.
//
// The first is prepared by the ordinary path, which stores the image, records the aiming figures and
// runs the merge and recovery stages — all of which describe the photograph rather than any one lane
// and should happen once. Any further frames are prepared against the same stored image.
//
// A capture holding one frame costs one extra fiducial search over the old path and behaves
// identically otherwise, so this is not a tiled-only path with a single-frame path beside it.
func (r *Receiver) prepareAll(ctx context.Context, capture Capture) []prepared {
	first := r.prepare(ctx, capture)

	// Nothing further to look for if the capture could not even be stored: that is a fault in this
	// process rather than a property of the picture, and the failure is already recorded.
	if first.err != nil {
		return []prepared{first}
	}

	cfg := r.cfg.Current()
	if cfg.Capture.Lanes <= 1 {
		return []prepared{first}
	}

	opts := r.locateOptions(cfg)
	found := protocol.LocateAll(capture.Image, opts, min(cfg.Capture.Lanes, maxLanesPerCapture))

	// Recorded for every photograph, including the ones that hold a single frame or none. This is what
	// the aiming display compares against, and it is only honest if the lean captures count too: a
	// sender switched down to one lane has to be able to bring the expectation down with it, which it
	// can only do by being seen. See laneexpect.go.
	expected := r.lanes.observe(len(found))

	if len(found) <= 1 {
		// One frame or none. The ordinary result already describes it, and a second pass that found
		// the same frame would file it twice.
		return []prepared{first}
	}

	out := make([]prepared, 0, len(found))
	out = append(out, first)

	// Each lane's outcome, kept for the aiming display so it can colour every outline by what that
	// lane actually did rather than by what the capture as a whole did.
	readings := make([]laneReading, 0, len(found))

	for lane, g := range found {
		// The frame the ordinary path already produced. Compared by frame number rather than by
		// geometry, because the two searches may have located the same frame from slightly different
		// fiducial estimates and would not compare equal as structures.
		if first.geometry != nil && g.Header.FrameNumber == first.geometry.Header.FrameNumber {
			// Still an outline. It is a lane in the picture, and the overlay leaving a gap where the
			// lead frame sits would read as that lane having been missed.
			readings = append(readings, laneReading{geometry: g, decoded: first.decodeErr == nil})
			// The lead row belongs to this photograph too, so it is told where it sits.
			out[0].detail.laneIndex = lane
			out[0].detail.laneCount = len(found)
			continue
		}

		frame, err := encoding.DecodeAt(g, capture.Image, opts)

		// The same read the lead lane gets, not a bare decode.
		//
		// This is where a tiled transfer was quietly losing most of its lanes. A raw decode is not the
		// ordinary way a real frame succeeds — on a camera it is the lucky way. Nearly every capture
		// lands just past its payload CRC and is rescued by the merge across shots or by the recovery
		// engine, and neither of those ran here, so the lead lane recovered and every other lane was
		// thrown away with the payload_crc it would have survived. Measured over one 775-capture
		// session: 205 photographs read one of their two lanes, and 3 read both.
		frame, err, detail := r.readLane(ctx, cfg, capture, g, frame, err)

		// Where in the picture this read came from. Several rows share one stored image, so without
		// these the detail view can show four results against one photograph and say nothing about
		// which part of it produced any of them.
		detail.laneIndex = lane
		detail.laneCount = len(found)

		readings = append(readings, laneReading{geometry: g, decoded: err == nil})
		if err != nil {
			// Located but unreadable. Recorded as its own failure rather than dropped: a lane that
			// finds its geometry and fails its payload is the state worth seeing, and lumping it in
			// with the lanes that were never found at all is what hid this class of problem before.
			out = append(out, r.laneResult(capture, first, g, nil, err, detail))
			continue
		}
		out = append(out, r.laneResult(capture, first, g, frame, nil, detail))
	}

	// The aiming display is re-measured across every lane, replacing the single-frame reading the
	// ordinary path recorded. That reading described one lane and reported how much of the view it
	// filled, which on a tiled display is advice that actively works against the operator: following
	// it moves the other lanes out of shot.
	//
	// Stored after the lanes have been read rather than before, which is what lets it report per-lane
	// outcomes instead of one verdict stamped across all of them.
	r.alignment.Store(ptr(measureReadings(capture.Image, readings, expected)))

	if len(out) > 1 {
		r.log.Debug("read several frames from one capture",
			zap.Int("lanes", len(out)),
			zap.Int64("capture", capture.Sequence))
	}
	return out
}

// laneResult builds a prepared for one additional lane of a capture already stored.
func (r *Receiver) laneResult(capture Capture, first prepared, g *protocol.Geometry,
	frame *protocol.Frame, decodeErr error, detail laneDetail,
) prepared {
	// Its own sequence number, because captured_frames is unique on (session, sequence) and each lane
	// is its own row. The stored path is the first result's: one photograph, one image on disk.
	lane := capture
	lane.Sequence = r.capturedSequence.Add(1)

	finder, timing, contrast := decodeQuality(g)
	return prepared{
		capture:   lane,
		key:       first.key,
		sum:       first.sum,
		frame:     frame,
		geometry:  g,
		finder:    finder,
		timing:    timing,
		contrast:  contrast,
		decodeErr: decodeErr,
		detail:    detail,
	}
}

// locateOptions is what every decode in this receiver is given, including the learned layout.
//
// A single accessor because the alternative was tried and cost a run. The lane path built its own
// options from configuration and omitted the learned layout, so every lane re-read the grid
// descriptor from scratch — a few dozen cells in the corner of the header band, no more legible on a
// marginal capture than anything else. Measured against a camera holding all four lanes in view with
// fiducials matching at 0.98: 65 of 67 frames failed on the descriptor and one decoded. The geometry
// was right every time; the block that says what the geometry *is* was not readable, and the receiver
// already knew the answer from the frame that had decoded.
func (r *Receiver) locateOptions(cfg config.Config) protocol.LocateOptions {
	opts := cfg.LocateOptions()
	if learned := r.layout.Load(); learned != nil {
		opts.ExpectedLayout = learned
	}
	return opts
}
