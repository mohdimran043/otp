package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// formRequest builds a multipart/form-data POST carrying the given fields plus a small
// file part, the shape createTransfer expects. Only parseTransferRequest is exercised
// here — it reads the form directly, not the uploaded file — but a real multipart form
// needs a file part to be well-formed.
func formRequest(t *testing.T, fields map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		require.NoError(t, writer.WriteField(key, value))
	}
	part, err := writer.CreateFormFile("file", "payload.bin")
	require.NoError(t, err)
	_, err = part.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	require.NoError(t, req.ParseMultipartForm(32<<20))
	return req
}

func TestParseTransferRequestEncryptionAndGrid(t *testing.T) {
	s := New(Options{
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
	})
	cfg := config.Default()

	cases := []struct {
		name    string
		fields  map[string]string
		wantErr string // substring, empty means accepted
		check   func(t *testing.T, req TransferRequest)
	}{
		{name: "default is none", fields: map[string]string{},
			check: func(t *testing.T, req TransferRequest) {
				require.Equal(t, uint8(0), req.EncryptionID)
				require.Nil(t, req.EncryptionKey)
			}},
		{name: "chacha with key", fields: map[string]string{
			"encryption": "chacha20poly1305", "encryption_key_hex": strings.Repeat("ab", 32)},
			check: func(t *testing.T, req TransferRequest) {
				require.Equal(t, uint8(2), req.EncryptionID)
				require.Len(t, req.EncryptionKey, 32)
			}},
		{name: "cipher without key", fields: map[string]string{"encryption": "aes256gcm"},
			wantErr: "requires a 64-hex-character key"},
		{name: "key without cipher", fields: map[string]string{
			"encryption": "none", "encryption_key_hex": strings.Repeat("ab", 32)},
			wantErr: "encryption is \"none\""},
		{name: "short key", fields: map[string]string{
			"encryption": "aes256gcm", "encryption_key_hex": "abcd"},
			wantErr: "64-hex-character"},
		{name: "unknown cipher", fields: map[string]string{"encryption": "rot13"},
			wantErr: "not one of"},
		{name: "grid preset", fields: map[string]string{"grid_width": "256", "grid_height": "256"},
			check: func(t *testing.T, req TransferRequest) {
				require.Equal(t, 256, req.GridWidth)
			}},
		{name: "grid too small to carry anything", fields: map[string]string{
			"grid_width": "16", "grid_height": "16"}, wantErr: "grid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := formRequest(t, tc.fields)
			result, err := s.parseTransferRequest(req, cfg)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.check != nil {
				tc.check(t, result)
			}
		})
	}
}

// TestParseTransferRequestKeepsLegacyBehaviourWithAGlobalKey covers the compatibility case:
// a deployment that configured a global encryption key before this feature existed must keep
// encrypting under it when a request says nothing about encryption at all.
func TestParseTransferRequestKeepsLegacyBehaviourWithAGlobalKey(t *testing.T) {
	s := New(Options{
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
	})
	cfg := config.Default()
	cfg.Optical.EncryptionKeyHex = strings.Repeat("cd", 32)

	req := formRequest(t, map[string]string{})
	result, err := s.parseTransferRequest(req, cfg)
	require.NoError(t, err)
	require.Equal(t, uint8(1), result.EncryptionID)
	require.Equal(t, cfg.EncryptionKey(), result.EncryptionKey)
}
