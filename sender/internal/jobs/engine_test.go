package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/db"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// harness is a store, an engine, and a database wiped clean.
type harness struct {
	pool   *db.Pool
	store  *jobs.Store
	engine *jobs.Engine
	cfg    *config.Watcher
	log    *zap.Logger
}

func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()

	pool := testdb.New(t)

	cfg := config.Default()
	cfg.Database.URL = testdb.URLFor(t, pool)
	cfg.Ack.Secret = "test acknowledgement secret"
	cfg.Auth.JWTSecret = "test jwt secret that is long enough"
	// Fast enough that a test does not wait on production-scale intervals.
	cfg.Jobs.PollInterval = 20 * time.Millisecond
	cfg.Jobs.BackoffBase = 20 * time.Millisecond
	cfg.Jobs.BackoffMax = 100 * time.Millisecond
	cfg.Jobs.ClaimTimeout = 10 * time.Second
	if tune != nil {
		tune(&cfg)
	}
	require.NoError(t, cfg.Validate())

	log := zaptest.NewLogger(t)
	watcher := config.NewWatcher("", cfg)
	return &harness{
		pool:   pool,
		store:  jobs.NewStore(pool),
		engine: jobs.NewEngine(jobs.NewStore(pool), watcher, log),
		cfg:    watcher,
		log:    log,
	}
}

// run starts the engine and stops it when the test ends.
func (h *harness) run(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.NoError(t, h.engine.Start(ctx))
	}()

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return ctx
}

// waitFor polls until a condition holds, so tests do not depend on sleeps that are either
// flaky or slow.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) get(t *testing.T, id uuid.UUID) jobs.Job {
	t.Helper()
	job, err := h.store.Get(context.Background(), id)
	require.NoError(t, err)
	return job
}

