package optical_test

import (
	"context"
	"github.com/google/uuid"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/optical"
)

// Two transfers sharing one screen have to take turns.
//
// There is one display and a scheduler per transfer, so with two files in flight two loops call the
// sink on their own tickers. Before the turnstile each simply replaced whatever the other had just
// put up — frequently within milliseconds, well inside the time a camera needs to photograph it. The
// consequence is not a cosmetic flicker: an unphotographed frame is an unacknowledged chunk, so both
// transfers retransmit for ever and two files together finish slower than the same two run in
// sequence.

// countingSink records what reached the channel and when.
type countingSink struct {
	mu     sync.Mutex
	at     []time.Time
	frames []optical.Frame
}

func (c *countingSink) Name() string { return "counting" }
func (c *countingSink) Show(_ context.Context, f optical.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = append(c.at, time.Now())
	c.frames = append(c.frames, f)
	return nil
}

func (c *countingSink) Shown() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(len(c.at))
}
func (c *countingSink) Close() error { return nil }

func (c *countingSink) gaps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(c.at); i++ {
		out = append(out, c.at[i].Sub(c.at[i-1]))
	}
	return out
}

// A frame keeps the screen for the time it was given, however many callers are waiting.
func TestAFrameKeepsTheScreenForItsOwnTime(t *testing.T) {
	inner := &countingSink{}
	live := optical.NewLive(inner)
	ctx := context.Background()

	const hold = 40 * time.Millisecond

	// Two transfers, both asking for the screen as fast as they can. This is the pathological version
	// of what two schedulers do; a real one waits on its own ticker between rounds.
	var wg sync.WaitGroup
	for transfer := range 2 {
		wg.Add(1)
		go func(transfer int) {
			defer wg.Done()
			for range 3 {
				require.NoError(t, live.ShowFor(ctx, optical.Frame{Number: transfer}, hold))
			}
		}(transfer)
	}
	wg.Wait()

	require.Equal(t, int64(6), inner.Shown(), "every frame offered should reach the channel")

	// Each frame after the first waited for its predecessor's time. A little slack for scheduling:
	// the property under test is that frames are spaced, not that the clock is exact.
	for i, gap := range inner.gaps() {
		assert.GreaterOrEqual(t, gap, hold-5*time.Millisecond,
			"frame %d replaced its predecessor after only %s, inside the %s it was given", i+1, gap, hold)
	}
}

// Both transfers get the screen. The turnstile must share it, not hand it to whoever asks first.
func TestBothTransfersReachTheScreen(t *testing.T) {
	inner := &countingSink{}
	live := optical.NewLive(inner)
	ctx := context.Background()

	var wg sync.WaitGroup
	for transfer := range 2 {
		wg.Add(1)
		go func(transfer int) {
			defer wg.Done()
			for range 4 {
				require.NoError(t, live.ShowFor(ctx, optical.Frame{Number: transfer}, 10*time.Millisecond))
			}
		}(transfer)
	}
	wg.Wait()

	seen := map[int]int{}
	inner.mu.Lock()
	for _, f := range inner.frames {
		seen[f.Number]++
	}
	inner.mu.Unlock()

	assert.Equal(t, 4, seen[0], "the first transfer should have shown all of its frames")
	assert.Equal(t, 4, seen[1], "the second transfer should not have been starved by the first")
}

// A cancelled transfer stops waiting rather than holding a slot it will never use.
func TestAWaitingTransferGivesUpWhenCancelled(t *testing.T) {
	inner := &countingSink{}
	live := optical.NewLive(inner)

	// Take the screen for a long time.
	require.NoError(t, live.ShowFor(context.Background(), optical.Frame{Number: 1}, 5*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- live.ShowFor(ctx, optical.Frame{Number: 2}, time.Second) }()

	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled transfer was still waiting for the display")
	}

	assert.Equal(t, int64(1), inner.Shown(), "the cancelled frame must not reach the channel")
}

