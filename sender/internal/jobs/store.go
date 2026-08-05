package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opticaltransport/otp/sender/internal/db"
)

// Store errors.
var (
	// ErrNotFound means no job has that identifier.
	ErrNotFound = errors.New("jobs: no such job")

	// ErrNotPending means a transition was asked for that the job's current status does
	// not allow — resuming something that is not paused, say.
	ErrNotPending = errors.New("jobs: the job is not in a state that allows this")
)

// Store is the job engine's persistence.
type Store struct {
	pool *db.Pool
}

// NewStore returns a store over a connection pool.
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// jobColumns is the column list every read shares, so a new field cannot be added to
// one query and forgotten in another.
const jobColumns = `
	id, type, status, priority, transmission_id, file_id, payload, depends_on,
	attempts, max_attempts, run_after, claimed_by, claimed_at,
	progress, message, error, control, started_at, finished_at, created_at, updated_at`

// scanJob reads one row in jobColumns order.
func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var payload []byte
	err := row.Scan(
		&j.ID, &j.Type, &j.Status, &j.Priority, &j.TransmissionID, &j.FileID, &payload,
		&j.DependsOn, &j.Attempts, &j.MaxAttempts, &j.RunAfter, &j.ClaimedBy, &j.ClaimedAt,
		&j.Progress, &j.Message, &j.Error, &j.Control, &j.StartedAt, &j.FinishedAt,
		&j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return Job{}, err
	}
	if len(payload) > 0 {
		j.Payload = json.RawMessage(payload)
	}
	return j, nil
}

// Enqueue creates a job.
func (s *Store) Enqueue(ctx context.Context, spec Spec, defaultAttempts int) (Job, error) {
	specs, err := s.EnqueueAll(ctx, []Spec{spec}, defaultAttempts)
	if err != nil {
		return Job{}, err
	}
	return specs[0], nil
}

