package pipeline

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

// clock is a hand-wound clock, so the window can be tested without waiting through it.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	// A fixed instant rather than time.Now, so a test that accidentally depends on the wall clock
	// fails here rather than at some unlucky hour.
	return &clock{at: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func expectation(t *testing.T) (*laneExpectation, *clock) {
	t.Helper()
	c := newClock()
	return &laneExpectation{now: c.now}, c
}

// TestATwoLaneSenderIsNeverToldItIsMissingTwoFrames — the bug this exists for.
//
// A receiver configured for four lanes, pointed at a sender showing two. Before this, the aiming
// display compared two found against four configured and reported "move back until all 4 are inside
// the shot", outranking the fact that both frames were decoding. Backing away is the one move that
// makes a working aim stop working.
func TestATwoLaneSenderIsNeverToldItIsMissingTwoFrames(t *testing.T) {
	e, _ := expectation(t)

	for range 20 {
		require.Equal(t, 2, e.observe(2),
			"two is all that has ever been in shot, so two is all there is to expect")
	}
}

// TestADriftedAimIsStillReported: the useful half of the instrument has to survive the fix.
//
// Four lanes caught, then only two. That is a real aiming problem — the frames are being sent and are
// not arriving — and it must still be reported, which it is because four was seen a moment ago.
func TestADriftedAimIsStillReported(t *testing.T) {
	e, c := expectation(t)

	require.Equal(t, 4, e.observe(4))

	c.advance(time.Second)
	require.Equal(t, 4, e.observe(2), "the aim has drifted off two lanes that were in shot a second ago")
}

// TestOneBadPhotographDoesNotLowerTheBar.
//
// Hands shake and autofocus hunts, so a single capture catching one lane is ordinary. If that dropped
// the expectation, the drifted-aim warning would switch itself off at the first blur — which is
// exactly when an operator is most likely to be drifting.
func TestOneBadPhotographDoesNotLowerTheBar(t *testing.T) {
	e, c := expectation(t)

	e.observe(4)
	c.advance(100 * time.Millisecond)

	require.Equal(t, 4, e.observe(1), "one blurred frame is not evidence the sender changed")
}

// TestSwitchingTheSenderDownStopsTheNagging, which is the case the display page's lane control creates.
//
// Four lanes, then the operator switches the sender to one. The expectation has to come down, and
// within a few seconds rather than for as long as the camera runs.
func TestSwitchingTheSenderDownStopsTheNagging(t *testing.T) {
	e, c := expectation(t)

	e.observe(4)

	// Photographs keep arriving while the window runs out, as they would from a running camera.
	c.advance(laneWindow / 2)
	require.Equal(t, 4, e.observe(1), "still inside the window: four was real, and recent")

	c.advance(laneWindow)
	require.Equal(t, 1, e.observe(1), "the four-lane display is gone and the expectation has followed it")
}

// TestSwitchingTheSenderUpIsPickedUpAtOnce. Raising needs no window: one photograph holding four
// frames is proof that four are being sent, where lowering needs time to rule out a bad shot.
func TestSwitchingTheSenderUpIsPickedUpAtOnce(t *testing.T) {
	e, _ := expectation(t)

	require.Equal(t, 1, e.observe(1))
	require.Equal(t, 4, e.observe(4), "evidence of four frames is immediate and unambiguous")
}

// TestNothingSeenYetExpectsOne, rather than zero.
//
// Zero would be meaningless on the readout, and the switch that reads it treats anything above one as
// a tiled display — so one is also the value that asks it to stay quiet.
func TestNothingSeenYetExpectsOne(t *testing.T) {
	e, _ := expectation(t)

	require.Equal(t, 1, e.observe(0))
}

// TestTheWindowDoesNotGrowWithoutBound: this is fed by every photograph for as long as the camera
// runs, which on a page someone leaves open all afternoon is a leak if the old entries are merely
// skipped rather than dropped.
func TestTheWindowDoesNotGrowWithoutBound(t *testing.T) {
	e, c := expectation(t)

	for range 5000 {
		e.observe(2)
		c.advance(10 * time.Millisecond)
	}

	e.mu.Lock()
	held := len(e.seen)
	e.mu.Unlock()

	require.LessOrEqual(t, held, int(laneWindow/(10*time.Millisecond))+2,
		"only the window's worth of sightings should be retained")
}

// TestConcurrentObservationsAreSafe — the decode workers report from several goroutines at once, and
// this is read on the request path serving the aiming display.
func TestConcurrentObservationsAreSafe(t *testing.T) {
	e := newLaneExpectation()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				e.observe(i%4 + 1)
			}
		}()
	}
	wg.Wait()

	require.GreaterOrEqual(t, e.observe(1), 1)
}

// The seam between the expectation and the aiming verdict, which is where the bug actually bit.
//
// Both tests below hand measureLanes the *same photograph* — two frames, both decoding, well framed.
// The only difference is where the expectation came from. That is the whole of the fix: the picture
// was never the problem, the number it was being compared against was.

// TestATwoLaneDisplayReadsAsGoodOnceTheExpectationIsLearned.
func TestATwoLaneDisplayReadsAsGoodOnceTheExpectationIsLearned(t *testing.T) {
	e, _ := expectation(t)
	lanes := []*protocol.Geometry{frameAt(650), frameAt(650)}

	expected := e.observe(len(lanes))
	a := measureLanes(capture(), lanes, expected, true)

	require.Equal(t, StatusGood, a.Status,
		"two frames sent, two frames read, both decoding — there is nothing to correct: %s", a.Advice)
	require.Equal(t, 2, a.LanesExpected)
}

// TestTheSamePhotographIsAFaultWhenFourWereJustInShot — the signal that had to survive the fix.
func TestTheSamePhotographIsAFaultWhenFourWereJustInShot(t *testing.T) {
	e, _ := expectation(t)
	lanes := []*protocol.Geometry{frameAt(650), frameAt(650)}

	e.observe(4)
	expected := e.observe(len(lanes))
	a := measureLanes(capture(), lanes, expected, true)

	require.Equal(t, StatusTooClose, a.Status, "two of four lanes really are missing from this shot")
	require.Contains(t, a.Advice, "2 of 4 frames in view")
}