func TestJobRunsAndCompletes(t *testing.T) {
	h := newHarness(t, nil)

	var ran atomic.Int64
	h.engine.Register(jobs.HandlerFunc{JobType: "noop", Fn: func(ctx context.Context, jc *jobs.Context) error {
		ran.Add(1)
		jc.Progress(ctx, 50, "halfway")
		jc.Infof(ctx, "did the work")
		return nil
	}})
	h.run(t)

	job, err := h.store.Enqueue(context.Background(), jobs.Spec{Type: "noop"}, 3)
	require.NoError(t, err)

	waitFor(t, "the job to complete", func() bool {
		return h.get(t, job.ID).Status == jobs.StatusCompleted
	})

	done := h.get(t, job.ID)
	require.Equal(t, int64(1), ran.Load())
	require.Equal(t, 100, done.Progress, "a completed job is at a hundred percent")
	require.Equal(t, 1, done.Attempts)
	require.NotNil(t, done.StartedAt)
	require.NotNil(t, done.FinishedAt)
	require.Nil(t, done.ClaimedBy, "a finished job holds no claim")

	logs, err := h.store.Logs(context.Background(), job.ID, 0, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.Equal(t, "did the work", logs[0].Message)
}

// TestPayloadReachesTheHandler covers the parameters a job carries, and that a payload
// which cannot be parsed is a permanent failure rather than five doomed attempts.
func TestPayloadReachesTheHandler(t *testing.T) {
	h := newHarness(t, nil)

	type params struct {
		Path  string `json:"path"`
		Count int    `json:"count"`
	}
	got := make(chan params, 1)
	h.engine.Register(jobs.HandlerFunc{JobType: "typed", Fn: func(ctx context.Context, jc *jobs.Context) error {
		var p params
		if err := jc.Payload(&p); err != nil {
			return err
		}
		got <- p
		return nil
	}})
	h.run(t)

	_, err := h.store.Enqueue(context.Background(), jobs.Spec{
		Type:    "typed",
		Payload: params{Path: "/uploads/report.tar", Count: 17},
	}, 3)
	require.NoError(t, err)

	select {
	case p := <-got:
		require.Equal(t, "/uploads/report.tar", p.Path)
		require.Equal(t, 17, p.Count)
	case <-time.After(15 * time.Second):
		t.Fatal("the handler never received its payload")
	}
}

// TestFailureRetriesThenGivesUp covers the retry policy: a transient failure is retried
// with backoff, and the job fails for good once its attempts are spent.
func TestFailureRetriesThenGivesUp(t *testing.T) {
	h := newHarness(t, nil)

	var attempts atomic.Int64
	h.engine.Register(jobs.HandlerFunc{JobType: "flaky", Fn: func(ctx context.Context, jc *jobs.Context) error {
		attempts.Add(1)
		return errors.New("the object store was unreachable")
	}})
	h.run(t)

	job, err := h.store.Enqueue(context.Background(), jobs.Spec{Type: "flaky", MaxAttempts: 3}, 3)
	require.NoError(t, err)

	waitFor(t, "the job to fail for good", func() bool {
		return h.get(t, job.ID).Status == jobs.StatusFailed
	})

	failed := h.get(t, job.ID)
	require.Equal(t, 3, failed.Attempts, "every attempt should have been spent")
	require.Equal(t, int64(3), attempts.Load())
	require.Contains(t, failed.Error, "unreachable")
	require.NotNil(t, failed.FinishedAt)
}

// TestPermanentFailureIsNotRetried is the other half of that policy. A payload that will
// never parse should not consume four backoff intervals before anyone hears about it.
func TestPermanentFailureIsNotRetried(t *testing.T) {
	h := newHarness(t, nil)

	var attempts atomic.Int64
	h.engine.Register(jobs.HandlerFunc{JobType: "doomed", Fn: func(ctx context.Context, jc *jobs.Context) error {
		attempts.Add(1)
		return jobs.Permanent(errors.New("the encoder profile names an encoder that does not exist"))
	}})
	h.run(t)

	job, err := h.store.Enqueue(context.Background(), jobs.Spec{Type: "doomed", MaxAttempts: 5}, 5)
	require.NoError(t, err)

	waitFor(t, "the job to fail", func() bool {
		return h.get(t, job.ID).Status == jobs.StatusFailed
	})
	require.Equal(t, int64(1), attempts.Load(), "a permanent failure must be tried once")
}

// TestPanicFailsOnlyItsOwnJob checks a handler that panics costs one job rather than the
// process. The pipeline works on uploaded files and operator-supplied names, so a panic in
// one stage must not take down every other transmission the instance is running.
func TestPanicFailsOnlyItsOwnJob(t *testing.T) {
	h := newHarness(t, nil)

	h.engine.Register(jobs.HandlerFunc{JobType: "panics", Fn: func(ctx context.Context, jc *jobs.Context) error {
		panic("a nil map somewhere")
	}})
	var survived atomic.Int64
	h.engine.Register(jobs.HandlerFunc{JobType: "fine", Fn: func(ctx context.Context, jc *jobs.Context) error {
		survived.Add(1)
		return nil
	}})
	h.run(t)

	bad, err := h.store.Enqueue(context.Background(), jobs.Spec{Type: "panics", MaxAttempts: 2}, 2)
	require.NoError(t, err)
	good, err := h.store.Enqueue(context.Background(), jobs.Spec{Type: "fine"}, 3)
	require.NoError(t, err)

	waitFor(t, "the panicking job to fail", func() bool {
		return h.get(t, bad.ID).Status == jobs.StatusFailed
	})
	waitFor(t, "the other job to complete", func() bool {
		return h.get(t, good.ID).Status == jobs.StatusCompleted
	})
	require.Equal(t, int64(1), survived.Load())
	require.Contains(t, h.get(t, bad.ID).Error, "panicked")
}

// TestUnknownTypeFailsImmediately covers a mixed-version deployment, where a job type was
// enqueued by a newer binary than the one claiming it. Retrying cannot make this binary
// grow a handler, so an operator should hear about it at once.
func TestUnknownTypeFailsImmediately(t *testing.T) {
	h := newHarness(t, nil)
	h.run(t)

	job, err := h.store.Enqueue(context.Background(), jobs.Spec{Type: "from-the-future", MaxAttempts: 5}, 5)
	require.NoError(t, err)

	waitFor(t, "the unknown job to fail", func() bool {
		return h.get(t, job.ID).Status == jobs.StatusFailed
	})
	require.Contains(t, h.get(t, job.ID).Error, "no handler")
}

// TestDependenciesRunInOrder is the DAG property the pipeline depends on: a job is not
// claimed until everything it waits on has completed.
func TestDependenciesRunInOrder(t *testing.T) {
	h := newHarness(t, nil)

	var order []string
	var mu sync.Mutex
	record := func(name string) jobs.HandlerFunc {
		return jobs.HandlerFunc{JobType: name, Fn: func(ctx context.Context, jc *jobs.Context) error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}}
	}
	for _, name := range []string{"compress", "chunk", "fec", "render"} {
		h.engine.Register(record(name))
	}
	h.run(t)

	chain, err := h.store.EnqueueChain(context.Background(), []jobs.Spec{
		{Type: "compress"}, {Type: "chunk"}, {Type: "fec"}, {Type: "render"},
	}, 3)
	require.NoError(t, err)
	require.Len(t, chain, 4)

	// Each link must actually record its dependency, or the ordering below would hold by
	// luck rather than by construction.
	require.Empty(t, chain[0].DependsOn)
	for i := 1; i < len(chain); i++ {
		require.Equal(t, []uuid.UUID{chain[i-1].ID}, chain[i].DependsOn)
	}

	waitFor(t, "the whole chain to complete", func() bool {
		return h.get(t, chain[3].ID).Status == jobs.StatusCompleted
	})

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"compress", "chunk", "fec", "render"}, order)
}

