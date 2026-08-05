package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// Handler runs one type of job.
//
// A handler is given a Context rather than a bare Job so that reporting progress and
// writing to the job's log are always available and always attributed to the right job —
// a handler that had to be passed a store and an id separately would eventually be passed
// the wrong id.
type Handler interface {
	// Type is the job type this handler serves. It must be unique across handlers.
	Type() string

	// Run does the work. It should return promptly when the context is cancelled, which
	// happens on shutdown and when an operator pauses or cancels the job.
	Run(ctx context.Context, jc *Context) error
}

// HandlerFunc adapts a function to the Handler interface.
type HandlerFunc struct {
	JobType string
	Fn      func(ctx context.Context, jc *Context) error
}

// Type returns the handler's job type.
func (h HandlerFunc) Type() string { return h.JobType }

// Run calls the function.
func (h HandlerFunc) Run(ctx context.Context, jc *Context) error { return h.Fn(ctx, jc) }

// ErrPermanent wraps an error that retrying cannot fix.
//
// The distinction matters because the two failures need opposite handling. A database that
// was briefly unreachable should be retried; a payload that does not parse will not parse
// on the fifth attempt either, and retrying it wastes four backoff intervals before
// reporting a problem an operator could have seen immediately.
type ErrPermanent struct{ Err error }

func (e *ErrPermanent) Error() string { return "permanent: " + e.Err.Error() }
func (e *ErrPermanent) Unwrap() error { return e.Err }

// Permanent marks an error as not worth retrying.
func Permanent(err error) error { return &ErrPermanent{Err: err} }

// Context is what a handler is given: the job, and the means to report on it.
type Context struct {
	Job   Job
	Log   *zap.Logger
	store *Store

	// enqueued collects jobs the handler created, so they can be reported in the log line
	// that closes the job.
	enqueued []uuid.UUID
}

// Payload unmarshals the job's payload into v.
func (c *Context) Payload(v any) error {
	if len(c.Job.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(c.Job.Payload, v); err != nil {
		// A payload that does not parse will never parse, so this is permanent by
		// construction rather than by the handler remembering to say so.
		return Permanent(fmt.Errorf("jobs: %s payload: %w", c.Job.Type, err))
	}
	return nil
}

// Progress reports how far along the work is, as a percentage and a description.
func (c *Context) Progress(ctx context.Context, percent int, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	// Progress is a convenience for whoever is watching, so a failure to record it must
	// never fail the job that was making progress.
	if err := c.store.Progress(ctx, c.Job.ID, percent, message); err != nil {
		c.Log.Warn("could not record job progress", zap.Error(err))
	}
}

// Infof writes an informational line to the job's log.
func (c *Context) Infof(ctx context.Context, format string, args ...any) {
	c.logf(ctx, LogInfo, format, args...)
}

// Warnf writes a warning to the job's log.
func (c *Context) Warnf(ctx context.Context, format string, args ...any) {
	c.logf(ctx, LogWarn, format, args...)
}

func (c *Context) logf(ctx context.Context, level LogLevel, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if err := c.store.Log(ctx, c.Job.ID, level, message, nil); err != nil {
		c.Log.Warn("could not write to the job log", zap.Error(err))
	}
}

// Enqueue creates a follow-on job, recording it as this job's work.
func (c *Context) Enqueue(ctx context.Context, spec Spec, defaultAttempts int) (Job, error) {
	if spec.TransmissionID == nil {
		spec.TransmissionID = c.Job.TransmissionID
	}
	if spec.FileID == nil {
		spec.FileID = c.Job.FileID
	}
	job, err := c.store.Enqueue(ctx, spec, defaultAttempts)
	if err != nil {
		return Job{}, err
	}
	c.enqueued = append(c.enqueued, job.ID)
	return job, nil
}

// Engine is the worker pool.
type Engine struct {
	store    *Store
	log      *zap.Logger
	cfg      *config.Watcher
	handlers map[string]Handler

	// name identifies this process in the claimed_by column, so an operator can see which
	// instance is running what.
	name string

	// concurrency is read from configuration on each poll so it can be reloaded, and the
	// running count is what enforces it.
	running atomic.Int64

	mu      sync.Mutex
	started bool
	stop    chan struct{}
	done    chan struct{}
}

// NewEngine returns an engine. Handlers are registered before it is started.
func NewEngine(store *Store, cfg *config.Watcher, log *zap.Logger) *Engine {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "sender"
	}
	return &Engine{
		store:    store,
		log:      log.Named("jobs"),
		cfg:      cfg,
		handlers: map[string]Handler{},
		name:     fmt.Sprintf("%s/%d", host, os.Getpid()),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Register adds a handler. It panics on a duplicate type, because two handlers for one
// job type means work runs under whichever of them happened to register last — a
// programming error present in every run rather than a condition to handle.
func (e *Engine) Register(h Handler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		panic("jobs: handlers must be registered before the engine starts")
	}
	if prev, dup := e.handlers[h.Type()]; dup {
		panic(fmt.Sprintf("jobs: %q is already handled by %T", h.Type(), prev))
	}
	e.handlers[h.Type()] = h
}

// Types lists the registered job types, for the diagnostics endpoint.
func (e *Engine) Types() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.handlers))
	for t := range e.handlers {
		out = append(out, t)
	}
	return out
}

