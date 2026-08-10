package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/store"
)

// TestGetTransferReportsLegacyEncryptionForPreMigrationRows covers a row written before
// migration 004 added encryption_id: the column defaults to 0 ("none") for such a row
// regardless of what its frames actually carry, because the single global-key scheme this
// feature replaced never recorded a cipher id at all — it only ever meant one cipher,
// AES-256-GCM. A status response that read encryption_id alone would call that transfer
// unencrypted, which is not true and not what its frames are.
func TestGetTransferReportsLegacyEncryptionForPreMigrationRows(t *testing.T) {
	h := newArchiveHarness(t)
	file := h.putFile(t, "report.pdf", []byte("the original file"))

	tx, err := h.store.Transmissions.Create(context.Background(), store.Transmission{
		FileID: file.ID,
		// Exactly the shape a pre-migration-004 row reads today: Encrypted set by the old
		// scheme, EncryptionID at the column's NOT NULL DEFAULT 0 because that column did
		// not exist yet when the row was written.
		Encrypted:    true,
		EncryptionID: 0,
	})
	require.NoError(t, err)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/transfers/"+tx.ID.String(), nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var status TransferStatus
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
	require.Equal(t, "aes256gcm", status.Encryption)
}

// TestGetTransferReportsNoneForAnOrdinaryUnencryptedRow is the control case: a row that was
// never encrypted must still report "none", proving the legacy fallback above is selective
// rather than always reporting aes256gcm regardless of the Encrypted flag.
func TestGetTransferReportsNoneForAnOrdinaryUnencryptedRow(t *testing.T) {
	h := newArchiveHarness(t)
	file := h.putFile(t, "report.pdf", []byte("the original file"))

	tx, err := h.store.Transmissions.Create(context.Background(), store.Transmission{
		FileID: file.ID,
	})
	require.NoError(t, err)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/transfers/"+tx.ID.String(), nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var status TransferStatus
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
	require.Equal(t, "none", status.Encryption)
}