// TestDependentsFailWithTheirChain covers what happens to the rest of a pipeline when one
// stage cannot be made to work. Left alone, the later jobs would sit pending for ever with
// nothing to explain why, which is the worst possible outcome for whoever is debugging it.
func TestDependentsFailWithTheirChain(t *testing.T) {
	h := newHarness(t, nil)

	h.engine.Register(jobs.HandlerFunc{JobType: "breaks", Fn: func(ctx context.Context, jc *jobs.Context) error {
		return jobs.Permanent(errors.New("the file is not readable"))
	}})
	var laterRan atomic.Int64
	for _, name := range []string{"later", "later-still"} {
		h.engine.Register(jobs.HandlerFunc{JobType: name, Fn: func(ctx context.Context, jc *jobs.Context) error {
			laterRan.Add(1)
			return nil
		}})
	}
	h.run(t)

	chain, err := h.store.EnqueueChain(context.Background(), []jobs.Spec{
		{Type: "breaks", MaxAttempts: 1}, {Type: "later"}, {Type: "later-still"},
	}, 1)
	require.NoError(t, err)

	waitFor(t, "the whole chain to fail", func() bool {
		return h.get(t, chain[2].ID).Status == jobs.StatusFailed
	})

	require.Equal(t, jobs.StatusFailed, h.get(t, chain[0].ID).Status)
	require.Equal(t, jobs.StatusFailed, h.get(t, chain[1].ID).Status)
	require.Contains(t, h.get(t, chain[1].ID).Error, "depends on")
	require.Zero(t, laterRan.Load(), "nothing downstream of a broken stage may run")
}

// TestConcurrencyIsHonoured checks the configured limit is actually a limit. It is what
// stands between a deployment and a hundred simultaneous frame renders exhausting its
// memory.
func TestConcurrencyIsHonoured(t *testing.T) {
	const limit = 3
	h := newHarness(t, func(c *config.Config) { c.Jobs.Concurrency = limit })

	var inFlight atomic.Int64
	var peak atomic.Int64
	release := make(chan struct{})

	h.engine.Register(jobs.HandlerFunc{JobType: "slow", Fn: func(ctx context.Context, jc *jobs.Context) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			if p := peak.Load(); n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}})
	h.run(t)

	for i := 0; i < 12; i++ {
		_, err := h.store.Enqueue(context.Background(), jobs.Spec{Type: "slow"}, 3)
		require.NoError(t, err)
	}

	waitFor(t, "the pool to fill", func() bool { return inFlight.Load() == limit })
	time.Sleep(200 * time.Millisecond) // Long enough for the loop to claim more if it would.
	require.LessOrEqual(t, peak.Load(), int64(limit),
		"the engine ran %d jobs at once with a limit of %d", peak.Load(), limit)

	close(release)
	waitFor(t, "every job to finish", func() bool {
		counts, err := h.store.CountByStatus(context.Background())
		require.NoError(t, err)
		return counts[jobs.StatusCompleted] == 12
	})
}