// Running is how many jobs this instance is executing.
func (e *Engine) Running() int { return int(e.running.Load()) }

// Start runs the pool until the context is cancelled or Stop is called.
//
// It is deliberately one dispatch loop rather than a fixed set of worker goroutines. A
// fixed pool has to decide its size at startup, and concurrency is reloadable — so the
// loop claims work while it has room and spawns a goroutine per job, which makes a
// concurrency change take effect on the next claim rather than on the next restart.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("jobs: the engine is already running")
	}
	e.started = true
	e.mu.Unlock()

	defer close(e.done)

	cfg := e.cfg.Current().Jobs
	e.log.Info("job engine starting",
		zap.String("worker", e.name),
		zap.Int("concurrency", cfg.Concurrency),
		zap.Strings("types", e.Types()))

	var wg sync.WaitGroup
	defer wg.Wait()

	reaper := time.NewTicker(cfg.ClaimTimeout / 2)
	defer reaper.Stop()

	idle := time.NewTimer(0)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			e.log.Info("job engine stopping, waiting for running jobs")
			return nil
		case <-e.stop:
			e.log.Info("job engine stopping, waiting for running jobs")
			return nil

		case <-reaper.C:
			// Jobs whose worker died are returned to the queue. Doing it here rather than in
			// a separate process means a single-instance deployment recovers its own work.
			jobs := e.cfg.Current().Jobs
			if n, err := e.store.ReclaimStale(ctx, jobs.ClaimTimeout); err != nil {
				e.log.Warn("could not reclaim stale jobs", zap.Error(err))
			} else if n > 0 {
				e.log.Warn("reclaimed jobs from workers that stopped responding", zap.Int("jobs", n))
			}

		case <-idle.C:
			jobs := e.cfg.Current().Jobs
			claimed := 0
			for int(e.running.Load()) < jobs.Concurrency {
				job, err := e.store.Claim(ctx, e.name)
				if errors.Is(err, ErrNotFound) {
					break
				}
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					e.log.Error("could not claim a job", zap.Error(err))
					break
				}

				claimed++
				e.running.Add(1)
				wg.Add(1)
				go func(job Job) {
					defer wg.Done()
					defer e.running.Add(-1)
					e.execute(ctx, job)
				}(job)
			}

			// Poll promptly while there is work, and back off to the configured interval
			// when there is none, so an idle deployment is not querying constantly.
			if claimed > 0 {
				idle.Reset(time.Millisecond)
			} else {
				idle.Reset(jobs.PollInterval)
			}
		}
	}
}

// Stop asks the engine to finish. It returns once the dispatch loop has exited; running
// jobs are given the shutdown grace their own contexts allow.
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.started {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	<-e.done
}