// Show without a reservation is unchanged, so a manual step from the operator's page never queues
// behind a transfer that is pacing itself.
func TestAManualShowDoesNotWait(t *testing.T) {
	inner := &countingSink{}
	live := optical.NewLive(inner)
	ctx := context.Background()

	require.NoError(t, live.ShowFor(ctx, optical.Frame{Number: 1}, time.Hour))

	start := time.Now()
	require.NoError(t, live.Show(ctx, optical.Frame{Number: 2}))
	assert.Less(t, time.Since(start), 250*time.Millisecond,
		"an operator stepping the display by hand should not wait behind a transfer's reservation")
}

// A finished transfer takes its frame down, and only its own.
//
// Left up, the last frame of a completed transfer goes on being photographed, goes on locating, and goes on
// being reported as a chunk the receiver already holds — so from the camera page a finished transfer is
// indistinguishable from a running one and an operator has no signal that they can stop.
func TestAFinishedTransferClearsTheScreen(t *testing.T) {
	inner := &countingSink{}
	live := optical.NewLive(inner)
	ctx := context.Background()

	transfer := uuid.New()
	require.NoError(t, live.Show(ctx, optical.Frame{Number: 3, Transmission: transfer}))

	_, _, have := live.Current()
	require.True(t, have)

	assert.True(t, live.ClearFor(transfer), "the screen was showing this transfer's frame")

	frame, _, have := live.Current()
	require.True(t, have, "a clear is published as a frame, not as an absence — see Frame.Cleared")
	assert.True(t, frame.Cleared, "and it says the screen is empty")
	assert.Empty(t, frame.PNG, "with no picture on it")
}

// One transfer finishing must not blank another's frame.
//
// Two transfers share this screen by taking turns, so the one that finishes first would otherwise clear a
// frame its neighbour put up a moment earlier — costing that transfer a display slot, and a chunk if the
// camera happened to fire just then.
func TestFinishingDoesNotBlankAnotherTransfersFrame(t *testing.T) {
	inner := &countingSink{}
	live := optical.NewLive(inner)
	ctx := context.Background()

	finishing, running := uuid.New(), uuid.New()

	// The other transfer has the screen when this one finishes.
	require.NoError(t, live.Show(ctx, optical.Frame{Number: 9, Transmission: running}))

	assert.False(t, live.ClearFor(finishing), "the screen belongs to the other transfer")

	frame, _, have := live.Current()
	require.True(t, have)
	assert.False(t, frame.Cleared, "the running transfer's frame stays up")
	assert.Equal(t, running, frame.Transmission)

	// And when that one finishes too, the screen does clear: the last to finish tidies up.
	assert.True(t, live.ClearFor(running))
	frame, _, _ = live.Current()
	assert.True(t, frame.Cleared)
}

// A clear advances the sequence, so a viewer parked in a long poll learns about it.
func TestClearingWakesAViewerWaitingForTheNextFrame(t *testing.T) {
	inner := &countingSink{}
	live := optical.NewLive(inner)
	ctx := context.Background()

	transfer := uuid.New()
	require.NoError(t, live.Show(ctx, optical.Frame{Number: 1, Transmission: transfer}))
	shown, _, _ := live.Current()

	waited := make(chan optical.Frame, 1)
	go func() {
		next, _, ok := live.Next(ctx, shown.Sequence)
		if ok {
			waited <- next
		}
	}()

	// Let the waiter park before the screen changes under it.
	time.Sleep(20 * time.Millisecond)
	require.True(t, live.ClearFor(transfer))

	select {
	case next := <-waited:
		assert.True(t, next.Cleared, "the waiter should be told the screen is empty")
		assert.Greater(t, next.Sequence, shown.Sequence, "which needs its own sequence to be delivered at all")
	case <-time.After(2 * time.Second):
		t.Fatal("a viewer following the display was never told it had been cleared")
	}
}

// Clearing an already-clear screen does nothing, so a retry or a second scheduler cannot loop.
func TestClearingTwiceIsHarmless(t *testing.T) {
	live := optical.NewLive(&countingSink{})
	transfer := uuid.New()

	require.NoError(t, live.Show(context.Background(), optical.Frame{Transmission: transfer}))
	require.True(t, live.ClearFor(transfer))
	assert.False(t, live.ClearFor(transfer), "there is nothing left of this transfer to take down")
}