// TestPriorityOrdersTheQueue checks high-priority work overtakes normal work. The
// scheduler escalates missing chunks to high, so this is what makes a retransmission
// arrive before the next fifty fresh frames.
func TestPriorityOrdersTheQueue(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Jobs.Concurrency = 1 })

	var order []string
	var mu sync.Mutex
	h.engine.Register(jobs.HandlerFunc{JobType: "ordered", Fn: func(ctx context.Context, jc *jobs.Context) error {
		var p struct{ Name string }
		require.NoError(t, jc.Payload(&p))
		mu.Lock()
		order = append(order, p.Name)
		mu.Unlock()
		return nil
	}})

	// Enqueue before starting the engine, so all of them are waiting when it begins and
	// the order is decided by priority rather than by arrival.
	ctx := context.Background()
	for _, spec := range []struct {
		name     string
		priority jobs.Priority
	}{
		{"low-1", jobs.PriorityLow},
		{"normal-1", jobs.PriorityNormal},
		{"high-1", jobs.PriorityHigh},
		{"normal-2", jobs.PriorityNormal},
		{"high-2", jobs.PriorityHigh},
	} {
		_, err := h.store.Enqueue(ctx, jobs.Spec{
			Type:     "ordered",
			Priority: spec.priority,
			Payload:  map[string]string{"Name": spec.name},
		}, 3)
		require.NoError(t, err)
		// Distinct creation times, so the tie-break within a priority is deterministic.
		time.Sleep(2 * time.Millisecond)
	}

	h.run(t)
	waitFor(t, "all five jobs to run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 5
	})

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"high-1", "high-2", "normal-1", "normal-2", "low-1"}, order)
}