// EnqueueAll creates several jobs in one transaction.
//
// The transaction is what makes a pipeline safe to enqueue. The sender's stages form a
// chain where each depends on the last, and a partial enqueue would leave jobs waiting on
// dependencies that were never created — pending for ever, with nothing to explain why.
// Either the whole chain exists or none of it does.
func (s *Store) EnqueueAll(ctx context.Context, specs []Spec, defaultAttempts int) ([]Job, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	for i, spec := range specs {
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("jobs: spec %d: %w", i, err)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	out := make([]Job, 0, len(specs))
	for _, spec := range specs {
		payload := json.RawMessage("{}")
		if spec.Payload != nil {
			encoded, err := json.Marshal(spec.Payload)
			if err != nil {
				return nil, fmt.Errorf("jobs: %s payload: %w", spec.Type, err)
			}
			payload = encoded
		}

		priority := spec.Priority
		if priority == "" {
			priority = PriorityNormal
		}
		attempts := spec.MaxAttempts
		if attempts == 0 {
			attempts = defaultAttempts
		}
		runAfter := spec.RunAfter
		if runAfter.IsZero() {
			runAfter = time.Now()
		}
		dependsOn := spec.DependsOn
		if dependsOn == nil {
			dependsOn = []uuid.UUID{}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO jobs (id, type, status, priority, transmission_id, file_id,
			                  payload, depends_on, max_attempts, run_after)
			VALUES ($1, $2, 'pending', $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+jobColumns,
			uuid.New(), spec.Type, priority, spec.TransmissionID, spec.FileID,
			payload, dependsOn, attempts, runAfter)

		job, err := scanJob(row)
		if err != nil {
			return nil, fmt.Errorf("jobs: enqueue %s: %w", spec.Type, err)
		}
		out = append(out, job)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// EnqueueChain creates jobs where each depends on the one before it, and returns them in
// order. It is how the pipeline is expressed: compress, then chunk, then error-code, then
// render.
func (s *Store) EnqueueChain(ctx context.Context, specs []Spec, defaultAttempts int) ([]Job, error) {
	linked := make([]Spec, len(specs))
	copy(linked, specs)

	// The identifiers are not known until the rows exist, so the chain is built by
	// enqueuing one at a time within the same transaction. EnqueueAll would be wrong here
	// precisely because it cannot see the identifiers it is creating.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	out := make([]Job, 0, len(linked))
	var previous *uuid.UUID
	for i, spec := range linked {
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("jobs: spec %d: %w", i, err)
		}
		if previous != nil {
			spec.DependsOn = append(append([]uuid.UUID{}, spec.DependsOn...), *previous)
		}

		payload := json.RawMessage("{}")
		if spec.Payload != nil {
			encoded, err := json.Marshal(spec.Payload)
			if err != nil {
				return nil, fmt.Errorf("jobs: %s payload: %w", spec.Type, err)
			}
			payload = encoded
		}
		priority := spec.Priority
		if priority == "" {
			priority = PriorityNormal
		}
		attempts := spec.MaxAttempts
		if attempts == 0 {
			attempts = defaultAttempts
		}
		runAfter := spec.RunAfter
		if runAfter.IsZero() {
			runAfter = time.Now()
		}
		dependsOn := spec.DependsOn
		if dependsOn == nil {
			dependsOn = []uuid.UUID{}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO jobs (id, type, status, priority, transmission_id, file_id,
			                  payload, depends_on, max_attempts, run_after)
			VALUES ($1, $2, 'pending', $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+jobColumns,
			uuid.New(), spec.Type, priority, spec.TransmissionID, spec.FileID,
			payload, dependsOn, attempts, runAfter)

		job, err := scanJob(row)
		if err != nil {
			return nil, fmt.Errorf("jobs: enqueue %s: %w", spec.Type, err)
		}
		out = append(out, job)
		id := job.ID
		previous = &id
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// Claim takes the next runnable job for a worker, or returns ErrNotFound if there is
// nothing to do.
//
// The query is the heart of the engine, and every clause in it is load-bearing:
//
// FOR UPDATE SKIP LOCKED is what makes the pool safe across replicas without a
// coordinator. Each worker locks the row it is claiming and skips rows another worker has
// locked, so N workers claim N different jobs in one round trip each, with no contention
// and no possibility of two workers running the same job.
//
// The dependency check runs in the same statement rather than being read first and acted
// on afterwards. Checking separately would be a race: a dependency could complete, or a
// job could be claimed by someone else, between the read and the update.
//
// A dependency that no longer exists counts as satisfied. Jobs cascade when their
// transmission is deleted, and a chain whose earlier links have been swept away should
// run or be cleaned up, not wait for a row that will never return.
func (s *Store) Claim(ctx context.Context, worker string) (Job, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE jobs SET
			status      = 'running',
			claimed_by  = $1,
			claimed_at  = now(),
			started_at  = coalesce(started_at, now()),
			attempts    = attempts + 1,
			control     = '',
			updated_at  = now()
		WHERE id = (
			SELECT j.id FROM jobs j
			WHERE j.status = 'pending'
			  AND j.run_after <= now()
			  AND NOT EXISTS (
				  SELECT 1
				  FROM unnest(j.depends_on) AS dep(id)
				  JOIN jobs d ON d.id = dep.id
				  WHERE d.status <> 'completed'
			  )
			ORDER BY
				CASE j.priority WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
				j.run_after,
				j.created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+jobColumns, worker)

	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

// Get returns one job.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Job, error) {
	job, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

// Complete marks a job finished successfully.
func (s *Store) Complete(ctx context.Context, id uuid.UUID, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET
			status = 'completed', progress = 100, message = $2, error = '',
			claimed_by = NULL, claimed_at = NULL, control = '',
			finished_at = now(), updated_at = now()
		WHERE id = $1`, id, message)
	return err
}

// Fail records a failed attempt.
//
// Whether the job is retried is decided here rather than by the caller, because the
// decision depends on state only the row holds — how many attempts have been made
// already. The backoff doubles per attempt up to a ceiling, which keeps a job that is
// failing for an external reason from hammering whatever it depends on.
func (s *Store) Fail(ctx context.Context, id uuid.UUID, cause error, base, max time.Duration) (retrying bool, err error) {
	job, err := s.Get(ctx, id)
	if err != nil {
		return false, err
	}

	message := cause.Error()
	if job.Attempts >= job.MaxAttempts {
		if _, err := s.pool.Exec(ctx, `
			UPDATE jobs SET
				status = 'failed', error = $2,
				claimed_by = NULL, claimed_at = NULL, control = '',
				finished_at = now(), updated_at = now()
			WHERE id = $1`, id, message); err != nil {
			return false, err
		}
		// A chain cannot continue past a broken link, and its later jobs would otherwise
		// stay pending for ever with nothing to explain it.
		if err := s.failDependents(ctx, id); err != nil {
			return false, err
		}
		return false, nil
	}

	delay := base << uint(job.Attempts-1)
	if delay > max || delay <= 0 {
		delay = max
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE jobs SET
			status = 'pending', error = $2, run_after = now() + $3::interval,
			claimed_by = NULL, claimed_at = NULL, control = '',
			updated_at = now()
		WHERE id = $1`, id, message, delay.String())
	return err == nil, err
}

// failDependents fails every job waiting on one that will not complete.
//
// It walks the graph rather than only the immediate dependents, because a pipeline is a
// chain: failing only the next link would leave the ones after it pending, which is the
// same problem one step further along.
func (s *Store) failDependents(ctx context.Context, id uuid.UUID) error {
	frontier := []uuid.UUID{id}
	seen := map[uuid.UUID]bool{id: true}

	for len(frontier) > 0 {
		current := frontier[0]
		frontier = frontier[1:]

		rows, err := s.pool.Query(ctx, `
			UPDATE jobs SET
				status = 'failed',
				error = 'a job this one depends on did not complete',
				finished_at = now(), updated_at = now()
			WHERE status IN ('pending', 'paused') AND $1 = ANY (depends_on)
			RETURNING id`, current)
		if err != nil {
			return err
		}
		var next []uuid.UUID
		for rows.Next() {
			var dependent uuid.UUID
			if err := rows.Scan(&dependent); err != nil {
				rows.Close()
				return err
			}
			if !seen[dependent] {
				seen[dependent] = true
				next = append(next, dependent)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		frontier = append(frontier, next...)
	}
	return nil
}

// Progress records how far along a running job is.
func (s *Store) Progress(ctx context.Context, id uuid.UUID, percent int, message string) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET progress = $2, message = $3, updated_at = now() WHERE id = $1`,
		id, percent, message)
	return err
}

// Log appends a line to a job's log.
func (s *Store) Log(ctx context.Context, id uuid.UUID, level LogLevel, message string, fields map[string]any) error {
	encoded := json.RawMessage("{}")
	if len(fields) > 0 {
		b, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		encoded = b
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO job_logs (job_id, level, message, fields) VALUES ($1, $2, $3, $4)`,
		id, level, message, encoded)
	return err
}

// Logs returns a job's log lines, oldest first.
func (s *Store) Logs(ctx context.Context, id uuid.UUID, limit, offset int) ([]LogLine, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, job_id, level, message, fields, logged_at
		FROM job_logs WHERE job_id = $1 ORDER BY id LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogLine
	for rows.Next() {
		var line LogLine
		var fields []byte
		if err := rows.Scan(&line.ID, &line.JobID, &line.Level, &line.Message, &fields, &line.LoggedAt); err != nil {
			return nil, err
		}
		line.Fields = json.RawMessage(fields)
		out = append(out, line)
	}
	return out, rows.Err()
}

// RequestControl asks a job to pause or cancel.
//
// A pending job transitions immediately, since nothing is running to interrupt. A running
// job only has the request recorded: the engine notices it, cancels the handler's context,
// and the status changes when the handler actually returns. Anything else would mean
// reporting a job as stopped while its handler was still writing to the database.
func (s *Store) RequestControl(ctx context.Context, id uuid.UUID, control Control) (Job, error) {
	job, err := s.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if job.Status.Terminal() {
		return Job{}, fmt.Errorf("%w: %s is already %s", ErrNotPending, id, job.Status)
	}

	switch {
	case job.Status == StatusRunning:
		_, err = s.pool.Exec(ctx,
			`UPDATE jobs SET control = $2, updated_at = now() WHERE id = $1`, id, control)

	case control == ControlPause:
		_, err = s.pool.Exec(ctx,
			`UPDATE jobs SET status = 'paused', control = '', updated_at = now() WHERE id = $1`, id)

	default: // cancel
		_, err = s.pool.Exec(ctx, `
			UPDATE jobs SET status = 'cancelled', control = '',
			                finished_at = now(), updated_at = now()
			WHERE id = $1`, id)
		if err == nil {
			err = s.failDependents(ctx, id)
		}
	}
	if err != nil {
		return Job{}, err
	}
	return s.Get(ctx, id)
}

// Resume returns a paused job to the queue.
func (s *Store) Resume(ctx context.Context, id uuid.UUID) (Job, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'pending', control = '', error = '',
		                run_after = now(), updated_at = now()
		WHERE id = $1 AND status = 'paused'`, id)
	if err != nil {
		return Job{}, err
	}
	if tag.RowsAffected() == 0 {
		job, getErr := s.Get(ctx, id)
		if getErr != nil {
			return Job{}, getErr
		}
		return Job{}, fmt.Errorf("%w: %s is %s, not paused", ErrNotPending, id, job.Status)
	}
	return s.Get(ctx, id)
}

// finishControlled applies whatever was asked of a job that has now stopped running.
//
// Pausing gives back the attempt the interrupted run consumed. A pause is not a failure,
// and an operator who pauses and resumes a job several times should not find it out of
// attempts because of it.
func (s *Store) finishControlled(ctx context.Context, id uuid.UUID, control Control) error {
	switch control {
	case ControlPause:
		_, err := s.pool.Exec(ctx, `
			UPDATE jobs SET
				status = 'paused', control = '',
				attempts = greatest(attempts - 1, 0),
				claimed_by = NULL, claimed_at = NULL, updated_at = now()
			WHERE id = $1`, id)
		return err

	case ControlCancel:
		if _, err := s.pool.Exec(ctx, `
			UPDATE jobs SET
				status = 'cancelled', control = '',
				claimed_by = NULL, claimed_at = NULL,
				finished_at = now(), updated_at = now()
			WHERE id = $1`, id); err != nil {
			return err
		}
		return s.failDependents(ctx, id)
	}
	return nil
}

// ReclaimStale returns jobs whose worker disappeared to the queue.
//
// A worker that is killed mid-job leaves its row claimed and running for ever, and no
// other worker will touch it. This is what makes the pool survive a crash, an eviction, or
// a deploy that does not wait for jobs to finish. The attempt already counted, so a job
// that repeatedly kills its worker still exhausts its attempts rather than looping.
func (s *Store) ReclaimStale(ctx context.Context, timeout time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET
			status = 'pending', claimed_by = NULL, claimed_at = NULL,
			error = 'the worker holding this job stopped responding',
			updated_at = now()
		WHERE status = 'running' AND claimed_at < now() - $1::interval`, timeout.String())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// List returns jobs matching a filter, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Job, error) {
	where := []string{"true"}
	args := []any{}

	if len(f.Status) > 0 {
		statuses := make([]string, len(f.Status))
		for i, s := range f.Status {
			statuses[i] = string(s)
		}
		args = append(args, statuses)
		where = append(where, fmt.Sprintf("status = ANY ($%d)", len(args)))
	}
	if len(f.Types) > 0 {
		args = append(args, f.Types)
		where = append(where, fmt.Sprintf("type = ANY ($%d)", len(args)))
	}
	if f.TransmissionID != nil {
		args = append(args, *f.TransmissionID)
		where = append(where, fmt.Sprintf("transmission_id = $%d", len(args)))
	}

	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	limitArg := len(args)
	args = append(args, max(f.Offset, 0))
	offsetArg := len(args)

	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		jobColumns, strings.Join(where, " AND "), limitArg, offsetArg)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// CountByStatus is how many jobs are in each status, for the dashboard.
func (s *Store) CountByStatus(ctx context.Context) (Counts, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := Counts{}
	for rows.Next() {
		var status Status
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// Prune deletes finished jobs older than the retention period, and returns how many went.
// Their logs go with them, by the cascade on job_logs.
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM jobs
		WHERE status IN ('completed', 'failed', 'cancelled')
		  AND finished_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
