package camera

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stage is a machine whose cameras can be plugged and unplugged.
type stage struct {
	mu      sync.Mutex
	devices []Device
	current Selection
	applied []Selection
	reasons []string
}

func (s *stage) list() ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devices, nil
}

func (s *stage) currentSelection() Selection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *stage) apply(selection Selection, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = selection
	s.applied = append(s.applied, selection)
	s.reasons = append(s.reasons, reason)
	return nil
}

func (s *stage) plug(devices ...Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices = devices
}

func (s *stage) watcher() Watcher {
	return Watcher{List: s.list, Current: s.currentSelection, Apply: s.apply}
}

func laptop() Device {
	return Device{
		Path: "/dev/video0", Name: "Integrated Camera", Driver: "uvcvideo", Default: true,
		Modes: []Mode{
			{Format: "MJPG", Width: 1280, Height: 720, FPS: []float64{30}},
			{Format: "MJPG", Width: 1920, Height: 1080, FPS: []float64{30}},
		},
	}
}

func industrial() Device {
	return Device{
		Path: "/dev/video2", Name: "Industrial Camera", Driver: "uvcvideo",
		Modes: []Mode{{Format: "MJPG", Width: 3840, Height: 2160, FPS: []float64{25}}},
	}
}

// TestWatcherConfiguresTheCameraItFinds is the behaviour an operator expects of a laptop: plug nothing in,
// change nothing, and the built-in camera is used at its best.
func TestWatcherConfiguresTheCameraItFinds(t *testing.T) {
	s := &stage{devices: []Device{laptop()}}
	s.watcher().check(context.Background())

	require.Len(t, s.applied, 1)
	require.Equal(t, Selection{
		Device: "/dev/video0", Format: "MJPG", Width: 1920, Height: 1080, FPS: 30,
	}, s.applied[0], "the largest frame the camera offers, and the fastest at that size")
	require.Contains(t, s.reasons[0], "detected and configured automatically")
}

// TestWatcherNoticesACameraPluggedInLater is the reason this exists at all. A receiver that concluded "no
// camera" at startup and never looked again would go on believing it however many were plugged in after.
func TestWatcherNoticesACameraPluggedInLater(t *testing.T) {
	s := &stage{}
	w := s.watcher()

	w.check(context.Background())
	require.Empty(t, s.applied, "nothing attached is not an event")

	s.plug(laptop())
	w.check(context.Background())
	require.Len(t, s.applied, 1)
	require.Equal(t, "/dev/video0", s.applied[0].Device)
}

// TestWatcherLeavesAWorkingChoiceAlone is the narrow part of the rule, and the important one: an operator who
// chose the second camera keeps it when a third appears, because their decision is better evidence than the
// order the kernel enumerated the devices in.
func TestWatcherLeavesAWorkingChoiceAlone(t *testing.T) {
	s := &stage{
		devices: []Device{laptop(), industrial()},
		current: Selection{Device: "/dev/video2", Width: 3840, Height: 2160, FPS: 25, Format: "MJPG"},
	}
	w := s.watcher()

	w.check(context.Background())
	require.Empty(t, s.applied, "a camera that is present and chosen must not be reconfigured")

	// Another camera appears. Still no change: the industrial camera is the one that was chosen, and it is
	// still there — even though the laptop camera enumerates first.
	s.plug(laptop(), industrial(), Device{Path: "/dev/video4", Modes: []Mode{
		{Format: "MJPG", Width: 4096, Height: 2160, FPS: []float64{60}},
	}})
	w.check(context.Background())
	require.Empty(t, s.applied, "a bigger camera appearing does not override a deliberate choice")
}

// TestWatcherSubstitutesWhenTheChosenCameraIsUnplugged covers the swap, which must be loud. Capturing from a
// camera nobody chose is exactly the surprise this must not spring quietly.
func TestWatcherSubstitutesWhenTheChosenCameraIsUnplugged(t *testing.T) {
	s := &stage{
		devices: []Device{laptop(), industrial()},
		current: Selection{Device: "/dev/video2"},
	}
	w := s.watcher()

	s.plug(laptop()) // the industrial camera is unplugged
	w.check(context.Background())

	require.Len(t, s.applied, 1)
	require.Equal(t, "/dev/video0", s.applied[0].Device)
	require.Contains(t, s.reasons[0], "/dev/video2 is no longer attached",
		"the substitution must name what went missing")
}

// TestWatcherDoesNothingWithNoCameras keeps a receiver reading frames from a directory quiet. That state is
// permanent and correct for a file-backed channel, and it must not produce a message every few seconds.
func TestWatcherDoesNothingWithNoCameras(t *testing.T) {
	s := &stage{}
	w := s.watcher()
	for range 5 {
		w.check(context.Background())
	}
	require.Empty(t, s.applied)
	require.Empty(t, s.reasons)
}

func TestWatcherRunStopsWithItsContext(t *testing.T) {
	s := &stage{devices: []Device{laptop()}}
	w := s.watcher()
	w.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// The immediate check on start is what configures a camera already attached, without waiting an interval.
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.applied) >= 1
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not stop with its context")
	}
}
