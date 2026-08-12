package engine

import (
	"context"
	"time"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/receiver/ai/soft"
)

// Go is the deterministic engine: a bounded candidate search over the cells the sampler was least
// sure about, verified against the footer.
//
// It is the baseline in the sense that matters — it ships, it needs no weights, no device and no
// network, and any learned engine has to beat it on the same corpus to justify its existence. It is
// not a placeholder.
type Go struct {
	opts soft.Options
}

// NewGo returns the deterministic engine.
func NewGo(opts soft.Options) *Go { return &Go{opts: opts} }

func (g *Go) Name() string { return "go" }

// Version names the strategy rather than a model, and changes when the search changes in a way that
// would alter which frames it recovers.
//
// "margin-ordered-subset" is exactly what it does: rank payload cells by how close the palette
// decision was, then try subsets of the least confident ones in order of total margin spent.
func (g *Go) Version() string { return "margin-ordered-subset/1" }

// Recover searches for a correction that satisfies the footer.
//
// Refuses without a geometry, and refuses on buckets a geometry-based search cannot address — a frame
// whose footer is unreadable has no oracle to check a correction against, so there is nothing to search
// toward. Both are cheap refusals by design: this runs on the decode path.
func (g *Go) Recover(ctx context.Context, req Request) (*Result, error) {
	if req.Geometry == nil || !classify.Recoverable(req.Bucket) {
		return nil, ErrNotRecovered
	}
	// Context is honoured before the expensive sampling pass rather than after. The search itself is
	// bounded by its own budget, so this only matters for a receiver shutting down mid-frame.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	started := time.Now()
	res, err := soft.Recover(req.Geometry, req.Image, g.opts)
	if err != nil {
		return nil, ErrNotRecovered
	}
	return &Result{
		Frame: res.Frame,
		Report: Report{
			Engine:      g.Name(),
			Version:     g.Version(),
			Stage:       "soft",
			Flips:       res.Flips,
			Candidates:  res.Candidates,
			WorstMargin: res.WorstMargin,
			Elapsed:     time.Since(started),
		},
	}, nil
}

// Null recovers nothing.
//
// It exists so that "recovery is off" is an engine rather than a branch in the pipeline. A nil check
// around the call site would have to be repeated everywhere the engine is used, and the one place it
// was forgotten would be a nil dereference on the decode path.
type Null struct{}

func NewNull() *Null { return &Null{} }

func (*Null) Name() string    { return "none" }
func (*Null) Version() string { return "0" }

func (*Null) Recover(context.Context, Request) (*Result, error) { return nil, ErrNotRecovered }
