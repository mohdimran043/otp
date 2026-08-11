package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Stopping a transfer.
//
// Until this existed the only way to stop a transmission was to kill the process, which is a poor answer
// for a system whose whole point is transfers that take hours: an operator who has just noticed the wrong
// file, or who needs the display for something else, should not have to restart the sender and lose
// everything else in flight with it.
//
// A stop is a status change on the row rather than a signal to a goroutine. The display loop re-reads the
// status every frame, so the change reaches it within one frame interval — and neither the handler nor the
// loop has to know the other exists. That also makes a stop durable: it survives a restart of either side,
// which an in-process channel would not.
//
// Three actions, because they mean genuinely different things:
//
//   - **Cancel** ends the transfer. Nothing further is displayed and the far side is never told, because
//     there is nothing to tell it: the receiver simply stops seeing frames for a transmission it holds
//     partially, which is indistinguishable from the sender being switched off — and that is honest,
//     because as far as the receiver is concerned it is the same event.
//   - **Pause** stops the display and keeps everything. Acknowledged chunks stay acknowledged, so resuming
//     shows only what is still outstanding rather than starting again.
//   - **Resume** starts the display loop again from where the acknowledgements left it.

// controlResponse is what a stop, pause, or resume reports back.
type controlResponse struct {
	TransmissionID string `json:"transmission_id"`
	Status         string `json:"status"`

	// AckedChunks and ChunkCount say how far it got, which is the thing an operator wants to know
	// immediately after stopping something.
	AckedChunks int `json:"acked_chunks"`
	ChunkCount  int `json:"chunk_count"`

	// JobsCancelled is how many pipeline stages were still queued and have been told to stop. A transfer
	// cancelled during preparation has jobs to stop; one cancelled mid-display does not.
	JobsCancelled int `json:"jobs_cancelled,omitempty"`

	Note string `json:"note,omitempty"`
}

// stoppableStatuses are the states a transfer can be stopped from.
//
// A completed, failed, or already-cancelled transfer is not stoppable, and saying so is better than
// silently succeeding: an operator who cancels the wrong transfer needs to find out from the response
// rather than from the absence of an effect.
func stoppable(status store.TransmissionStatus) bool {
	switch status {
	case store.TxPending, store.TxPreparing, store.TxReady, store.TxTransmitting, store.TxPaused:
		return true
	default:
		return false
	}
}

// cancelTransfer stops a transfer for good.
func (s *Server) cancelTransfer(w http.ResponseWriter, r *http.Request) {
	s.changeState(w, r, store.TxCancelled)
}

// pauseTransfer stops the display but keeps the transfer resumable.
func (s *Server) pauseTransfer(w http.ResponseWriter, r *http.Request) {
	s.changeState(w, r, store.TxPaused)
}

// changeState is the shared path for cancel and pause.
func (s *Server) changeState(w http.ResponseWriter, r *http.Request, target store.TransmissionStatus) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}
	ctx := r.Context()

	transfer, err := s.store.Transmissions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such transfer", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read the transfer", err)
		return
	}

	if !stoppable(transfer.Status) {
		s.fail(w, http.StatusConflict, fmt.Sprintf(
			"this transfer is %s, so there is nothing to stop", transfer.Status), nil)
		return
	}
	if transfer.Status == target {
		s.fail(w, http.StatusConflict, fmt.Sprintf("this transfer is already %s", target), nil)
		return
	}
	if target == store.TxPaused && transfer.Status != store.TxTransmitting {
		s.fail(w, http.StatusConflict, fmt.Sprintf(
			"only a transfer that is transmitting can be paused; this one is %s. Cancel it instead.",
			transfer.Status), nil)
		return
	}

	// The status goes first. The display loop reads it every frame, so from here on nothing further is
	// shown — and if the job cancellation below fails, a stopped transfer is still stopped.
	if err := s.store.Transmissions.SetStatus(ctx, id, target, ""); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not stop the transfer", err)
		return
	}

	response := controlResponse{
		TransmissionID: id.String(),
		Status:         string(target),
		AckedChunks:    transfer.AckedChunks,
		ChunkCount:     transfer.ChunkCount,
	}

	if target == store.TxCancelled {
		// Preparation stages still queued are told to stop too. Without this, a transfer cancelled while
		// it was being compressed or rendered would go on consuming a worker to produce frames nobody
		// will ever display.
		response.JobsCancelled = s.cancelJobsFor(ctx, id)
		response.Note = "Nothing further will be displayed. The receiver is not told: it simply stops " +
			"seeing frames, which is the same event as the sender being switched off."
	} else {
		response.Note = "The display has stopped. Acknowledged chunks are kept, so resuming shows only " +
			"what is still outstanding."
	}

	s.log.Info("transfer stopped",
		zap.String("transmission", id.String()),
		zap.String("status", string(target)),
		zap.Int("jobs_cancelled", response.JobsCancelled))

	s.respond(w, http.StatusOK, response)
}

