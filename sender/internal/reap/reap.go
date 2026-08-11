// Package reap deletes a transfer for good: every object it wrote to the object store, and,
// last, the file row whose cascade removes every database row derived from it.
//
// It is its own package, beneath the API and the pipeline rather than beside them, because two
// callers need exactly this and nothing else — an operator's DELETE request and the retention
// job that runs on a schedule — and neither may depend on the other to get it. Keeping the
// logic here, importable by both without either importing the other, is what makes that
// possible.
package reap

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Transfer deletes a transmission's objects and, last, its file row.
//
// The object deletes are best-effort: a listing that fails, or a key that will not delete, is
// logged and skipped rather than aborting the whole call. That is deliberate, not sloppy — an
// operator who asked for a transfer to be gone should not stay blocked by one stray object
// nobody will ever address again, and Delete is documented as idempotent, so whatever a later
// pass or a retried retention run finds still there is safe to remove again.
//
// The row delete is the opposite of best-effort, and it goes last for the same reason: it is
// the step that makes the deletion durable. Objects gone with the row still present is a state
// a retry repairs — the row still names everything that needs cleaning up. Row gone with
// objects still present is not: nothing left in the database points at them, so they would
// leak until something else scans the whole bucket looking for orphans.
func Transfer(ctx context.Context, st *store.Store, objects objectstore.Store, log *zap.Logger, id uuid.UUID) error {
	tx, err := st.Transmissions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("reap: transfer %s: %w", id, store.ErrNotFound)
		}
		return fmt.Errorf("reap: could not read transfer %s: %w", id, err)
	}

	for _, key := range objectKeys(ctx, objects, log, id, tx.FileID) {
		if err := objects.Delete(ctx, key); err != nil {
			log.Warn("reap: could not delete an object",
				zap.String("transmission", id.String()), zap.String("key", key), zap.Error(err))
		}
	}

	// Last, and not best-effort: the cascade from this row is what actually removes the
	// transmission, its chunks, its frames, and everything else derived from it.
	if err := st.Files.Delete(ctx, tx.FileID); err != nil {
		return fmt.Errorf("reap: could not delete file %s for transfer %s: %w", tx.FileID, id, err)
	}
	return nil
}

// objectKeys collects every key a transfer's objects live under: the two listed prefixes
// (chunks and frames, whose count is not known without asking the store), plus the two
// single, well-known keys nothing needs to list to find.
func objectKeys(ctx context.Context, objects objectstore.Store, log *zap.Logger, id, fileID uuid.UUID) []string {
	var keys []string

	for _, sub := range []string{"chunks", "frames"} {
		prefix := fmt.Sprintf("transmissions/%s/%s/", id, sub)
		listed, err := objects.List(ctx, prefix)
		if err != nil {
			// A listing failure means these objects are not known to this pass, not that they
			// do not exist — logged so it is visible, and left for a retry to pick up rather
			// than failing the whole deletion over what is otherwise a cleanup detail.
			log.Warn("reap: could not list objects",
				zap.String("transmission", id.String()), zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		for _, o := range listed {
			keys = append(keys, o.Key)
		}
	}

	if key, err := objectstore.Key("transmissions", id.String(), "compressed.bin"); err != nil {
		log.Warn("reap: could not build the compressed-stream key",
			zap.String("transmission", id.String()), zap.Error(err))
	} else {
		keys = append(keys, key)
	}

	if key, err := objectstore.Key("files", fileID.String()); err != nil {
		log.Warn("reap: could not build the file key",
			zap.String("transmission", id.String()), zap.String("file", fileID.String()), zap.Error(err))
	} else {
		keys = append(keys, key)
	}

	return keys
}
