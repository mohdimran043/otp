package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/reap"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Deleting a transfer.
//
// This removes it entirely — the row and everything the pipeline wrote for it — rather than
// only marking it cancelled. Cancel exists for a transfer still in flight and leaves the
// history behind on purpose; delete is for an operator who wants the thing gone, history and
// all, typically because it holds a file that should not have been uploaded.
//
// It is refused while a transfer is actively occupying the display path — preparing,
// transmitting, or paused partway through — because the pipeline and the display loop hold
// this row's identifier and go on writing objects and reading status from it. Deleting out
// from under them would have a stage resurrect an object the delete just removed, or a display
// loop fail on a row that vanished mid-frame. A pending transfer has no such stage running yet,
// only queued jobs, so those are cancelled first and then the delete proceeds; ready,
// completed, failed, and cancelled transfers have nothing left running at all.
func (s *Server) handleDeleteTransfer(w http.ResponseWriter, r *http.Request) {
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

	switch transfer.Status {
	case store.TxPreparing, store.TxTransmitting, store.TxPaused:
		s.fail(w, http.StatusConflict, fmt.Sprintf(
			"this transfer is %s; cancel it first", transfer.Status), nil)
		return
	}

	if transfer.Status == store.TxPending {
		// Queued pipeline stages are told to stop before their objects are removed. Without
		// this, a worker that claims one of them after the delete would write a chunk or a
		// frame back into a namespace the delete just emptied, leaving an orphaned object
		// behind with nothing in the database left to name it.
		s.cancelJobsFor(ctx, id)
	}

	if err := reap.Transfer(ctx, s.store, s.objects, s.log, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such transfer", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not delete the transfer", err)
		return
	}

	s.log.Info("transfer deleted", zap.String("transmission", id.String()))
	w.WriteHeader(http.StatusNoContent)
}
