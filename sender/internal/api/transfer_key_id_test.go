package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/pipeline"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// Picking a saved key by id is an alternative to pasting its hex into every request. These
// tests cover parseTransferRequest's resolution of encryption_key_id (which needs a real
// SenderKeys store, hence testdb rather than the pure-config table test in api_test.go) and,
// end to end, that a transfer created with one actually encrypts under the saved key.

func TestParseTransferRequestEncryptionKeyID(t *testing.T) {
	s := newKeysTestServer(t)
	cfg := config.Default()
	ctx := context.Background()

	keyHex := strings.Repeat("5a", 32)
	keyBytes, err := hex.DecodeString(keyHex)
	require.NoError(t, err)
	saved, err := s.store.SenderKeys.Add(ctx, keyBytes, "saved for parsing")
	require.NoError(t, err)

	cases := []struct {
		name    string
		fields  map[string]string
		wantErr string
		check   func(t *testing.T, req TransferRequest)
	}{
		{
			name: "resolves a saved key",
			fields: map[string]string{
				"encryption":        "aes256gcm",
				"encryption_key_id": strconv.FormatInt(saved.ID, 10),
			},
			check: func(t *testing.T, req TransferRequest) {
				require.Equal(t, uint8(protocol.EncryptionAES256GCM), req.EncryptionID)
				require.Equal(t, keyBytes, req.EncryptionKey)
			},
		},
		{
			name: "id and hex together is refused",
			fields: map[string]string{
				"encryption":         "aes256gcm",
				"encryption_key_id":  strconv.FormatInt(saved.ID, 10),
				"encryption_key_hex": keyHex,
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "id with encryption none is refused",
			fields: map[string]string{
				"encryption":        "none",
				"encryption_key_id": strconv.FormatInt(saved.ID, 10),
			},
			wantErr: "encryption is \"none\"",
		},
		{
			name: "id without an encryption type is refused",
			fields: map[string]string{
				"encryption_key_id": strconv.FormatInt(saved.ID, 10),
			},
			wantErr: "without an encryption type",
		},
		{
			name: "unknown id is refused",
			fields: map[string]string{
				"encryption":        "aes256gcm",
				"encryption_key_id": "999999",
			},
			wantErr: "does not name a saved key",
		},
		{
			name: "non-numeric id is refused",
			fields: map[string]string{
				"encryption":        "aes256gcm",
				"encryption_key_id": "not-a-number",
			},
			wantErr: "not a number",
		},
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

// keyTransferHarness is a whole sender back end — database, object store, job engine, and
// pipeline — wired to the real HTTP handler, because the claim under test ("a transfer created
// with encryption_key_id actually encrypts under that saved key") can only be checked by
// letting a real transfer run to rendered frames.
type keyTransferHarness struct {
	store   *store.Store
	objects objectstore.Store
	handler http.Handler
}

func newKeyTransferHarness(t *testing.T) *keyTransferHarness {
	t.Helper()
	pool := testdb.New(t)

	cfg := config.Default()
	cfg.Database.URL = testdb.URLFor(t, pool)
	cfg.Ack.Secret = "test acknowledgement secret"
	cfg.Auth.JWTSecret = "a test jwt secret long enough to sign"
	cfg.Storage.Root = t.TempDir()
	cfg.Jobs.PollInterval = 20 * time.Millisecond
	cfg.Jobs.BackoffBase = 20 * time.Millisecond
	cfg.Jobs.BackoffMax = 100 * time.Millisecond
	cfg.Jobs.ClaimTimeout = 60 * time.Second
	cfg.Optical.GridWidth = 96
	cfg.Optical.GridHeight = 96
	cfg.Optical.CellPixels = 4
	require.NoError(t, cfg.Validate())

	objects, err := objectstore.Open(context.Background(), cfg.Storage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, objects.Close()) })

	log := zaptest.NewLogger(t)
	watcher := config.NewWatcher("", cfg)
	st := store.New(pool)
	js := jobs.NewStore(pool)
	engine := jobs.NewEngine(js, watcher, log)
	line := pipeline.New(st, js, objects, watcher, log)
	line.Register(engine)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.NoError(t, engine.Start(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	handler := New(Options{
		Store:    st,
		Jobs:     js,
		Objects:  objects,
		Pipeline: line,
		Config:   watcher,
		Log:      zap.NewNop(),
	}).Routes()

	return &keyTransferHarness{store: st, objects: objects, handler: handler}
}

// waitReady polls until the transmission the pipeline is preparing reaches "ready", or fails
// the test if it errors or times out — the same shape pipeline_test.go's prepareAndWait uses.
func (h *keyTransferHarness) waitReady(t *testing.T, id uuid.UUID) store.Transmission {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		tx, err := h.store.Transmissions.Get(ctx, id)
		require.NoError(t, err)
		if tx.Status == store.TxReady {
			return tx
		}
		if tx.Status == store.TxFailed {
			t.Fatalf("the transmission failed: %s", tx.Error)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the pipeline did not finish in time")
	return store.Transmission{}
}

// TestTransferWithSavedKeyRoundTrips creates a transfer that names a saved key by id rather
// than supplying its hex, and checks the claim end to end: the response never carries the key,
// and the rendered frames are FlagEncrypted and open under exactly that key.
func TestTransferWithSavedKeyRoundTrips(t *testing.T) {
	h := newKeyTransferHarness(t)
	ctx := context.Background()

	keyHex := strings.Repeat("5a", 32)
	keyBytes, err := hex.DecodeString(keyHex)
	require.NoError(t, err)
	saved, err := h.store.SenderKeys.Add(ctx, keyBytes, "round trip key")
	require.NoError(t, err)

	fields := map[string]string{
		"filename":          "secret.bin",
		"callback_url":      "https://example.com/callback",
		"encryption":        "aes256gcm",
		"encryption_key_id": strconv.FormatInt(saved.ID, 10),
		"autostart":         "false",
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for k, v := range fields {
		require.NoError(t, writer.WriteField(k, v))
	}
	part, err := writer.CreateFormFile("file", "secret.bin")
	require.NoError(t, err)
	payload := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 200)
	_, err = part.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), keyHex, "the response must never carry key material")

	var resp TransferResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	ready := h.waitReady(t, resp.TransmissionID)
	require.Equal(t, protocol.EncryptionAES256GCM, uint8(ready.EncryptionID))

	frames, err := h.store.Frames.List(ctx, ready.ID)
	require.NoError(t, err)
	require.NotEmpty(t, frames)

	sawEncryptedDataFrame := false
	for _, record := range frames {
		raw, err := objectstore.GetBytes(ctx, h.objects, record.StoredPath, 16<<20)
		require.NoError(t, err)
		img, err := png.Decode(bytes.NewReader(raw))
		require.NoError(t, err)

		frame, err := encoding.Decode(img, protocol.LocateOptions{})
		require.NoError(t, err)
		if frame.Header.Flags.Has(protocol.FlagManifest) || frame.Header.Flags.Has(protocol.FlagParity) {
			continue
		}

		require.True(t, frame.Header.Flags.Has(protocol.FlagEncrypted))
		require.Equal(t, uint8(protocol.EncryptionAES256GCM), frame.Header.EncryptionID)

		_, err = protocol.OpenFrame([][]byte{keyBytes}, frame)
		require.NoError(t, err, "the saved key must open every data frame this transfer rendered")
		sawEncryptedDataFrame = true
	}
	require.True(t, sawEncryptedDataFrame, "the transfer must have produced at least one encrypted data frame")
}
