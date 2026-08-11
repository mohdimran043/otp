package config

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Reloadable is the subset of configuration that may change while the process runs.
//
// Everything here has one thing in common: nothing already in flight depends on it. A
// log level changes what the next line prints. A frame rate changes when the next frame
// is displayed. A window size changes how many chunks the scheduler will allow in
// flight from now on. None of them can invalidate a frame already rendered or a chunk
// already sized, which is what disqualifies everything else.
type Reloadable struct {
	LogLevel    string
	Concurrency int
	FPS         float64
	Brightness  float64
	Gamma       float64
	WindowSize  int
}

// reloadableOf extracts the subset from a whole configuration.
func reloadableOf(c Config) Reloadable {
	return Reloadable{
		LogLevel:    c.Log.Level,
		Concurrency: c.Jobs.Concurrency,
		FPS:         c.Display.FPS,
		Brightness:  c.Display.Brightness,
		Gamma:       c.Display.Gamma,
		WindowSize:  c.Display.WindowSize,
	}
}

// apply writes the subset back into a whole configuration.
func (r Reloadable) apply(c *Config) {
	c.Log.Level = r.LogLevel
	c.Jobs.Concurrency = r.Concurrency
	c.Display.FPS = r.FPS
	c.Display.Brightness = r.Brightness
	c.Display.Gamma = r.Gamma
	c.Display.WindowSize = r.WindowSize
}

// Watcher holds the live configuration and reloads the safe subset when the file
// changes.
//
// Readers get a whole Config rather than only the reloadable part, so nothing has to
// know which fields can change — a component reads what it needs when it needs it and
// is correct either way. What it must not do is cache a value across a reload and
// expect it to stay current, which is why Current returns a copy: a caller holding a
// snapshot is holding something it can reason about.
type Watcher struct {
	path string

	current atomic.Pointer[Config]

	mu          sync.Mutex
	subscribers []func(Config)

	// onIgnored reports fields that changed in the file but were not applied, so an
	// operator who edited a non-reloadable field learns that it needs a restart rather
	// than concluding the reload is broken.
	onIgnored func(fields []string)

	// onError reports a reload that failed, which is the common case in practice: a
	// file saved mid-edit is usually invalid for a moment.
	onError func(error)
}

// NewWatcher returns a watcher over an already-loaded configuration.
func NewWatcher(path string, initial Config) *Watcher {
	w := &Watcher{path: path}
	w.current.Store(&initial)
	return w
}

// Current returns the configuration as it stands.
func (w *Watcher) Current() Config { return *w.current.Load() }

// OnChange registers a function called after each successful reload. It is called with
// the new configuration, from the watcher's own goroutine, so a subscriber that blocks
// delays later reloads and nothing else.
func (w *Watcher) OnChange(fn func(Config)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.subscribers = append(w.subscribers, fn)
}

// OnIgnored registers a function called when a reload found changes it could not apply.
func (w *Watcher) OnIgnored(fn func(fields []string)) { w.onIgnored = fn }

// Apply installs a whole configuration and notifies subscribers.
//
// It is how a change made through the API takes effect, and it deliberately accepts more than Reload does.
// Reload applies only the subset that is safe to change while the file is being edited underneath a running
// process; Apply is called by a handler that has already established the stronger precondition — nothing is
// in flight — and can therefore change the frame geometry, which Reload must never do.
//
// The configuration is validated here as well as by the caller. A watcher that could be made to hold an
// invalid configuration would fail somewhere far away from the call that broke it, and the second check
// costs one function call.
//
// It does not write the file. The configuration file is the operator's document, with their comments and
// their formatting in it, and a service that rewrote it to record a change made through a form would be
// destroying something it does not own. So a change made here lasts until the process restarts or the file
// is reloaded, and the handler says so.
func (w *Watcher) Apply(next Config) (Config, error) {
	if err := next.Validate(); err != nil {
		return Config{}, err
	}

	old := w.Current()
	if reflect.DeepEqual(next, old) {
		return old, nil
	}
	w.current.Store(&next)

	w.mu.Lock()
	subscribers := make([]func(Config), len(w.subscribers))
	copy(subscribers, w.subscribers)
	w.mu.Unlock()
	for _, fn := range subscribers {
		fn(next)
	}
	return next, nil
}

// OnError registers a function called when a reload failed.
func (w *Watcher) OnError(fn func(error)) { w.onError = fn }

