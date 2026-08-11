package pipeline

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/reap"
)

// TypeRetention is the job type for the sweep that deletes transfers which never completed.
const TypeRetention = "retention"

// retention lists every transmission older than the configured max age that never reached
// "completed", reaps each one, and re-enqueues itself for the next pass.
//
// It is a self-scheduling job rather than a cron entry because the engine has no cron: a job
// type that wants to run forever on an interval expresses that by enqueuing its own successor,
// with RunAfter set to when it should next run. That is the same mechanism retry backoff uses,
// applied to a job that is meant to succeed every time rather than one recovering from failure.
//
// The successor is scheduled by a defer at the very top, before the listing or any reaping
// happens, rather than as the last line of the function. A sweep can fail wholesale — the
// listing query itself can error, not just one row's deletion — and a tail enqueue would never
// run in that case: the handler would return early, the engine's own retry/backoff would take
// over, and once MaxAttempts was spent the job would be marked terminally failed with no
// successor queued anywhere. Retention would then be silently over until something restarts
// the process. Scheduling the next run before doing any of the work that can fail means this
// pass's outcome only ever changes how much got reaped, never whether there is a next pass.
//
// One bad row must not stop the sweep either: reap.Transfer is called for every candidate
// regardless of whether an earlier one failed, and a failure is logged rather than returned, so
// a single transmission that will not delete — an object store hiccup, a row someone else is
// mid-way through deleting by hand — does not leave everything after it in the list untouched.
func (p *Pipeline) retention(ctx context.Context, jc *jobs.Context) (err error) {
	cfg := p.cfg.Current().Retention
	now := time.Now()
	attempts := p.cfg.Current().Jobs.MaxAttempts

	// context.WithoutCancel because this must still run, and commit, even when ctx was
	// cancelled by shutdown or by an operator's stop request arriving mid-sweep — the same
	// reasoning the engine itself uses when it records an interrupted job.
	defer func() {
		if _, enqErr := jc.Enqueue(context.WithoutCancel(ctx), jobs.Spec{
			Type:     TypeRetention,
			RunAfter: now.Add(cfg.Interval),
		}, attempts); enqErr != nil {
			p.log.Error("retention: could not schedule the next sweep", zap.Error(enqErr))
			if err == nil {
				err = enqErr
			}
		}
	}()

	ids, err := p.store.Transmissions.ListForRetention(ctx, now.Add(-cfg.MaxAge))
	if err != nil {
		return err
	}

	reaped := 0
	for _, id := range ids {
		if rerr := reap.Transfer(ctx, p.store, p.objects, p.log, id); rerr != nil {
			jc.Warnf(ctx, "could not reap transfer %s: %s", id, rerr)
			p.log.Warn("retention: could not reap a transfer", zap.String("transmission", id.String()), zap.Error(rerr))
			continue
		}
		reaped++
	}
	if reaped > 0 {
		jc.Infof(ctx, "reaped %d of %d transfers older than %s that never completed", reaped, len(ids), cfg.MaxAge)
	}
	return nil
}
