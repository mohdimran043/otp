package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// fake is a source that records whether it was closed, and can pretend to hold an exclusive device.
type fake struct {
	name      string
	exclusive bool

	mu     sync.Mutex
	closed bool
	reads  int
}

func (f *fake) Name() string { return f.name }

func (f *fake) Next(context.Context) (Capture, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return Capture{}, errors.New("closed")
	}
	f.reads++
	return Capture{Sequence: int64(f.reads)}, nil
}

func (f *fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fake) Exclusive() bool { return f.exclusive }

func (f *fake) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// TestSwapReplacesTheSource is the behaviour the whole type exists for: selecting a camera means the camera
// starts, not that a preference is filed for the next restart.
func TestSwapReplacesTheSource(t *testing.T) {
	old := &fake{name: "file"}
	next := &fake{name: "camera"}

	s := NewSwappable(old, config.Capture{Source: "file"})
	s.open = func(config.Capture) (Source, error) { return next, nil }

	require.NoError(t, s.Swap(config.Capture{Source: "camera"}))
	require.Equal(t, "camera", s.Name())
	require.True(t, old.wasClosed(), "the source being replaced must be released")

	capture, err := s.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), capture.Sequence, "reads now go to the new source")
}

// TestSwapClosesAnExclusiveSourceFirst is the fix for a real failure. A camera cannot be opened twice, so opening
// the new source before closing the old one fails with "device or resource busy" every time — which it did, when
// re-selecting the camera that was already running.
func TestSwapClosesAnExclusiveSourceFirst(t *testing.T) {
	camera := &fake{name: "camera", exclusive: true}
	replacement := &fake{name: "camera", exclusive: true}

	var openedWhileOldStillOpen bool
	s := NewSwappable(camera, config.Capture{Source: "camera", Device: "/dev/video0"})
	s.open = func(config.Capture) (Source, error) {
		if !camera.wasClosed() {
			openedWhileOldStillOpen = true
		}
		return replacement, nil
	}

	require.NoError(t, s.Swap(config.Capture{Source: "camera", Device: "/dev/video0"}))
	require.False(t, openedWhileOldStillOpen,
		"an exclusive device must be released before it is opened again")
	require.True(t, camera.wasClosed())
}

// TestSwapKeepsANonExclusiveSourceUntilTheNewOneOpens is the other order, and the reason it exists: a failure
// must leave the receiver reading from whatever it had.
func TestSwapKeepsANonExclusiveSourceUntilTheNewOneOpens(t *testing.T) {
	old := &fake{name: "file"}
	s := NewSwappable(old, config.Capture{Source: "file"})
	s.open = func(config.Capture) (Source, error) { return nil, errors.New("no such directory") }

	err := s.Swap(config.Capture{Source: "file", Dir: "/nowhere"})
	require.ErrorContains(t, err, "could not open the file source")
	require.False(t, old.wasClosed(), "a failed swap must not leave the receiver with nothing")
	require.Equal(t, "file", s.Name())

	capture, err := s.Next(context.Background())
	require.NoError(t, err, "the original source is still readable")
	require.Equal(t, int64(1), capture.Sequence)
}

// TestSwapRestoresAnExclusiveSourceWhenTheNewOneFails covers the cost of closing first: if the replacement will
// not open, the receiver would be left with no source at all unless the old one is put back.
func TestSwapRestoresAnExclusiveSourceWhenTheNewOneFails(t *testing.T) {
	camera := &fake{name: "camera", exclusive: true}
	restored := &fake{name: "camera", exclusive: true}

	s := NewSwappable(camera, config.Capture{Source: "camera", Device: "/dev/video0"})
	var calls int
	s.open = func(cfg config.Capture) (Source, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("device or resource busy")
		}
		require.Equal(t, "/dev/video0", cfg.Device, "the previous configuration is what gets reopened")
		return restored, nil
	}

	err := s.Swap(config.Capture{Source: "camera", Device: "/dev/video9"})
	require.ErrorContains(t, err, "device or resource busy")
	require.Equal(t, 2, calls, "the previous source must be reopened")

	capture, err := s.Next(context.Background())
	require.NoError(t, err, "the receiver is capturing again rather than left with nothing")
	require.Equal(t, int64(1), capture.Sequence)
}

// TestSwapReportsBothFailuresWhenTheOldSourceCannotBeReopened keeps the two situations distinct. "The switch
// failed" and "and there is now no source" need different responses from an operator.
func TestSwapReportsBothFailuresWhenTheOldSourceCannotBeReopened(t *testing.T) {
	camera := &fake{name: "camera", exclusive: true}
	s := NewSwappable(camera, config.Capture{Source: "camera", Device: "/dev/video0"})
	s.open = func(config.Capture) (Source, error) { return nil, errors.New("unplugged") }

	err := s.Swap(config.Capture{Source: "camera", Device: "/dev/video9"})
	require.ErrorContains(t, err, "could not open the camera source")
	require.ErrorContains(t, err, "could not be reopened either")
}

func TestSwappableWithNoSourceIsQuiet(t *testing.T) {
	s := NewSwappable(nil, config.Capture{})
	require.Equal(t, "none", s.Name())
	_, err := s.Next(context.Background())
	require.ErrorIs(t, err, ErrNoFrame, "no source is the channel being quiet, not a fault")
	require.NoError(t, s.Close())
}
