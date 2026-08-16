package pipeline

import (
	"sync"
	"time"
)

// How many frames the receiver should expect to see in one photograph.
//
// This used to be read straight from configuration, and that was wrong in a way that only became
// reachable once the sender grew a control for it. The aiming display compares lanes found against
// lanes expected and, when it is short, reports "2 of 4 frames in view — move back until all 4 are
// inside the shot". That verdict outranks every other, including "frames are decoding": an operator
// whose sender is showing two lanes against a receiver configured for four is told to back away from
// a display that is working, and keeps backing away, because the two missing frames are not missing.
// They were never sent.
//
// Configuration cannot answer this. The lane count is the sender's, it can be changed mid-transfer
// from the display page, and the two applications share a protocol and a directory — there is no
// channel through a camera by which the sender could say so. What the receiver has is evidence: the
// most frames it has actually managed to hold in one photograph lately. Comparing against that keeps
// the useful half of the instrument — an aim that was catching four and is now catching two really
// has drifted, and should say so — while a sender that is only showing two produces no complaint at
// all, because two is all there has ever been to catch.
//
// The configured count still bounds the search. This decides what to *expect*, not what to look for.

// laneWindow is how far back the expectation looks.
//
// It is a compromise between two failures. Too short and a single blurred photograph drops the
// expectation to whatever that one frame caught, which silences the drifted-aim warning exactly when
// the aim is drifting. Too long and an operator who switches the sender from four lanes to one is
// nagged about three missing frames until it expires. Four seconds is many photographs at any capture
// rate this receiver runs at, so a stumble cannot move it, and it is about as long as someone will
// spend looking at the readout after changing a setting before deciding it is broken.
const laneWindow = 4 * time.Second

// laneSighting is one photograph's count and when it was taken.
type laneSighting struct {
	lanes int
	at    time.Time
}

// laneExpectation is the running high-water mark of frames seen in one photograph.
//
// Held by the receiver and observed from the decode workers, so it locks: several workers finish
// captures concurrently and all of them report.
type laneExpectation struct {
	mu   sync.Mutex
	seen []laneSighting

	// now is injectable so the window can be tested without sleeping through it.
	now func() time.Time
}

func newLaneExpectation() *laneExpectation {
	return &laneExpectation{now: time.Now}
}

// observe records one photograph's lane count and returns what to expect now.
//
// Never returns less than one. Zero expected would be nonsense to display, and the switch that reads
// it treats anything above one as a tiled display — so one is also the value that asks it to say
// nothing, which is right when nothing has been seen yet.
func (e *laneExpectation) observe(found int) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	at := e.now()
	e.seen = append(e.seen, laneSighting{lanes: found, at: at})

	// Dropped rather than kept and skipped: without this the slice grows for as long as the camera
	// runs, which at ten photographs a second is a slow leak on a page people leave open.
	cutoff := at.Add(-laneWindow)
	kept := e.seen[:0]
	best := 1
	for _, s := range e.seen {
		if s.at.Before(cutoff) {
			continue
		}
		kept = append(kept, s)
		if s.lanes > best {
			best = s.lanes
		}
	}
	e.seen = kept
	return best
}
