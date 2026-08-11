package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/receiver/internal/store"
)

// Deleting a transmission.
//
// The receiver has nothing like the sender's in-flight states to refuse against: a transmission
// here is either being written to by frames still arriving, in which case deleting it just means
// the next chunk creates a fresh manifestless row again, or it is finished, in which case there
// is nothing left running to disturb. So this does not need the sender's 409-while-busy guard —
// it removes the objects first, then the rows, and reports whether the transmission was known at
// all.
//
// Objects go first and are removed best-effort, rows last and atomically, for the same reason
// the sender's deletion is ordered that way: an object left behind with its rows gone is a leak
// nothing will ever find again, since nothing in the database still names it. Rows left behind
// with their objects gone is a state a retry repairs, because the manifest row is still there to
// say what needs cleaning up.
func (s *Server) handleDeleteTransmission(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}
	ctx := r.Context()

	s.deleteObjectsUnder(ctx, s.objects, "chunks/"+id.String()+"/", id)
	s.deleteObjectsUnder(ctx, s.objects, "merged/"+id.String()+"/", id)
	if s.acks != nil {
		s.deleteObjectsUnder(ctx, s.acks, protocol.AckDir(id)+"/", id)
	}

	if err := s.store.Transmissions.DeleteCascade(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such transmission", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not delete the transmission", err)
		return
	}

	s.log.Info("transmission deleted", zap.String("transmission", id.String()))
	w.WriteHeader(http.StatusNoContent)
}

// deleteObjectsUnder removes every object under a prefix, best-effort.
//
// There is no prefix-delete in the object store interface, so this is List then Delete each —
// and both halves are logged and skipped rather than aborting the request. A listing that fails
// or a key that will not delete should not leave an operator unable to remove a transmission
// over what is otherwise a cleanup detail; Delete is documented as idempotent, so whatever is
// left behind is safe for a retry or a later pass to remove again.
func (s *Server) deleteObjectsUnder(ctx context.Context, bucket objectstore.Store, prefix string, id uuid.UUID) {
	objects, err := bucket.List(ctx, prefix)
	if err != nil {
		s.log.Warn("could not list objects to delete",
			zap.String("transmission", id.String()), zap.String("prefix", prefix), zap.Error(err))
		return
	}
	for _, o := range objects {
		if err := bucket.Delete(ctx, o.Key); err != nil {
			s.log.Warn("could not delete an object",
				zap.String("transmission", id.String()), zap.String("key", o.Key), zap.Error(err))
		}
	}
}
