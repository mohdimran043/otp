package pipeline

import (
	"context"
	"fmt"
	"sync"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// Swappable is a capture source that can be replaced while the receiver is running.
//
// It exists because "select a camera" should mean the camera starts, not that a preference is recorded for the
// next restart. That was the first attempt, and the reasoning behind it — the capture loop holds its source open
// and swapping it underneath would tear down a session mid-frame — turned out to be an argument for doing the
// swap carefully rather than for not doing it. An operator clicking a camera and being told to restart the
// service is being asked to do the work the service should be doing.
//
// The swap is safe because of where the seam is. Readers only ever call Next, which takes the lock, reads the
// current source, and releases it before waiting on that source — so a swap can happen while a read is in
// flight, and the worst outcome is one frame read from the source being replaced. That frame is either a good
// frame, which is fine, or a failed read, which the loop already treats as the channel being briefly quiet.
//
// Which of the two orders a swap uses depends on whether what is open is exclusive — see Swap, where the whole
// of that argument lives.
type Swappable struct {
	mu      sync.RWMutex
	current Source

	// opened is the configuration the current source was built from, kept so that a failed swap can put back
	// what was working.
	opened config.Capture

	// open builds a source from a capture configuration. Injected so this type needs no knowledge of which
	// sources exist, which is what keeps OpenSource the single place that decides.
	open func(config.Capture) (Source, error)
}

// NewSwappable wraps a source so it can be replaced later.
func NewSwappable(initial Source, from config.Capture) *Swappable {
	return &Swappable{current: initial, opened: from, open: OpenSource}
}

// Exclusive reports whether a source holds a device that cannot be opened twice.
//
// A camera is exclusive; a directory is not. The distinction decides the order a swap has to happen in, and
// getting it wrong is not subtle: opening the new source first is the safe order in general — a failure leaves
// the old one working — but a camera cannot be opened while it is already open, so for an exclusive source that
// order fails every time with "device or resource busy". Which it did.
type exclusiveSource interface {
	Exclusive() bool
}

func isExclusive(source Source) bool {
	if source == nil {
		return false
	}
	if e, ok := source.(exclusiveSource); ok {
		return e.Exclusive()
	}
	return false
}

// Name is the current source's name.
func (s *Swappable) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return "none"
	}
	return s.current.Name()
}

// Next reads from whichever source is current.
//
// The lock is released before the read, deliberately: a camera read waits up to half a second for a frame, and
// holding a lock across it would make a swap wait that long too — and would make every reader wait on every
// other, which is exactly the serialisation that made the receiver slow once already.
func (s *Swappable) Next(ctx context.Context) (Capture, error) {
	s.mu.RLock()
	current := s.current
	s.mu.RUnlock()

	if current == nil {
		return Capture{}, ErrNoFrame
	}
	return current.Next(ctx)
}

// Close closes the current source.
func (s *Swappable) Close() error {
	s.mu.Lock()
	current := s.current
	s.current = nil
	s.mu.Unlock()

	if current == nil {
		return nil
	}
	return current.Close()
}

// Current returns the source in use, for callers that need to ask it something specific.
func (s *Swappable) Current() Source {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Swap opens a source from the given configuration and installs it.
//
// The order depends on whether what is currently open is exclusive, and this is the part that has to be right.
//
// For a source that can be opened twice — a directory — the new one is opened first, so a failure leaves the
// receiver reading from whatever it had. That is the safe order and the obvious one.
//
// A camera cannot be opened twice. Attempting it fails with "device or resource busy" every single time, which
// is what happened: selecting the camera that was already running reported a busy device and left the operator
// with a message about a switch that had not been needed. So an exclusive source is closed first, and if the new
// one then fails to open, the old configuration is reopened to put things back. That reopen can itself fail — a
// camera unplugged between the two calls — and the error then says both things, because "the switch failed" and
// "and there is now no source" are different situations and an operator needs to know which they are in.
func (s *Swappable) Swap(cfg config.Capture) error {
	s.mu.Lock()
	previous, previousCfg := s.current, s.opened
	exclusive := isExclusive(previous)
	if exclusive {
		// Released before anything else can take the device. A reader mid-call is finishing one read against a
		// closing source, which surfaces as a failed read — already treated as the channel being briefly quiet.
		s.current = nil
	}
	s.mu.Unlock()

	if exclusive && previous != nil {
		_ = previous.Close()
	}

	next, err := s.open(cfg)
	if err != nil {
		opened := fmt.Errorf("pipeline: could not open the %s source: %w", cfg.Source, err)
		if !exclusive {
			return opened
		}
		// Put back what was working.
		restored, restoreErr := s.open(previousCfg)
		if restoreErr != nil {
			return fmt.Errorf("%w; and the previous %s source could not be reopened either: %v",
				opened, previousCfg.Source, restoreErr)
		}
		s.mu.Lock()
		s.current, s.opened = restored, previousCfg
		s.mu.Unlock()
		return opened
	}

	s.mu.Lock()
	replaced := s.current
	s.current, s.opened = next, cfg
	s.mu.Unlock()

	if replaced != nil {
		_ = replaced.Close()
	}
	return nil
}
