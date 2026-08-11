package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

func newKeysTestServer(t *testing.T) *Server {
	t.Helper()
	pool := testdb.New(t)
	return New(Options{
		Store:  store.New(pool),
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
	})
}

// TestKeysCRUDNeverEchoesKeyMaterial mirrors the receiver's decoder keyring test: a saved key
// goes in and a fingerprint comes out, and nothing ever hands the key itself back — a page
// that can display a key is a page that can leak one, and the operator who saved it already
// has it.
func TestKeysCRUDNeverEchoesKeyMaterial(t *testing.T) {
	s := newKeysTestServer(t)
	mux := s.Routes()

	keyHex := strings.Repeat("ab", 32)

	// POST a valid key: 201, a fingerprint, and no trace of the key hex anywhere in the body.
	body, _ := json.Marshal(map[string]string{"key_hex": keyHex, "label": "quarterly reports"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), keyHex)

	var created keyView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, "quarterly reports", created.Label)
	require.Len(t, created.Fingerprint, 16, "fingerprint is 16 hex chars (first 8 bytes of SHA-256)")
	require.NotZero(t, created.ID)

	// POST with a too-short key: 400.
	badBody, _ := json.Marshal(map[string]string{"key_hex": "abcd1234", "label": "too short"})
	badReq := httptest.NewRequest(http.MethodPost, "/api/v1/keys", bytes.NewReader(badBody))
	badReq.Header.Set("Content-Type", "application/json")
	badRec := httptest.NewRecorder()
	mux.ServeHTTP(badRec, badReq)
	require.Equal(t, http.StatusBadRequest, badRec.Code)

	// POST with invalid hex: 400.
	notHexBody, _ := json.Marshal(map[string]string{"key_hex": "not-hex-at-all!!", "label": "bad"})
	notHexReq := httptest.NewRequest(http.MethodPost, "/api/v1/keys", bytes.NewReader(notHexBody))
	notHexReq.Header.Set("Content-Type", "application/json")
	notHexRec := httptest.NewRecorder()
	mux.ServeHTTP(notHexRec, notHexReq)
	require.Equal(t, http.StatusBadRequest, notHexRec.Code)

	// GET lists the one key that was added, fingerprint only.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	require.Equal(t, http.StatusOK, listRec.Code)
	require.NotContains(t, listRec.Body.String(), keyHex)

	var listed struct {
		Keys []keyView `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listed))
	require.Len(t, listed.Keys, 1)
	require.Equal(t, created.Fingerprint, listed.Keys[0].Fingerprint)

	// DELETE removes it: 204.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/keys/"+strconv.FormatInt(created.ID, 10), nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusNoContent, delRec.Code)

	// A second DELETE of the same id: 404, since it is no longer there.
	delAgainReq := httptest.NewRequest(http.MethodDelete, "/api/v1/keys/"+strconv.FormatInt(created.ID, 10), nil)
	delAgainRec := httptest.NewRecorder()
	mux.ServeHTTP(delAgainRec, delAgainReq)
	require.Equal(t, http.StatusNotFound, delAgainRec.Code)
}

// TestDeleteUnknownKeyIs404 covers deleting an id that was never created at all, as distinct
// from one deleted a moment ago.
func TestDeleteUnknownKeyIs404(t *testing.T) {
	s := newKeysTestServer(t)
	mux := s.Routes()

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/keys/999999", nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusNotFound, delRec.Code)
}

// TestDeleteKeyBadIDIs400 covers a path segment that is not a number at all.
func TestDeleteKeyBadIDIs400(t *testing.T) {
	s := newKeysTestServer(t)
	mux := s.Routes()

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/keys/not-a-number", nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusBadRequest, delRec.Code)
}
