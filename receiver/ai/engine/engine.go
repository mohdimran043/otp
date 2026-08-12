// Package engine is the seam every AI implementation sits behind.
//
// The receiver asks one question — "this frame was rejected; can you read it?" — and does not know or
// care what answers it. Today the answer comes from a deterministic candidate search in Go. Later it
// may come from a restoration network on a GPU, from a symbol classifier, or from several in sequence.
// None of that should reach the decode path, because the moment it does, swapping an implementation
// means editing the pipeline.
//
// So the interface is deliberately narrow and stated in the decoder's own terms: an image and a
// geometry in, a verified frame out. It does not expose enhancement, posteriors, tensors or model
// files, because a caller that could see those would start depending on them.
//
// What every implementation must guarantee, and what makes this safe to put on the decode path:
//
//   - A returned frame has been verified against the footer's CRC32 and SHA-256. Not "improved",
//     not "probably right" — confirmed. Callers treat success as final.
//   - Nothing is mutated. The image and geometry belong to the caller.
//   - Refusal is normal and cheap. ErrNotRecovered is the expected answer most of the time.
package engine

import (
	"context"
	"errors"
	"image"
	"time"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/shared/protocol"
)

// ErrNotRecovered means this engine could not read the frame. It is the ordinary answer, not a fault.
var ErrNotRecovered = errors.New("engine: frame not recovered")

// Request is one rejected frame, with everything the decoder already learned about it.
//
// Geometry is the important field and it may be nil. A frame whose fiducials were never found has no
// geometry, and an engine that works at a known geometry — which is all of them today — must refuse
// rather than invent one. Passing the bucket as well means an engine can decide without re-deriving it
// from the error string.
type Request struct {
	// Image is the capture as it arrived. Engines must not modify it.
	Image image.Image

	// Geometry is where the decoder found the grid, or nil if it never did.
	Geometry *protocol.Geometry

	// Bucket is the stage the decode failed at.
	Bucket classify.Bucket

	// Clipped is the fraction of pixels saturated in at least one channel. An engine may refuse
	// outright on a badly clipped frame: no processing recovers a 255, and spending a GPU on one is
	// worse than declining.
	Clipped float64
}

// Report is what an engine did, for the logs and the operator's panel.
//
// Every field here exists to make a model change measurable. Swapping an implementation and finding
// the recovered count unchanged is not evidence of anything unless the versions, stages and latencies
// are recorded alongside it.
type Report struct {
	// Engine and Version identify what produced this. Version is free-form: a model version for a
	// learned engine, a strategy name for a deterministic one.
	Engine  string `json:"engine"`
	Version string `json:"version"`

	// Stage names the rung of the ladder that succeeded, for a chain of engines.
	Stage string `json:"stage,omitempty"`

	// Flips is how many cell decisions were corrected, and Candidates how many corrections were
	// tried. Candidates rising per recovery is the earliest sign a camera is drifting.
	Flips      int `json:"flips"`
	Candidates int `json:"candidates"`

	// Considered is how many cells the engine looked at: the search set for a bounded search, or the
	// whole payload region for an engine that reads every cell. It separates "tried one correction and
	// it worked" from "read twelve thousand cells and one was wrong", which cost very different amounts.
	Considered int `json:"considered,omitempty"`

	// WorstMargin is the least confident cell considered, in the palette's weighted units.
	WorstMargin float64 `json:"worst_margin"`

	// Elapsed is how long this engine took, whether or not it succeeded.
	Elapsed time.Duration `json:"elapsed"`
}

// Result is a recovered frame and the account of how it was recovered.
type Result struct {
	Frame  *protocol.Frame
	Report Report
}

// Engine reads frames the decoder rejected.
type Engine interface {
	// Name is the stable configuration name, such as "go" or "sidecar".
	Name() string

	// Version identifies the implementation or its weights, and is logged with every recovery so a
	// change in behaviour can be attributed to a change in version.
	Version() string

	// Recover attempts to read the frame, returning ErrNotRecovered when it cannot.
	//
	// A returned frame is verified against the footer. An engine that cannot verify its own output
	// must refuse rather than return an unchecked guess.
	Recover(ctx context.Context, req Request) (*Result, error)
}
