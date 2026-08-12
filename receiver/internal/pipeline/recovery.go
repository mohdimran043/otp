package pipeline

import (
	"sync"
	"sync/atomic"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/receiver/ai/engine"
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
	// Attempted is how many failed frames were offered to the engine — not how many it worked on.
	//
	// The distinction is deliberate and worth knowing when reading the ratio. Which failures an engine
	// can help is the engine's own business: a candidate search needs a geometry and declines without
	// one, while a model server may make fiducials findable where they were not, so it wants exactly
	// the frames the search refuses. The pipeline therefore offers everything and lets each engine
	// decide, which keeps the decode path free of knowledge about what is installed behind it.
	//
	// The consequence is that Attempted includes frames no engine could ever have helped, so the
	// recovered-of-attempted ratio is a floor rather than a hit rate. Buckets is where the real
	// reading is: it says which stages the failures were at, and RecoveredFrom on the corpus report
	// says which of those the engine actually rescued.
	//
	// Recovered is how many verified, and Candidates the total corrections tried across all of them.
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

	// Engine and Version name what did the recovering. Reported so a change in the recovered count is
	// attributable: the same figure under "go" and under "sidecar+go" is the finding that a model is
	// earning nothing.
	Engine  string `json:"engine"`
	Version string `json:"version"`
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

// RecoveryStats reports what recovery has done in this capture session, and which engine did it.
func (r *Receiver) RecoveryStats() RecoveryStats {
	out := r.recovery.stats()
	out.Engine, out.Version = r.EngineInfo()
	return out
}

// EngineInfo names the engine and version in force, for the API and the logs.
func (r *Receiver) EngineInfo() (name, version string) {
	if r.engine == nil {
		return "none", "0"
	}
	return r.engine.Name(), r.engine.Version()
}

// UseEngine replaces the recovery engine.
//
// Called before Run, from the startup path that resolved the configured engine and is willing to fail
// on it. Not safe to call while frames are being decoded, and not needed there: the engine is a
// property of the deployment, not of a session.
func (r *Receiver) UseEngine(e engine.Engine) {
	if e == nil {
		return
	}
	r.engine = e
}