// TestPauseAndResume covers an operator stopping a running job and starting it again. The
// attempt spent on the interrupted run is given back, because a pause is not a failure and
// an operator who pauses twice should not find the job out of attempts.
func TestPauseAndResume(t *testing.T) {
	h := newHarness(t, nil)

	var runs atomic.Int64
	started := make(chan struct{}, 4)
	finish := make(chan struct{})
	h.engine.Register(jobs.HandlerFunc{JobType: "pausable", Fn: func(ctx context.Context, jc *jobs.Context) error {
		runs.Add(1)
		started <- struct{}{}
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	h.run(t)

	ctx := context.Background()
	job, err := h.store.Enqueue(ctx, jobs.Spec{Type: "pausable", MaxAttempts: 2}, 2)
	require.NoError(t, err)

	<-started
	waitFor(t, "the job to be claimed", func() bool {
		return h.get(t, job.ID).Status == jobs.StatusRunning
	})

	_, err = h.store.RequestControl(ctx, job.ID, jobs.ControlPause)
	require.NoError(t, err)

	waitFor(t, "the job to pause", func() bool {
		return h.get(t, job.ID).Status == jobs.StatusPaused
	})
	paused := h.get(t, job.ID)
	require.Zero(t, paused.Attempts, "pausing gives back the attempt it interrupted")
	require.Nil(t, paused.ClaimedBy)

	close(finish)
	_, err = h.store.Resume(ctx, job.ID)
	require.NoError(t, err)

	waitFor(t, "the resumed job to complete", func() bool {
		return h.get(t, job.ID).Status == jobs.StatusCompleted
	})
	require.Equal(t, int64(2), runs.Load(), "the handler runs again after a resume")
}

// TestCancelStopsARunningJob covers cancellation, and that it takes the rest of the chain
// with it: there is no point rendering frames for a transmission an operator cancelled.
func TestCancelStopsARunningJob(t *testing.T) {
	h := newHarness(t, nil)

	started := make(chan struct{}, 1)
	h.engine.Register(jobs.HandlerFunc{JobType: "cancellable", Fn: func(ctx context.Context, jc *jobs.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}})
	h.engine.Register(jobs.HandlerFunc{JobType: "downstream", Fn: func(ctx context.Context, jc *jobs.Context) error {
		return nil
	}})
	h.run(t)

	ctx := context.Background()
	chain, err := h.store.EnqueueChain(ctx, []jobs.Spec{
		{Type: "cancellable"}, {Type: "downstream"},
	}, 3)
	require.NoError(t, err)

	<-started
	_, err = h.store.RequestControl(ctx, chain[0].ID, jobs.ControlCancel)
	require.NoError(t, err)

	waitFor(t, "the job to cancel", func() bool {
		return h.get(t, chain[0].ID).Status == jobs.StatusCancelled
	})
	waitFor(t, "the downstream job to fail", func() bool {
		return h.get(t, chain[1].ID).Status == jobs.StatusFailed
	})
}

// TestCancelAPendingJob covers the simpler case, where nothing is running to interrupt.
func TestCancelAPendingJob(t *testing.T) {
	h := newHarness(t, nil)

	ctx := context.Background()
	job, err := h.store.Enqueue(ctx, jobs.Spec{
		Type:     "not-started",
		RunAfter: time.Now().Add(time.Hour),
	}, 3)
	require.NoError(t, err)

	got, err := h.store.RequestControl(ctx, job.ID, jobs.ControlCancel)
	require.NoError(t, err)
	require.Equal(t, jobs.StatusCancelled, got.Status)

	// And a job that has already finished cannot be cancelled.
	_, err = h.store.RequestControl(ctx, job.ID, jobs.ControlCancel)
	require.ErrorIs(t, err, jobs.ErrNotPending)
}

// TestClaimIsExclusiveAcrossWorkers is the property FOR UPDATE SKIP LOCKED provides, and
// the reason the engine is safe to run on several replicas at once: a job runs exactly
// once even when every instance is trying to claim it.
func TestClaimIsExclusiveAcrossWorkers(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	const total = 40
	for i := 0; i < total; i++ {
		_, err := h.store.Enqueue(ctx, jobs.Spec{Type: "contended"}, 3)
		require.NoError(t, err)
	}

	// Twelve concurrent claimers, standing in for several replicas each with several
	// workers.
	var wg sync.WaitGroup
	claims := make(chan uuid.UUID, total*2)
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				job, err := h.store.Claim(ctx, fmt.Sprintf("worker-%d", w))
				if errors.Is(err, jobs.ErrNotFound) {
					return
				}
				require.NoError(t, err)
				claims <- job.ID
			}
		}(w)
	}
	wg.Wait()
	close(claims)

	seen := map[uuid.UUID]bool{}
	for id := range claims {
		require.False(t, seen[id], "job %s was claimed twice", id)
		seen[id] = true
	}
	require.Len(t, seen, total, "every job should have been claimed exactly once")
}

