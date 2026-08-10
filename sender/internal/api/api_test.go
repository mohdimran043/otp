package api

import (
	"bytes"
	"encoding/json"
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
		{name: "grid renders larger than any panel", fields: map[string]string{
			// 600 cells at the default 8 px/cell and 2-cell quiet zone renders to
			// (600+4)*8 = 4832 px, past maxImagePixels (4320) but well inside
			// NewLayoutQuiet's own 48..4096 cell bound — so this reaches the
			// panel-size check specifically, not the earlier grid-bounds check.
			"grid_width": "600", "grid_height": "600"}, wantErr: "larger than any panel"},
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

// TestParseTransferRequestRejectsAGridTheEncoderCannotCarryAtTheResolvedDepth reaches the
// encoder-capacity branch of the grid validation specifically, which the table test above
// cannot: every case there that fails validation is stopped earlier, either by the
// encoder/bit-depth check right after encoding.ByName or by NewLayoutQuiet's own grid-size
// bound. This case slips past both. The request asks for the "binary" encoder (which only
// ever carries bit depth 1) and an explicit bit_depth of "0" — read literally as 0, not
// omitted — so the earlier "does this encoder support the requested depth" check is skipped
// (it only runs when the request's bit depth is nonzero). The deployment's configured
// default bit depth is 4, which is not zero, so it is what the grid-validation code
// resolves "0" to before asking the encoder to estimate capacity at it — and 4 is invalid
// for "binary". That failure can only come from EstimateCapacity's resolveDepth call.
func TestParseTransferRequestRejectsAGridTheEncoderCannotCarryAtTheResolvedDepth(t *testing.T) {
	s := New(Options{
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
	})
	cfg := config.Default()
	cfg.Optical.BitDepth = 4 // valid for color16, not for binary

	req := formRequest(t, map[string]string{"encoder": "binary", "bit_depth": "0"})
	_, err := s.parseTransferRequest(req, cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot carry the binary encoding")
	require.Contains(t, err.Error(), "does not offer depth 4")
}

// TestListProfilesReportsWhetherEncryptionIsConfiguredButNeverTheKey covers the form's other
// half of the legacy-encryption guarantee: NewTransfer.tsx needs to know whether an omitted
// encryption field means "plaintext" or "encrypt under the global key" before it can default
// to the right one, and it can only learn that safely if the key itself never leaves the server.
func TestListProfilesReportsWhetherEncryptionIsConfiguredButNeverTheKey(t *testing.T) {
	key := strings.Repeat("cd", 32)

	cases := []struct {
		name       string
		keyHex     string
		configured bool
	}{
		{name: "no key configured", keyHex: "", configured: false},
		{name: "key configured", keyHex: key, configured: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Optical.EncryptionKeyHex = tc.keyHex
			handler := New(Options{
				Config: config.NewWatcher("", cfg),
				Log:    zap.NewNop(),
			}).Routes()

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/profiles", nil))
			require.Equal(t, http.StatusOK, response.Code)

			require.NotContains(t, response.Body.String(), key,
				"the profiles response must never carry key material")

			var body struct {
				Defaults struct {
					EncryptionConfigured bool `json:"encryption_configured"`
				} `json:"defaults"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.Equal(t, tc.configured, body.Defaults.EncryptionConfigured)
		})
	}
}
