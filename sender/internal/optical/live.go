package optical

import (
	"context"
	"sync"
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

// Show displays a frame and publishes it as the current one.
func (l *Live) Show(ctx context.Context, frame Frame) error {
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

// Shown is how many frames the wrapped sink has displayed.
func (l *Live) Shown() int64 { return l.inner.Shown() }

// Close releases the wrapped sink.
func (l *Live) Close() error { return l.inner.Close() }

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