// TestStaleClaimsAreReclaimed covers a worker that died holding a job. Without this the row
// would stay running for ever and no other worker would touch it, so a single crash would
// strand a transmission permanently.
func TestStaleClaimsAreReclaimed(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	job, err := h.store.Enqueue(ctx, jobs.Spec{Type: "orphaned"}, 3)
	require.NoError(t, err)

	claimed, err := h.store.Claim(ctx, "a-worker-that-is-about-to-die")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.Equal(t, jobs.StatusRunning, claimed.Status)

	// Nothing can be reclaimed while the claim is fresh.
	n, err := h.store.ReclaimStale(ctx, time.Hour)
	require.NoError(t, err)
	require.Zero(t, n)

	// Age the claim rather than waiting for a timeout to pass.
	_, err = h.pool.Exec(ctx,
		`UPDATE jobs SET claimed_at = now() - interval '1 hour' WHERE id = $1`, job.ID)
	require.NoError(t, err)

	n, err = h.store.ReclaimStale(ctx, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	back := h.get(t, job.ID)
	require.Equal(t, jobs.StatusPending, back.Status)
	require.Nil(t, back.ClaimedBy)
	require.Equal(t, 1, back.Attempts, "the attempt still counted, so a job cannot loop for ever")
}

// TestPruneRemovesOldJobsAndTheirLogs covers retention. A deployment that ran for a year
// would otherwise carry every job it ever ran in the table the claim query scans.
func TestPruneRemovesOldJobsAndTheirLogs(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	job, err := h.store.Enqueue(ctx, jobs.Spec{Type: "ancient"}, 3)
	require.NoError(t, err)
	require.NoError(t, h.store.Log(ctx, job.ID, jobs.LogInfo, "something happened", nil))
	require.NoError(t, h.store.Complete(ctx, job.ID, "done"))

	_, err = h.pool.Exec(ctx,
		`UPDATE jobs SET finished_at = now() - interval '60 days' WHERE id = $1`, job.ID)
	require.NoError(t, err)

	recent, err := h.store.Enqueue(ctx, jobs.Spec{Type: "recent"}, 3)
	require.NoError(t, err)
	require.NoError(t, h.store.Complete(ctx, recent.ID, "done"))

	n, err := h.store.Prune(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	_, err = h.store.Get(ctx, job.ID)
	require.ErrorIs(t, err, jobs.ErrNotFound)
	_, err = h.store.Get(ctx, recent.ID)
	require.NoError(t, err, "a recently finished job is kept")

	// The logs went with it, by the cascade rather than by a second delete anybody has to
	// remember to write.
	var logCount int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM job_logs WHERE job_id = $1`, job.ID).Scan(&logCount))
	require.Zero(t, logCount)
}

// TestListAndCount cover the endpoints the operator UI is built on.
func TestListAndCount(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	transmission := uuid.New()
	_, err := h.pool.Exec(ctx, `
		INSERT INTO files (id, filename, stored_path, size_bytes, sha256)
		VALUES ($1, 'f.bin', 'files/f.bin', 1, decode(repeat('00', 32), 'hex'))`, transmission)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx, `
		INSERT INTO transmissions (id, file_id, status, encoder, bit_depth, compression,
			compression_level, fec_codec, fec_data_shards, fec_parity_shards,
			grid_width, grid_height, cell_pixels, quiet_zone)
		VALUES ($1, $1, 'pending', 'binary', 1, 'none', 0, 'none', 0, 0, 96, 96, 8, 2)`, transmission)
	require.NoError(t, err)

	mine, err := h.store.Enqueue(ctx, jobs.Spec{Type: "a", TransmissionID: &transmission}, 3)
	require.NoError(t, err)
	other, err := h.store.Enqueue(ctx, jobs.Spec{Type: "b"}, 3)
	require.NoError(t, err)
	require.NoError(t, h.store.Complete(ctx, other.ID, "done"))

	byTransmission, err := h.store.List(ctx, jobs.Filter{TransmissionID: &transmission})
	require.NoError(t, err)
	require.Len(t, byTransmission, 1)
	require.Equal(t, mine.ID, byTransmission[0].ID)

	byStatus, err := h.store.List(ctx, jobs.Filter{Status: []jobs.Status{jobs.StatusCompleted}})
	require.NoError(t, err)
	require.Len(t, byStatus, 1)
	require.Equal(t, other.ID, byStatus[0].ID)

	byType, err := h.store.List(ctx, jobs.Filter{Types: []string{"a", "b"}})
	require.NoError(t, err)
	require.Len(t, byType, 2)

	counts, err := h.store.CountByStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, counts[jobs.StatusPending])
	require.Equal(t, 1, counts[jobs.StatusCompleted])
}

func TestEnqueueRejectsBadSpecs(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	_, err := h.store.Enqueue(ctx, jobs.Spec{Type: ""}, 3)
	require.Error(t, err)

	_, err = h.store.Enqueue(ctx, jobs.Spec{Type: "x", Priority: "urgent"}, 3)
	require.Error(t, err)

	// A payload that cannot be marshalled is caught before anything is written, so a
	// chain is never half-created.
	_, err = h.store.EnqueueChain(ctx, []jobs.Spec{
		{Type: "ok"},
		{Type: "bad", Payload: map[string]any{"fn": func() {}}},
	}, 3)
	require.Error(t, err)

	all, err := h.store.List(ctx, jobs.Filter{})
	require.NoError(t, err)
	require.Empty(t, all, "a failed chain must leave nothing behind")
}

// TestGetMissingJob covers the not-found path the API layer maps to a 404.
func TestGetMissingJob(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.store.Get(context.Background(), uuid.New())
	require.ErrorIs(t, err, jobs.ErrNotFound)
}
