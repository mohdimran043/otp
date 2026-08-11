package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/store"
)

// The saved-key API. Keys go in and fingerprints come out; the key itself is never readable
// back, because a page that can display a key is a page that can leak one — and the operator
// who saved it already has it. A transfer picks one of these by id (encryption_key_id) as an
// alternative to pasting its hex into every request.

// keyView is a saved key as the API reports it: never the key itself.
type keyView struct {
	ID          int64     `json:"id"`
	Label       string    `json:"label"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
}

// fingerprint identifies a key without revealing it: the first 8 bytes of its SHA-256, hex-encoded.
// Long enough that two different keys saved by mistake are visibly different, short enough that it
// is obviously not the key itself.
func fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

func toKeyView(k store.SenderKey) keyView {
	return keyView{
		ID:          k.ID,
		Label:       k.Label,
		Fingerprint: fingerprint(k.Key),
		CreatedAt:   k.CreatedAt,
	}
}

// listKeys reports every key saved so far, fingerprint only.
func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.SenderKeys.List(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list the saved keys", err)
		return
	}
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, toKeyView(k))
	}
	s.respond(w, http.StatusOK, map[string]any{"keys": views})
}

// addKeyRequest is what the transfer form sends to save a new key.
type addKeyRequest struct {
	KeyHex string `json:"key_hex"`
	Label  string `json:"label"`
}

// addKey saves a new encryption key, so a later transfer can name it by id instead of
// carrying its hex again.
//
// The key arrives as hex because that is how an operator carries it — typed, pasted, or
// generated on this same form — and it is validated against protocol.KeySize before it ever
// reaches the database, so a malformed key is rejected here rather than accepted and failing a
// transfer created against it later.
func (s *Server) addKey(w http.ResponseWriter, r *http.Request) {
	var req addKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "the request body is not valid JSON", err)
		return
	}

	key, err := hex.DecodeString(req.KeyHex)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the key is not valid hex", err)
		return
	}
	if len(key) != protocol.KeySize {
		s.fail(w, http.StatusBadRequest,
			"the key must be "+strconv.Itoa(protocol.KeySize)+" bytes ("+strconv.Itoa(protocol.KeySize*2)+" hex characters)", nil)
		return
	}

	created, err := s.store.SenderKeys.Add(r.Context(), key, req.Label)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not save the key", err)
		return
	}
	s.respond(w, http.StatusCreated, toKeyView(created))
}

// deleteKey removes a saved key.
func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the key id is not a number", err)
		return
	}
	if err := s.store.SenderKeys.Delete(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such key", nil)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not delete the key", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
