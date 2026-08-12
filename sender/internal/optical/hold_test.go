package optical

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Holding the display is a statement about who may drive it.
//
// The display advances at the frame rate and waits for nobody, which is right for a transfer running
// unattended and wrong for anyone standing at the rig — aiming a camera, checking that one frame decodes,
// working out why a transfer is not crossing the gap. At a few frames a second the picture is gone before
// you have finished looking at it.
//
// The hold lives here rather than in the scheduler because there is one screen and there can be several
// schedulers: two concurrent transfers interleave on the same display, so "stop the display" cannot be a
// property of one of them. Live is also the object both sides already hold — the API server as its display,
// every scheduler as its sink — so nothing new has to be plumbed anywhere for both to see it.

// TestHoldIsReportedSoEveryViewerAgrees: a second browser tab, or a page reloaded mid-hold, has to be able
// to find out that the display is held rather than inferring it from frames that stopped arriving.
func TestHoldIsReportedSoEveryViewerAgrees(t *testing.T) {
	live := NewLive(&recorder{})

	held, since := live.HoldState()
	require.False(t, held)
	require.True(t, since.IsZero(), "an unheld display has no held-since time")

	live.Hold()

	held, since = live.HoldState()
	require.True(t, held)
	require.False(t, since.IsZero(), "a held display records when the hold began")
}

// TestReleaseLetsTheDisplayRunAgain — the other half, and the state the display spends its life in.
func TestReleaseLetsTheDisplayRunAgain(t *testing.T) {
	live := NewLive(&recorder{})

	live.Hold()
	live.Release()

	held, since := live.HoldState()
	require.False(t, held)
	require.True(t, since.IsZero(), "releasing clears the held-since time rather than leaving it stale")
}

// TestHoldingTwiceKeepsTheOriginalHoldTime: two operators, or two tabs, pressing the same button is not an
// error. Idempotent because the UI cannot reliably know the current state at the moment of the click, and
// reporting a conflict would mean it had to.
//
// The first hold's timestamp is the one that survives: the display has been held since then, and restarting
// the clock on a second press would under-report how long the channel has been stopped — which is precisely
// the number the scheduler uses to decide what not to charge the receiver for.
func TestHoldingTwiceKeepsTheOriginalHoldTime(t *testing.T) {
	live := NewLive(&recorder{})

	live.Hold()
	_, first := live.HoldState()
	live.Hold()
	_, second := live.HoldState()

	require.Equal(t, first, second, "a second hold must not restart the clock")
}

// TestReleasingWhenNotHeldIsHarmless — same argument in the other direction.
func TestReleasingWhenNotHeldIsHarmless(t *testing.T) {
	live := NewLive(&recorder{})

	require.NotPanics(t, func() { live.Release() })

	held, _ := live.HoldState()
	require.False(t, held)
}

// TestShowStillWorksWhileHeld is the property the whole feature rests on, and the reason the hold is
// advisory rather than enforced here.
//
// Stepping frames by hand puts them on the screen through this same Show path. A hold that blocked Show
// would therefore deadlock the one thing it exists to enable: the operator would freeze the display and
// then be unable to change what is on it. So the hold is a statement the scheduler honours and the manual
// step deliberately ignores — enforcement belongs to the caller that agreed to be bound by it.
func TestShowStillWorksWhileHeld(t *testing.T) {
	inner := &recorder{}
	live := NewLive(inner)

	live.Hold()
	require.NoError(t, live.Show(context.Background(), Frame{Number: 7, PNG: []byte("frame")}))

	frame, _, have := live.Current()
	require.True(t, have)
	require.Equal(t, 7, frame.Number, "a held display still shows what it is explicitly told to show")
	require.EqualValues(t, 1, inner.Shown())
}