// execute runs one claimed job.
func (e *Engine) execute(ctx context.Context, job Job) {
	log := e.log.With(
		zap.String("job", job.ID.String()),
		zap.String("type", job.Type),
		zap.Int("attempt", job.Attempts))

	handler, known := e.handler(job.Type)
	if !known {
		// An unknown type is not a transient failure: no amount of retrying will make this
		// binary grow a handler. It usually means a mixed-version deployment, which an
		// operator needs to see rather than have hidden behind five attempts.
		log.Error("no handler is registered for this job type")
		if _, err := e.store.Fail(ctx, job.ID,
			Permanent(fmt.Errorf("jobs: no handler for %q", job.Type)),
			time.Second, time.Second); err != nil {
			log.Error("could not record the failure", zap.Error(err))
		}
		return
	}

	cfg := e.cfg.Current().Jobs

	// The handler's context is cancelled by shutdown, by the claim timeout, and by an
	// operator's pause or cancel. Watching for control happens alongside the work rather
	// than being checked between stages, because a stage can take minutes and an operator
	// who pressed pause expects something to happen sooner than that.
	runCtx, cancel := context.WithTimeout(ctx, cfg.ClaimTimeout)
	defer cancel()

	control := e.watchControl(runCtx, cancel, job.ID)

	jc := &Context{Job: job, Log: log, store: e.store}
	started := time.Now()

	err := e.runHandler(runCtx, handler, jc)
	requested := control()

	switch {
	case requested != ControlNone:
		// An operator asked for this, so it is not a failure however the handler returned.
		log.Info("job stopped on request", zap.String("control", string(requested)))
		if err := e.store.finishControlled(context.WithoutCancel(ctx), job.ID, requested); err != nil {
			log.Error("could not record the stop", zap.Error(err))
		}

	case err == nil:
		log.Info("job completed", zap.Duration("took", time.Since(started)))
		if err := e.store.Complete(context.WithoutCancel(ctx), job.ID, jc.Job.Message); err != nil {
			log.Error("could not record completion", zap.Error(err))
		}

	default:
		var permanent *ErrPermanent
		isPermanent := errors.As(err, &permanent)

		// A cancelled context that nobody asked for means shutdown: the job did not fail,
		// it was interrupted, and it should be picked up again rather than counted against
		// its attempts.
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			log.Info("job interrupted by shutdown, returning it to the queue")
			if _, err := e.store.Fail(context.WithoutCancel(ctx), job.ID,
				errors.New("interrupted by shutdown"), time.Second, time.Second); err != nil {
				log.Error("could not requeue the job", zap.Error(err))
			}
			return
		}

		base, max := cfg.BackoffBase, cfg.BackoffMax
		if isPermanent {
			// Skip straight to the final attempt by leaving no room for backoff to matter:
			// Fail decides on attempts, so a permanent error is reported by exhausting them.
			if err := e.exhaust(context.WithoutCancel(ctx), job.ID); err != nil {
				log.Error("could not record the permanent failure", zap.Error(err))
			}
		}
		retrying, ferr := e.store.Fail(context.WithoutCancel(ctx), job.ID, err, base, max)
		if ferr != nil {
			log.Error("could not record the failure", zap.Error(ferr))
			return
		}
		if retrying {
			log.Warn("job failed, will retry", zap.Error(err))
		} else {
			log.Error("job failed", zap.Error(err))
		}
	}
}

// exhaust spends a job's remaining attempts, so that a permanent failure is final without
// the failure path needing a second way to express finality.
func (e *Engine) exhaust(ctx context.Context, id uuid.UUID) error {
	_, err := e.store.pool.Exec(ctx,
		`UPDATE jobs SET attempts = max_attempts, updated_at = now() WHERE id = $1`, id)
	return err
}

// runHandler calls a handler, turning a panic into a failed job.
//
// A handler that panics must not take the process with it. The pipeline stages work on
// attacker-influenced input — an uploaded file, a filename, a payload — and while each is
// validated, a panic in one job should cost that job and not every other transmission the
// instance is running.
func (e *Engine) runHandler(ctx context.Context, h Handler, jc *Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			jc.Log.Error("job handler panicked", zap.Any("panic", r), zap.Stack("stack"))
			err = Permanent(fmt.Errorf("jobs: handler panicked: %v", r))
		}
	}()
	return h.Run(ctx, jc)
}

// watchControl polls for a pause or cancel request and cancels the handler when one
// arrives. It returns a function reporting what was asked, if anything.
//
// Polling is the right mechanism here despite being unfashionable. The alternative is
// LISTEN/NOTIFY, which needs its own dedicated connection per instance and delivers
// nothing during a transaction — and the requirement is only that an operator's pause takes
// effect in about a second, which a poll meets at negligible cost.
func (e *Engine) watchControl(ctx context.Context, cancel context.CancelFunc, id uuid.UUID) func() Control {
	var requested atomic.Value
	requested.Store(ControlNone)

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var control Control
				err := e.store.pool.QueryRow(ctx, `SELECT control FROM jobs WHERE id = $1`, id).Scan(&control)
				if err != nil || control == ControlNone {
					continue
				}
				requested.Store(control)
				cancel()
				return
			}
		}
	}()

	return func() Control {
		c, _ := requested.Load().(Control)
		return c
	}
}

func (e *Engine) handler(jobType string) (Handler, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.handlers[jobType]
	return h, ok
}
