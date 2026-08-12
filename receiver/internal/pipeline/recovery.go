package pipeline

import (
	"sync"
	"sync/atomic"

	"github.com/opticaltransport/otp/receiver/ai/classify"
)

// recoveryCounters is what the recovery layer did during this capture session.
//
// Its own type rather than four more fields on Receiver, because these are read together, reported
// together, and reset together when the session rotates — and Receiver is already large enough that
// another handful of loose counters would obscure which state belongs to what.
type recoveryCounters struct {
	// attempted, recovered and candidates are atomic because prepare runs on every decode worker
	// at once, and these are incremented there.
	attempted  atomic.Uint64
	recovered  atomic.Uint64
	candidates atomic.Uint64

	// buckets counts how frames finished, by the stage they failed at. A map behind a mutex rather
	// than atomics because the key set is not fixed at compile time and the write rate is one per
	// frame — nothing next to a decode.
	mu      sync.Mutex
	buckets map[classify.Bucket]uint64
}

// count records how one frame finished.
func (c *recoveryCounters) count(b classify.Bucket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buckets == nil {
		c.buckets = make(map[classify.Bucket]uint64, 10)
	}
	c.buckets[b]++
}

// RecoveryStats is the recovery layer's report, for the API and the operator's panel.
type RecoveryStats struct {
	// Attempted is how many failed frames were searched, Recovered how many of those verified,
	// and Candidates the total corrections tried across all of them.
	//
	// Candidates matters more than it looks: rising candidates per recovery means the channel is
	// getting worse even while the recovered count holds up, which is the earliest warning
	// available that a camera is drifting out of focus.
	Attempted  uint64 `json:"attempted"`
	Recovered  uint64 `json:"recovered"`
	Candidates uint64 `json:"candidates"`

	// Buckets counts frames by the stage they failed at, including the ones that decoded. It is
	// what makes the panel actionable rather than merely informative: the same recovered-of-attempted
	// figure means aim the camera when the failures sit in no_quad and lower the density when they
	// sit in payload_crc, and those call for opposite actions.
	Buckets map[string]uint64 `json:"buckets"`
}

// stats snapshots the counters.
func (c *recoveryCounters) stats() RecoveryStats {
	out := RecoveryStats{
		Attempted:  c.attempted.Load(),
		Recovered:  c.recovered.Load(),
		Candidates: c.candidates.Load(),
		Buckets:    map[string]uint64{},
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.buckets {
		out.Buckets[string(k)] = v
	}
	return out
}

// reset clears the counters, called when the capture session rotates.
//
// Session-scoped for the same reason the decode counters on the camera page are: a lifetime figure
// reads healthy for the rest of the afternoon once any transfer has succeeded, and an operator
// aiming a camera that is currently recovering nothing needs to see that rather than an average
// over everything since the process started.
func (c *recoveryCounters) reset() {
	c.attempted.Store(0)
	c.recovered.Store(0)
	c.candidates.Store(0)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buckets = nil
}

// RecoveryStats reports what recovery has done in this capture session.
func (r *Receiver) RecoveryStats() RecoveryStats { return r.recovery.stats() }