// cancelJobsFor asks every unfinished stage of a transmission to stop, and reports how many.
func (s *Server) cancelJobsFor(ctx context.Context, id uuid.UUID) int {
	list, err := s.jobs.List(ctx, jobs.Filter{TransmissionID: &id, Limit: 100})
	if err != nil {
		s.log.Warn("could not list jobs to cancel", zap.Error(err))
		return 0
	}

	cancelled := 0
	for _, job := range list {
		switch job.Status {
		case jobs.StatusPending, jobs.StatusRunning, jobs.StatusPaused:
			if _, err := s.jobs.RequestControl(ctx, job.ID, jobs.ControlCancel); err != nil {
				s.log.Warn("could not cancel a job",
					zap.String("job", job.ID.String()), zap.Error(err))
				continue
			}
			cancelled++
		}
	}
	return cancelled
}

// resumeTransfer starts a paused transfer displaying again.
func (s *Server) resumeTransfer(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}
	ctx := r.Context()

	transfer, err := s.store.Transmissions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such transfer", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read the transfer", err)
		return
	}
	if transfer.Status != store.TxPaused {
		s.fail(w, http.StatusConflict, fmt.Sprintf(
			"only a paused transfer can be resumed; this one is %s", transfer.Status), nil)
		return
	}

	// Back to ready rather than straight to transmitting: the display loop is what moves it on, and a row
	// claiming to be transmitting with nothing displaying it would be a lie an operator has no way to see
	// through.
	if err := s.store.Transmissions.SetStatus(ctx, id, store.TxReady, ""); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not resume the transfer", err)
		return
	}
	if s.transmit != nil {
		s.transmit(ctx, id)
	}

	s.log.Info("transfer resumed", zap.String("transmission", id.String()))
	s.respond(w, http.StatusOK, controlResponse{
		TransmissionID: id.String(),
		Status:         string(store.TxReady),
		AckedChunks:    transfer.AckedChunks,
		ChunkCount:     transfer.ChunkCount,
		Note: fmt.Sprintf("Displaying again. %d of %d chunks were already acknowledged and will not be "+
			"shown again.", transfer.AckedChunks, transfer.ChunkCount),
	})
}

// startTransfer begins displaying a manually-prepared ready transfer. Unlike resume, which starts
// a paused transfer again, this starts one that was prepared with autostart=false and has been
// waiting for an operator to begin its display. The transfer is already in the ready state; all
// that's left is to call the display loop.
func (s *Server) startTransfer(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}
	ctx := r.Context()

	transfer, err := s.store.Transmissions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such transfer", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read the transfer", err)
		return
	}
	if transfer.Status != store.TxReady {
		s.fail(w, http.StatusConflict, fmt.Sprintf(
			"only a ready transfer can be started; this one is %s", transfer.Status), nil)
		return
	}

	// The transfer is already ready, so no status change is needed. Call the display loop directly
	// to begin displaying it. The loop will move the status to transmitting as it starts rendering.
	if s.transmit != nil {
		s.transmit(ctx, id)
	}

	s.log.Info("transfer started", zap.String("transmission", id.String()))
	s.respond(w, http.StatusOK, controlResponse{
		TransmissionID: id.String(),
		Status:         string(store.TxReady),
		AckedChunks:    transfer.AckedChunks,
		ChunkCount:     transfer.ChunkCount,
		Note:           "Displaying now.",
	})
}
