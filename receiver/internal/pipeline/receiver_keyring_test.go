package pipeline

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/store"
	"github.com/opticaltransport/otp/receiver/internal/testdb"
)

// TestKeyringIsConfiguredKeyThenStoredKeys is the whole reason the keyring exists: the sender
// chooses a key per transfer and the operator carries it here out of band, so the receiver must
// try the key it was configured with and everything an operator has loaded, in that order.
func TestKeyringIsConfiguredKeyThenStoredKeys(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)
	ctx := context.Background()

	configured := bytes.Repeat([]byte{0xAA}, 32)
	cfg := config.Default()
	cfg.Decoder.EncryptionKeyHex = hex.EncodeToString(configured)
	r := &Receiver{store: st, cfg: config.NewWatcher("", cfg), log: zap.NewNop()}

	// With nothing loaded yet, the ring holds only the configured key.
	ring := r.keyring(ctx)
	require.Equal(t, [][]byte{configured}, ring)

	// A key added to the ring shows up too, after the configured one.
	loaded := bytes.Repeat([]byte{0xBB}, 32)
	_, err := st.DecoderKeys.Add(ctx, loaded, "q3 reports")
	require.NoError(t, err)

	// Force a refresh: the cache would otherwise still be within its window.
	r.keysFetched = time.Time{}
	ring = r.keyring(ctx)
	require.Equal(t, [][]byte{configured, loaded}, ring)
}

// TestKeyringCachesForAFewSeconds is what keeps this affordable at 25 fps: a database read for
// every frame would be a self-inflicted load problem, so a key added on the settings page is not
// expected to appear before the next refresh.
func TestKeyringCachesForAFewSeconds(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)
	ctx := context.Background()

	r := &Receiver{store: st, cfg: config.NewWatcher("", config.Default()), log: zap.NewNop()}

	// Prime the cache with an empty ring, as of now.
	require.Empty(t, r.keyring(ctx))

	// A key lands in the store immediately afterwards.
	_, err := st.DecoderKeys.Add(ctx, bytes.Repeat([]byte{0xCC}, 32), "late arrival")
	require.NoError(t, err)

	// Still within the cache window: the new key is not yet visible.
	require.Empty(t, r.keyring(ctx))

	// Once the window has passed, the refreshed ring picks it up.
	r.keysFetched = time.Now().Add(-4 * time.Second)
	require.Len(t, r.keyring(ctx), 1)
}

