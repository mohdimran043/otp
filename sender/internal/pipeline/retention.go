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
// One bad row must not stop the sweep: reap.Transfer is called for every candidate regardless
// of whether an earlier one failed, and a failure is logged rather than returned, so a single
// transmission that will not delete — an object store hiccup, a row someone else is mid-way
// through deleting by hand — does not leave everything after it in the list untouched.
func (p *Pipeline) retention(ctx context.Context, jc *jobs.Context) error {
	cfg := p.cfg.Current().Retention
	now := time.Now()

	ids, err := p.store.Transmissions.ListForRetention(ctx, now.Add(-cfg.MaxAge))
	if err != nil {
		return err
	}

	reaped := 0
	for _, id := range ids {
		if err := reap.Transfer(ctx, p.store, p.objects, p.log, id); err != nil {
			jc.Warnf(ctx, "could not reap transfer %s: %s", id, err)
			p.log.Warn("retention: could not reap a transfer", zap.String("transmission", id.String()), zap.Error(err))
			continue
		}
		reaped++
	}
	if reaped > 0 {
		jc.Infof(ctx, "reaped %d of %d transfers older than %s that never completed", reaped, len(ids), cfg.MaxAge)
	}

	// The next sweep is enqueued unconditionally, including when this one found nothing to
	// do or failed on every candidate: retention is a standing obligation, not a one-shot
	// task, and it must keep running for as long as the sender does.
	attempts := p.cfg.Current().Jobs.MaxAttempts
	if _, err := jc.Enqueue(ctx, jobs.Spec{
		Type:     TypeRetention,
		RunAfter: now.Add(cfg.Interval),
	}, attempts); err != nil {
		return err
	}
	return nil
}
