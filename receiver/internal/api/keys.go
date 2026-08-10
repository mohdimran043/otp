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

	"github.com/opticaltransport/otp/receiver/internal/store"
)

// The keyring API. Keys go in and fingerprints come out; the key itself is never
// readable back, because a page that can display a key is a page that can leak one —
// and the operator who loaded it already has it.

// keyView is a decoder key as the API reports it: never the key itself.
type keyView struct {
	ID          int64     `json:"id"`
	Label       string    `json:"label"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
}

// fingerprint identifies a key without revealing it: the first 8 bytes of its SHA-256, hex-encoded.
// Long enough that two different keys loaded by mistake are visibly different, short enough that it
// is obviously not the key itself.
func fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

func toKeyView(k store.DecoderKey) keyView {
	return keyView{
		ID:          k.ID,
		Label:       k.Label,
		Fingerprint: fingerprint(k.Key),
		CreatedAt:   k.CreatedAt,
	}
}

// listKeys reports every key loaded into the ring, fingerprint only.
func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.DecoderKeys.List(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list the decryption keys", err)
		return
	}
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, toKeyView(k))
	}
	s.respond(w, http.StatusOK, map[string]any{"keys": views})
}

// addKeyRequest is what the settings page sends to load a new key.
type addKeyRequest struct {
	KeyHex string `json:"key_hex"`
	Label  string `json:"label"`
}

// addKey loads a new decryption key into the ring.
//
// The key arrives as hex because that is how an operator carries it — typed, pasted, or generated
// on the sender's form — and it is validated against protocol.KeySize before it ever reaches the
// database, so a malformed key is rejected here rather than skipped silently later by OpenFrame.
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

	created, err := s.store.DecoderKeys.Add(r.Context(), key, req.Label)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not save the key", err)
		return
	}
	s.respond(w, http.StatusCreated, toKeyView(created))
}

// deleteKey removes a key from the ring.
func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the key id is not a number", err)
		return
	}
	if err := s.store.DecoderKeys.Delete(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such key", nil)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not delete the key", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
