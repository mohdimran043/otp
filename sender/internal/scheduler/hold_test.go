package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/optical"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// A held display, and the time it must not charge the receiver for.
//
// The scheduler treats silence as loss: a chunk displayed and not acknowledged within Ack.Timeout is assumed
// missed, re-sent, and counted against a retry ceiling that fails the transfer when it runs out. That is the
// right reading of silence when the display is running, because a frame the camera missed produces no report
// and there is no other signal to go on.
//
// It is exactly the wrong reading while the display is held. The receiver saw nothing new because nothing new
// was shown to it, and its silence says so. Without forgiving the held time, a hold longer than the timeout
// makes every outstanding chunk look lost the instant the display resumes — every one re-sent, attempts
// climbing together, and a long enough hold walking the transfer into ErrStalled while the operator is
// carefully stepping through it. The feature would destroy the transfers it exists to help.

// heldSink is a sink whose hold the test drives.
type heldSink struct {
	optical.Sink
	held bool
}

func (h *heldSink) Name() string                              { return "held" }
func (h *heldSink) Show(context.Context, optical.Frame) error { return nil }
func (h *heldSink) Shown() int64                              { return 0 }
func (h *heldSink) Close() error                              { return nil }
func (h *heldSink) HoldState() (bool, time.Time) {
	if !h.held {
		return false, time.Time{}
	}
	return true, time.Now()
}

// TestForgivingAHoldStopsItLookingLikeLoss is the regression test for the whole design.
//
// A chunk shown, then a hold twice as long as the acknowledgement timeout, then release. Without forgiveness
// the chunk is a retransmission the moment the display resumes; with it, the chunk is as fresh as it was when
// the hold began. If this test is ever deleted along with forgiveHold, transfers will fail on long holds and
// the cause will look like a channel problem rather than a scheduler one.
func TestForgivingAHoldStopsItLookingLikeLoss(t *testing.T) {
	cfg := config.Default()
	cfg.Ack.Timeout = 30 * time.Second

	chunk := store.Chunk{ID: uuid.New(), ESI: 0}
	frame := store.Frame{ID: uuid.New(), ChunkID: &chunk.ID, FrameNumber: 0}
	byChunk := map[uuid.UUID]store.Frame{chunk.ID: frame}

	s := New(nil, nil, &heldSink{}, config.NewWatcher("", cfg), zapNop())

	// Shown a minute ago, and held for that whole minute.
	held := 2 * cfg.Ack.Timeout
	s.sent[chunk.ESI] = time.Now().Add(-held)

	_, priority, err := s.choose(context.Background(), []store.Chunk{chunk}, byChunk, cfg, nil)
	require.NoError(t, err)
	require.Equal(t, PriorityRetransmit, priority,
		"without forgiveness a chunk older than the timeout is a retransmission — this is the failure mode")

	s.forgiveHold(held)

	_, priority, err = s.choose(context.Background(), []store.Chunk{chunk}, byChunk, cfg, nil)
	require.NoError(t, err)
	require.Equal(t, PriorityKeepAlive, priority,
		"held time is not the receiver's silence: after forgiveness the chunk is not treated as lost")
}

// TestForgivingAHoldLeavesAttemptsAlone: forgiveness is about when a chunk was last seen, not about how many
// times it has been tried. A hold must not hand back retries the channel genuinely spent.
func TestForgivingAHoldLeavesAttemptsAlone(t *testing.T) {
	s := New(nil, nil, &heldSink{}, config.NewWatcher("", config.Default()), zapNop())

	s.noteSent(3, PriorityFresh)
	s.noteSent(3, PriorityRetransmit)
	require.Equal(t, 2, s.attemptsOf(3))

	s.forgiveHold(time.Minute)

	require.Equal(t, 2, s.attemptsOf(3), "a hold forgives time, not attempts")
}

// TestParkingReturnsAtOnceWhenNothingIsHeld — the ordinary case, which must cost nothing.
func TestParkingReturnsAtOnceWhenNothingIsHeld(t *testing.T) {
	s := New(nil, nil, &heldSink{held: false}, config.NewWatcher("", config.Default()), zapNop())

	started := time.Now()
	held, err := s.parkWhileHeld(context.Background(), func() error { return nil })

	require.NoError(t, err)
	require.Zero(t, held)
	require.Less(t, time.Since(started), 100*time.Millisecond, "an unheld display must not be waited on")
}

// TestParkingEndsWhenTheDisplayIsReleased, reporting roughly how long it waited so the caller can forgive it.
func TestParkingEndsWhenTheDisplayIsReleased(t *testing.T) {
	sink := &heldSink{held: true}
	s := New(nil, nil, sink, config.NewWatcher("", config.Default()), zapNop())

	go func() {
		time.Sleep(150 * time.Millisecond)
		sink.held = false
	}()

	held, err := s.parkWhileHeld(context.Background(), func() error { return nil })

	require.NoError(t, err)
	require.GreaterOrEqual(t, held, 100*time.Millisecond, "the wait is reported so it can be forgiven")
}

// TestParkingStillObeysACancel is why parking is a wait with a wake-up rather than a blocking receive.
//
// An operator who holds the display and then cancels the transfer should not have to release it first to be
// obeyed. The status check runs while parked, and its error ends the park.
func TestParkingStillObeysACancel(t *testing.T) {
	s := New(nil, nil, &heldSink{held: true}, config.NewWatcher("", config.Default()), zapNop())

	_, err := s.parkWhileHeld(context.Background(), func() error { return ErrCancelled })

	require.ErrorIs(t, err, ErrCancelled, "a cancel issued while held has to reach the loop")
}

// TestParkingEndsWhenTheContextDoes — shutdown must not wait on an operator who went home.
func TestParkingEndsWhenTheContextDoes(t *testing.T) {
	s := New(nil, nil, &heldSink{held: true}, config.NewWatcher("", config.Default()), zapNop())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := s.parkWhileHeld(ctx, func() error { return nil })

	require.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
}

// TestASinkWithoutAHoldIsNeverParkedOn: the hold is an optional capability, discovered by assertion, so a
// sink that does not offer one keeps working exactly as before.
func TestASinkWithoutAHoldIsNeverParkedOn(t *testing.T) {
	s := New(nil, nil, plainSink{}, config.NewWatcher("", config.Default()), zapNop())

	held, err := s.parkWhileHeld(context.Background(), func() error { return nil })

	require.NoError(t, err)
	require.Zero(t, held)
}

// plainSink implements only the Sink interface, with no hold.
type plainSink struct{}

func (plainSink) Name() string                              { return "plain" }
func (plainSink) Show(context.Context, optical.Frame) error { return nil }
func (plainSink) Shown() int64                              { return 0 }
func (plainSink) Close() error                              { return nil }

// zapNop is a logger that discards, so the tests say nothing.
func zapNop() *zap.Logger { return zap.NewNop() }
