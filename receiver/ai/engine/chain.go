package engine

import (
	"context"
	"strings"
	"time"
)

// Chain tries engines in order and stops at the first that reads the frame.
//
// The order is cheapest-first, and that is what makes an expensive engine affordable: a deterministic
// search costing microseconds runs on every failure, and a model server costing a round trip runs only
// on the frames the cheap one could not read. Reversing it would pay the expensive price for every
// frame, including the ones a candidate search would have fixed immediately.
//
// A chain is itself an Engine, so a caller cannot tell one engine from five and nothing about the
// decode path changes when the composition does.
type Chain struct {
	engines []Engine
}

// NewChain returns a chain over the engines given, in the order they should be tried.
func NewChain(engines ...Engine) *Chain {
	kept := make([]Engine, 0, len(engines))
	for _, e := range engines {
		if e != nil {
			kept = append(kept, e)
		}
	}
	return &Chain{engines: kept}
}

func (c *Chain) Name() string {
	if len(c.engines) == 0 {
		return "none"
	}
	names := make([]string, 0, len(c.engines))
	for _, e := range c.engines {
		names = append(names, e.Name())
	}
	return strings.Join(names, "+")
}

// Version joins the members' versions, so one string identifies the whole composition.
//
// Recorded per recovery for exactly one reason: a change in recovered count is only attributable if
// what was running is on the record beside it. "go/1" and "sidecar/v3+go/1" recovering the same number
// of frames is the finding that a model is earning nothing.
func (c *Chain) Version() string {
	if len(c.engines) == 0 {
		return "0"
	}
	versions := make([]string, 0, len(c.engines))
	for _, e := range c.engines {
		versions = append(versions, e.Name()+"="+e.Version())
	}
	return strings.Join(versions, ",")
}

// Recover tries each engine in turn.
//
// A context cancellation stops the chain rather than falling through to the next engine: the receiver
// is shutting down, and the next engine is not going to do better with less time.
func (c *Chain) Recover(ctx context.Context, req Request) (*Result, error) {
	started := time.Now()
	for _, e := range c.engines {
		res, err := e.Recover(ctx, req)
		if err == nil {
			// Total elapsed, not the winner's own: the caller waited for every rung that came first,
			// and reporting only the successful one would understate the cost of the composition.
			res.Report.Elapsed = time.Since(started)
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, ErrNotRecovered
}
