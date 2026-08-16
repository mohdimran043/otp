package optical

import (
	"context"
	"github.com/google/uuid"
	"sync"
	"sync/atomic"
	"time"
)

// Live wraps a sink and remembers the frame currently on display.
//
// It exists because "what is on the screen right now" is a question with no answer anywhere else in
// the sender. The scheduler knows what it decided to show and the store knows what was rendered, but
// neither knows what a camera pointed at the display would be looking at this instant — and that is
// precisely what an operator lining up a camera, and a receiver pulling frames over HTTP, both need.
//
// It is a decorator rather than a sink of its own so that it works whatever the real channel is: a
// directory today, a monitor under OpenGL later. The frame is published only after the wrapped sink
// has accepted it, so "current" means displayed rather than intended.
type Live struct {
	inner Sink

	mu      sync.Mutex
	current Frame
	shownAt time.Time
	have    bool

	// changed is closed and replaced whenever a frame is published. Closing a channel wakes every
	// waiter at once, which is what a display needs: one frame goes to every camera, browser tab, and
	// receiver watching, and none of them should be able to starve another by arriving first.
	changed chan struct{}

	// held stops the display advancing on its own, and heldSince is when that started.
	//
	// It is here, beside the current frame, because there is one screen and there can be several
	// schedulers: two concurrent transfers interleave on the same display, so "stop the display" is not a
	// property any one of them can own. Both sides already hold this object — the API server as its
	// display, every scheduler as its sink — so putting the state here is what lets them agree without
	// either learning about the other.
	//
	// Guarded by mu, the same lock as the current frame, because a viewer asking what is on the screen and
	// whether it is held wants one answer rather than two taken a moment apart.
	held      bool
	heldSince time.Time

	// turnstile and freeAt are how concurrent transfers take turns on the one screen.
	//
	// freeAt is when the frame currently up has had the time it was given and may be replaced. Kept
	// under its own lock rather than mu: a caller waiting for the screen must not block a browser
	// asking what is on it, and holding mu across a wait would do exactly that.
	turnstile sync.Mutex
	freeAt    time.Time

	// next assigns display sequence numbers, and it lives here because there is exactly one display.
	//
	// This was a per-scheduler counter, and that was a real bug rather than an untidiness. A scheduler
	// runs per transmission, so two concurrent transfers each began at one — and since the file sink
	// names frames by sequence, both wrote 000000000001.png and one silently overwrote the other. The
	// symptom was a transfer that stalled with every frame decoded and no chunk acknowledged, because
	// its manifest had been replaced on disk before the receiver read it.
	//
	// One counter per display is the only arrangement that can be right: the sequence describes the
	// channel, not the file crossing it, and the channel is shared.
	next atomic.Int64
}

// NewLive returns a sink that records what it displays.
func NewLive(inner Sink) *Live {
	return &Live{inner: inner, changed: make(chan struct{})}
}

// Name returns the wrapped sink's name. The decorator is not a channel of its own and should not
// claim to be one in logs or diagnostics.
func (l *Live) Name() string { return l.inner.Name() }

// Inner returns the wrapped sink, for callers that need the concrete channel.
func (l *Live) Inner() Sink { return l.inner }

// ShowFor displays a frame and keeps the screen for it for at least hold.
//
// There is one screen and there can be several schedulers, one per transfer. Show on its own only
// stops them corrupting each other's *bookkeeping*; it does nothing about the screen itself, so two
// concurrent transfers each put a frame up on their own ticker and each replaced the other's — often
// within milliseconds. A frame the camera never had time to photograph is a chunk that is never
// acknowledged, so both transfers retransmit indefinitely and neither finishes. Two files together
// went slower than the same two files one after the other.
//
// So a frame reserves the display for the time it was meant to be visible, and the next caller waits.
// That is what makes concurrent transfers interleave rather than collide: each scheduler still runs
// its own loop and asks for the screen when it has something to show, and the turnstile hands it over
// one whole frame at a time. Total display bandwidth is fixed and is now *shared* rather than
// contended — two transfers each get half the frames, which is the honest division of one screen.
//
// A zero or negative hold is the old behaviour and reserves nothing, for callers that are not pacing
// a transfer: a manual step from the operator's display page should not be made to queue.
func (l *Live) ShowFor(ctx context.Context, frame Frame, hold time.Duration) error {
	if hold > 0 {
		if err := l.reserve(ctx, hold); err != nil {
			return err
		}
	}
	return l.Show(ctx, frame)
}

