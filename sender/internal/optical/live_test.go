package optical

import (
	"context"
	"errors"
	"image"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recorder is a sink that accepts frames and can be made to fail.
type recorder struct {
	mu     sync.Mutex
	frames []Frame
	err    error
	shown  int64
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) Show(_ context.Context, frame Frame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.frames = append(r.frames, frame)
	r.shown++
	return nil
}

func (r *recorder) Shown() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shown
}

func (r *recorder) Close() error { return nil }

func TestLivePublishesWhatWasDisplayed(t *testing.T) {
	inner := &recorder{}
	live := NewLive(inner)

	_, _, have := live.Current()
	require.False(t, have, "nothing has been displayed yet, so there is nothing to report")

	require.NoError(t, live.Show(context.Background(), Frame{Sequence: 1, Number: 0, PNG: []byte("one")}))

	frame, at, have := live.Current()
	require.True(t, have)
	require.Equal(t, int64(1), frame.Sequence)
	require.Equal(t, []byte("one"), frame.PNG)
	require.WithinDuration(t, time.Now(), at, time.Minute)

	require.Len(t, inner.frames, 1, "the frame must still reach the real channel")
	require.Equal(t, "recorder", live.Name(), "the decorator must not claim to be a channel of its own")
	require.Equal(t, int64(1), live.Shown())
}

// TestLiveDoesNotPublishAFrameTheChannelRefused is the reason publication happens after the wrapped
// Show rather than before it. A viewer asking what is on the display must not be told about a frame
// that never got there.
func TestLiveDoesNotPublishAFrameTheChannelRefused(t *testing.T) {
	inner := &recorder{err: errors.New("the disk is full")}
	live := NewLive(inner)

	err := live.Show(context.Background(), Frame{Sequence: 1, PNG: []byte("one")})
	require.Error(t, err)

	_, _, have := live.Current()
	require.False(t, have, "a frame the channel refused is not on the display")
}

func TestLiveNextWakesEveryWaiter(t *testing.T) {
	live := NewLive(&recorder{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Several viewers waiting on the same frame: a browser tab, an operator's second tab, a receiver
	// following the display. One frame goes to all of them.
	const viewers = 8
	var wg sync.WaitGroup
	got := make([]int64, viewers)
	for i := range viewers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frame, _, ok := live.Next(ctx, 0)
			if ok {
				got[i] = frame.Sequence
			}
		}()
	}

	// Give the waiters a moment to block, so this exercises the wake-up rather than the fast path.
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, live.Show(ctx, Frame{Sequence: 7, PNG: []byte("seven")}))

	wg.Wait()
	for i := range viewers {
		require.Equal(t, int64(7), got[i], "viewer %d missed the frame", i)
	}
}

// TestLiveNextReturnsImmediatelyWhenAlreadyAhead covers the viewer that has fallen behind: it must be
// given the screen as it is now, not made to wait for the next change.
func TestLiveNextReturnsImmediatelyWhenAlreadyAhead(t *testing.T) {
	live := NewLive(&recorder{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, live.Show(ctx, Frame{Sequence: 42, PNG: []byte("forty-two")}))

	frame, _, ok := live.Next(ctx, 0)
	require.True(t, ok)
	require.Equal(t, int64(42), frame.Sequence)

	frame, _, ok = live.Next(ctx, 41)
	require.True(t, ok)
	require.Equal(t, int64(42), frame.Sequence)
}

// TestLiveNextTimesOutWithoutANewFrame is what tells a long-polling request to answer "nothing new"
// rather than hold the connection forever.
func TestLiveNextTimesOutWithoutANewFrame(t *testing.T) {
	live := NewLive(&recorder{})
	require.NoError(t, live.Show(context.Background(), Frame{Sequence: 3, PNG: []byte("three")}))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, ok := live.Next(ctx, 3)
	require.False(t, ok, "the display has not moved past sequence 3")
}

// TestLiveDropsTheDecodedImage keeps a fully expanded bitmap from being held alive for as long as a
// frame stays on screen. Everything reading a published frame wants the encoded bytes.
func TestLiveDropsTheDecodedImage(t *testing.T) {
	live := NewLive(&recorder{})
	require.NoError(t, live.Show(context.Background(), Frame{
		Sequence: 1,
		PNG:      []byte("one"),
		Image:    image.NewGray(image.Rect(0, 0, 4, 4)),
	}))

	frame, _, _ := live.Current()
	require.Nil(t, frame.Image)
}
