// Package jobs is the sender's work engine: a Postgres-backed pool of workers running
// typed handlers, with dependencies, retries, progress, and control.
//
// Every stage of the sender's pipeline is a job, and the reason is that every stage
// represents real elapsed time. Compressing and chunking a fifty-gigabyte file, then
// rendering a hundred thousand frames, is minutes to hours of work; a process that held
// that in memory would lose all of it to a deploy or a crash, on precisely the files the
// platform exists to move. Rows survive restarts, so a restarted sender picks up where
// it stopped.
//
// Postgres is the queue rather than a queue being bolted alongside it, and that is a
// deliberate trade. A dedicated broker would dispatch faster, but the work here is
// measured in seconds to minutes, so dispatch latency is irrelevant — while having the
// queue in the same transaction as the data means a job and the rows it produced commit
// together or not at all. A separate broker would make partial failures possible in a
// way nothing here could reconcile.
package jobs

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status is where a job is in its life.
type Status string

// Job statuses.
const (
	// StatusPending means the job is waiting to be claimed. It may still be waiting on
	// its dependencies or on its retry delay.
	StatusPending Status = "pending"

	// StatusRunning means a worker holds the job.
	StatusRunning Status = "running"

	// StatusCompleted means the handler returned without error.
	StatusCompleted Status = "completed"

	// StatusFailed means the handler exhausted its attempts, or failed in a way that
	// retrying cannot fix.
	StatusFailed Status = "failed"

	// StatusCancelled means an operator stopped it.
	StatusCancelled Status = "cancelled"

	// StatusPaused means an operator stopped it with the intention of resuming. A paused
	// job has not spent the attempt it was interrupted during.
	StatusPaused Status = "paused"
)

// Terminal reports whether a status is final.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// Valid reports whether a status is one this build defines.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusCompleted, StatusFailed, StatusCancelled, StatusPaused:
		return true
	}
	return false
}

// Priority orders the queue.
type Priority string

// Job priorities. They are the same three the transmission scheduler uses, because a
// retransmission's jobs should inherit the urgency of the chunk that needs it.
const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

// Valid reports whether a priority is one this build defines.
func (p Priority) Valid() bool {
	return p == PriorityHigh || p == PriorityNormal || p == PriorityLow
}

// rank orders priorities for the claim query. Lower is claimed first.
func (p Priority) rank() int {
	switch p {
	case PriorityHigh:
		return 0
	case PriorityLow:
		return 2
	default:
		return 1
	}
}

// Control is a request made of a running job.
type Control string

// Control requests.
const (
	// ControlNone means nothing has been asked.
	ControlNone Control = ""

	// ControlPause asks a job to stop in a way it can be resumed from.
	ControlPause Control = "pause"

	// ControlCancel asks a job to stop for good.
	ControlCancel Control = "cancel"
)

// Job is one unit of work.
type Job struct {
	ID   uuid.UUID `json:"id"`
	Type string    `json:"type"`

	Status   Status   `json:"status"`
	Priority Priority `json:"priority"`

	// TransmissionID and FileID are the subject of the work, when it has one.
	TransmissionID *uuid.UUID `json:"transmission_id,omitempty"`
	FileID         *uuid.UUID `json:"file_id,omitempty"`

	// Payload is the handler's own parameters, opaque to the engine.
	Payload json.RawMessage `json:"payload,omitempty"`

	// DependsOn lists jobs that must complete before this one may be claimed.
	DependsOn []uuid.UUID `json:"depends_on,omitempty"`

	Attempts    int `json:"attempts"`
	MaxAttempts int `json:"max_attempts"`

	// RunAfter is the earliest this job may be claimed. It expresses both retry backoff
	// and deliberate scheduling, so there is one mechanism rather than two.
	RunAfter time.Time `json:"run_after"`

	// ClaimedBy names the worker holding the job, and ClaimedAt when it took it. Together
	// they let a job be recovered when its worker dies: without them, a crash would leave
	// work claimed for ever.
	ClaimedBy *string    `json:"claimed_by,omitempty"`
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`

	// Progress is a percentage and Message the handler's description of what it is doing.
	// Both exist for the operator's benefit rather than the engine's.
	Progress int    `json:"progress"`
	Message  string `json:"message"`
	Error    string `json:"error,omitempty"`

	// Control is what has been asked of a running job.
	Control Control `json:"control,omitempty"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Duration is how long the job ran, or has been running.
func (j Job) Duration() time.Duration {
	if j.StartedAt == nil {
		return 0
	}
	if j.FinishedAt != nil {
		return j.FinishedAt.Sub(*j.StartedAt)
	}
	return time.Since(*j.StartedAt)
}

// AttemptsLeft is how many more tries the job has.
func (j Job) AttemptsLeft() int {
	if left := j.MaxAttempts - j.Attempts; left > 0 {
		return left
	}
	return 0
}

// String renders a job for logs.
func (j Job) String() string {
	return fmt.Sprintf("job %s %s [%s] attempt %d/%d", j.ID, j.Type, j.Status, j.Attempts, j.MaxAttempts)
}

// Spec describes a job to be enqueued.
//
// It is a separate type from Job because most of a Job is the engine's business: an
// enqueuer has no opinion about attempts made, claim ownership, or progress, and letting
// it set those would let it lie about them.
type Spec struct {
	Type     string
	Priority Priority

	TransmissionID *uuid.UUID
	FileID         *uuid.UUID

	// Payload is marshalled to JSON. It may be nil.
	Payload any

	DependsOn []uuid.UUID

	// MaxAttempts overrides the configured default when positive.
	MaxAttempts int

	// RunAfter delays the job. The zero value means "as soon as possible".
	RunAfter time.Time
}

// Validate checks a spec can be enqueued.
func (s Spec) Validate() error {
	if s.Type == "" {
		return fmt.Errorf("jobs: a job needs a type")
	}
	if s.Priority != "" && !s.Priority.Valid() {
		return fmt.Errorf("jobs: %q is not a priority", s.Priority)
	}
	if s.MaxAttempts < 0 {
		return fmt.Errorf("jobs: max attempts cannot be negative")
	}
	return nil
}

// LogLevel is the severity of a job log line.
type LogLevel string

// Job log levels.
const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// LogLine is one entry in a job's log.
type LogLine struct {
	ID       int64           `json:"id"`
	JobID    uuid.UUID       `json:"job_id"`
	Level    LogLevel        `json:"level"`
	Message  string          `json:"message"`
	Fields   json.RawMessage `json:"fields,omitempty"`
	LoggedAt time.Time       `json:"logged_at"`
}

// Filter selects jobs for the listing endpoints.
type Filter struct {
	Status         []Status
	Types          []string
	TransmissionID *uuid.UUID
	Limit          int
	Offset         int
}

// Counts is how many jobs are in each status, for the dashboard.
type Counts map[Status]int
