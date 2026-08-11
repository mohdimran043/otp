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
// the next chunk creates a fresh row again, or it is finished, in which case there is nothing
// left running to disturb. So this does not need the sender's 409-while-busy guard — it decides
// whether the transmission is known, then removes its objects, then its rows.
//
// Existence is checked first, and separately, before anything is touched — deliberately not by
// looking for a manifest. Chunks routinely arrive before the manifest does and are stored and
// counted while the receiver waits for it, so a transmission with real decoded_chunks or
// missing_chunks or ack_state rows and real chunks/<id>/* objects can have no manifest row at
// all. Gating on the manifest would delete nothing, report 404, and still have destroyed those
// objects on the way there — telling the caller "no such transmission" about one whose bytes it
// had just thrown away. Store.Transmissions.Exists asks the same any-of-seven-tables question
// DeleteCascade itself checks, so a transmission that is only chunks so far is still found.
//
// Once existence is settled, objects go first and are removed best-effort, rows last and
// atomically: an object left behind with its rows gone is a leak nothing will ever find again,
// since nothing in the database still names it, while rows left behind with their objects gone
// is a state a retry repairs, because the rows are still there to say what needs cleaning up.
func (s *Server) handleDeleteTransmission(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}
	ctx := r.Context()

	exists, err := s.store.Transmissions.Exists(ctx, id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not check the transmission", err)
		return
	}
	if !exists {
		s.fail(w, http.StatusNotFound, "no such transmission", nil)
		return
	}

	s.deleteObjectsUnder(ctx, s.objects, "chunks/"+id.String()+"/", id)
	s.deleteObjectsUnder(ctx, s.objects, "merged/"+id.String()+"/", id)
	if s.acks != nil {
		s.deleteObjectsUnder(ctx, s.acks, protocol.AckDir(id)+"/", id)
	}

	if err := s.store.Transmissions.DeleteCascade(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Existed a moment ago and does not now: something else deleted it between the
			// check above and here. Reporting it as gone is correct either way.
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