// Watch reloads the configuration when its file changes, until the context is done.
//
// It watches the *directory* rather than the file, because that is the only thing that
// works. Editors and deployment tooling replace a configuration file rather than
// writing into it — a temporary file plus a rename, or in Kubernetes a symlink swap —
// and a watch on the file's inode stops receiving events the moment that happens,
// leaving a process that looks like it is watching and never reloads again.
func (w *Watcher) Watch(ctx context.Context) error {
	if w.path == "" {
		<-ctx.Done()
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("config: could not watch %s: %w", w.path, err)
	}
	defer watcher.Close()

	dir := filepath.Dir(w.path)
	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("config: could not watch %s: %w", dir, err)
	}

	// Changes arrive in bursts — a write, a chmod, a rename — so they are coalesced.
	// Reloading on each event would parse the file several times per save, and one of
	// those parses would be of a half-written file.
	const settle = 150 * time.Millisecond
	var timer *time.Timer
	var pending <-chan time.Time

	target := filepath.Clean(w.path)
	for {
		select {
		case <-ctx.Done():
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(event.Name) != target {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(settle)
				pending = timer.C
			} else {
				timer.Reset(settle)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			w.report(err)

		case <-pending:
			timer, pending = nil, nil
			w.Reload()
		}
	}
}

// Reload re-reads the file and applies whatever of it may safely change.
//
// A failed reload leaves the running configuration exactly as it was. That matters more
// than it looks: the most common reason a reload fails is that an editor saved the file
// halfway through, and a process that adopted that intermediate state — or worse, fell
// back to defaults — would take itself down over a keystroke.
func (w *Watcher) Reload() {
	loaded, err := Load(w.path)
	if err != nil {
		w.report(err)
		return
	}

	old := w.Current()
	next := old
	reloadableOf(loaded).apply(&next)

	if ignored := differences(old, loaded); len(ignored) > 0 && w.onIgnored != nil {
		w.onIgnored(ignored)
	}
	if reflect.DeepEqual(next, old) {
		return
	}

	w.current.Store(&next)

	w.mu.Lock()
	subscribers := make([]func(Config), len(w.subscribers))
	copy(subscribers, w.subscribers)
	w.mu.Unlock()
	for _, fn := range subscribers {
		fn(next)
	}
}

func (w *Watcher) report(err error) {
	if w.onError != nil {
		w.onError(err)
	}
}

// differences lists the configuration sections that changed in the file but were not
// applied, so the operator can be told which ones need a restart.
//
// It reports sections rather than individual fields because that is what is actionable:
// the answer to any of them is the same restart, and a section name is enough to point
// at what was edited.
func differences(running, loaded Config) []string {
	// Compare with the reloadable subset already equalised, so only the fields that
	// cannot be applied remain different.
	comparable := loaded
	reloadableOf(running).apply(&comparable)

	// reflect.DeepEqual rather than == because Server carries a slice of origins, and a
	// configuration struct that cannot be compared with == today may gain another such
	// field tomorrow.
	var out []string
	if !reflect.DeepEqual(comparable.Server, running.Server) {
		out = append(out, "server")
	}
	if !reflect.DeepEqual(comparable.Database, running.Database) {
		out = append(out, "database")
	}
	if !reflect.DeepEqual(comparable.Storage, running.Storage) {
		out = append(out, "storage")
	}
	if !reflect.DeepEqual(comparable.Broker, running.Broker) {
		out = append(out, "broker")
	}
	if !reflect.DeepEqual(comparable.Jobs, running.Jobs) {
		out = append(out, "jobs (only concurrency can be reloaded)")
	}
	if !reflect.DeepEqual(comparable.Optical, running.Optical) {
		out = append(out, "optical (frame geometry is written into every frame header)")
	}
	if !reflect.DeepEqual(comparable.Display, running.Display) {
		out = append(out, "display (only fps, brightness, gamma, and window size can be reloaded)")
	}
	if !reflect.DeepEqual(comparable.Ack, running.Ack) {
		out = append(out, "ack")
	}
	if !reflect.DeepEqual(comparable.Auth, running.Auth) {
		out = append(out, "auth")
	}
	if !reflect.DeepEqual(comparable.Retention, running.Retention) {
		out = append(out, "retention")
	}
	if !reflect.DeepEqual(comparable.Log, running.Log) {
		out = append(out, "log (only level can be reloaded)")
	}
	if !reflect.DeepEqual(comparable.Metrics, running.Metrics) {
		out = append(out, "metrics")
	}
	if !reflect.DeepEqual(comparable.Tracing, running.Tracing) {
		out = append(out, "tracing")
	}
	return out
}