// reserve waits for the current frame's time to run out and claims the next slot.
//
// The wait is on the context as well as the clock, so a cancelled transfer does not sit here holding a
// slot it will never use.
func (l *Live) reserve(ctx context.Context, hold time.Duration) error {
	for {
		l.turnstile.Lock()
		wait := time.Until(l.freeAt)
		if wait <= 0 {
			l.freeAt = time.Now().Add(hold)
			l.turnstile.Unlock()
			return nil
		}
		l.turnstile.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// Show assigns the frame its display sequence, displays it, and publishes it as the current one.
//
// The sequence is assigned here rather than taken from the caller, so that a caller cannot get it wrong
// and two callers cannot collide. Whatever a scheduler put in the field is overwritten.
//
// Show does not reserve the screen. A caller pacing a transfer wants ShowFor.
func (l *Live) Show(ctx context.Context, frame Frame) error {
	frame.Sequence = l.next.Add(1)

	if err := l.inner.Show(ctx, frame); err != nil {
		return err
	}

	// The decoded image is dropped on the way in. Holding it would keep a live reference to a fully
	// expanded bitmap for as long as the frame stays on screen, and everything that reads a published
	// frame wants the encoded bytes: a browser renders the PNG, an HTTP source forwards it unchanged.
	frame.Image = nil

	l.mu.Lock()
	l.current, l.shownAt, l.have = frame, time.Now(), true
	previous := l.changed
	l.changed = make(chan struct{})
	l.mu.Unlock()

	close(previous)
	return nil
}

// Hold stops the display advancing on its own, so that an operator can look at one frame.
//
// It does not stop Show, and that is deliberate rather than an oversight. Stepping frames by hand puts them
// on the screen through the same Show path, so a hold that blocked it would deadlock the one thing it exists
// to enable — the operator would freeze the display and then be unable to change what is on it. The hold is
// a statement about who may drive the display: honoured by the scheduler, ignored by a deliberate manual
// show. Enforcement belongs to the caller that agreed to be bound by it.
//
// Idempotent, and the first hold's timestamp is the one that survives. Two operators or two browser tabs
// pressing the same button is not an error, and restarting the clock on the second press would under-report
// how long the channel has been stopped — which is the number the scheduler uses to decide what not to
// charge the receiver for.
func (l *Live) Hold() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return
	}
	l.held, l.heldSince = true, time.Now()
}

// Release lets the display advance again. Idempotent, for the same reason Hold is.
func (l *Live) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held, l.heldSince = false, time.Time{}
}

// HoldState reports whether the display is held and since when.
//
// The time is part of the answer because the useful question is rarely "is it held" on its own: a scheduler
// resuming needs to know how long it was stopped, and an operator looking at a still picture needs to be
// able to tell a hold from a channel that has died.
func (l *Live) HoldState() (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held, l.heldSince
}

// Shown is how many frames the wrapped sink has displayed.
func (l *Live) Shown() int64 { return l.inner.Shown() }

// Close releases the wrapped sink.
func (l *Live) Close() error { return l.inner.Close() }

// ClearFor blanks the display, if what is on it belongs to the transmission given.
//
// A finished transfer should leave nothing on the screen. Leaving its last frame up is not merely untidy:
// the receiver keeps photographing it, keeps locating a perfectly good frame, and keeps reporting a chunk it
// already has — so a completed transfer looks, from the camera page, exactly like one still running. An
// operator watching the aiming display has no way to tell the difference.
//
// Conditional on the transmission because there may be another one displaying. Two transfers share this
// screen by taking turns, so a scheduler finishing must not blank a frame its neighbour put up a moment ago
// — it would cost that transfer a display slot and, if the camera happened to fire then, a chunk. Blanking
// only what belongs to the caller means the last transfer to finish is the one that clears the screen, which
// is the behaviour wanted in both cases.
//
// Returns whether it cleared, which is worth knowing at the call site: "cleared" and "someone else is still
// using the screen" are both correct outcomes and a log line that could not tell them apart would make a
// perfectly ordinary concurrent transfer look like a failure to tidy up.
func (l *Live) ClearFor(transmission uuid.UUID) bool {
	l.mu.Lock()
	if !l.have || l.current.Cleared || l.current.Transmission != transmission {
		l.mu.Unlock()
		return false
	}
	// Published as a frame of its own, with a sequence, so a long poll sees the change. See Frame.Cleared.
	l.current = Frame{Sequence: l.next.Add(1), Transmission: transmission, Cleared: true}
	l.have, l.shownAt = true, time.Now()
	previous := l.changed
	l.changed = make(chan struct{})
	l.mu.Unlock()

	// Waking the long-poll matters as much as the state does. A viewer is parked in Next waiting for the
	// screen to change, and clearing without this leaves it holding the frame that is no longer there until
	// its own timeout expires.
	close(previous)
	return true
}

// Current returns the frame on display, when it was shown, and whether there is one at all.
func (l *Live) Current() (Frame, time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current, l.shownAt, l.have
}

// Next blocks until a frame later than after is displayed, and returns it.
//
// It reports false when the context ends first, which is how a long-polling request distinguishes
// "nothing new yet, ask again" from "here is the next frame". A caller passing an after it has not
// seen — zero, or a sequence from a previous run of the display — gets the current frame immediately,
// because a viewer that has fallen behind wants the screen as it is now, not a queue of history.
func (l *Live) Next(ctx context.Context, after int64) (Frame, time.Time, bool) {
	for {
		l.mu.Lock()
		frame, at, have, wait := l.current, l.shownAt, l.have, l.changed
		l.mu.Unlock()

		if have && frame.Sequence > after {
			return frame, at, true
		}

		select {
		case <-wait:
			// A frame was published; go round and look at it.
		case <-ctx.Done():
			return Frame{}, time.Time{}, false
		}
	}
}
