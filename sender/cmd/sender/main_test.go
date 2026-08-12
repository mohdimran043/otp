package main

import (
	"context"
	"crypto/sha256"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// A transmitting row has to have something displaying it.
//
// The display loop is a goroutine this process owns, started from an HTTP request — an upload with autostart,
// a start, a resume — and nothing else. The status lives in the database, so it outlives the process that was
// driving it: a sender restarted mid-transfer comes back with a row that says "transmitting" and no loop
// behind it. Nothing displays, the transfer never completes, and the display page an operator is watching
// stays blank while every status the API reports insists the transfer is in flight.
//
// It is also unrecoverable through the API, which is what turns a restart into a dead end: start refuses
// anything that is not ready and resume refuses anything that is not paused, so the one status that needs
// rescuing is the one status neither of them will take. The way out was to pause the transfer and then resume
// it, which an operator has no reason to think of.
//
// The scheduler is already built for this — it keeps its retransmission timers in memory precisely because "a
// restart re-displays everything outstanding, which is correct: it cannot know what the receiver saw while it
// was gone". The missing piece was never the re-display. It was that nothing started it.

// recordingTransmit stands in for the display loop, capturing what startup asked to display.
type recordingTransmit struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (r *recordingTransmit) fn(_ context.Context, id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
}

func (r *recordingTransmit) seen() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]uuid.UUID(nil), r.ids...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// seedTransmission creates a transmission in the given state, with the file row it needs.
func seedTransmission(t *testing.T, st *store.Store, status store.TransmissionStatus) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	fileID := uuid.New()
	key, err := objectstore.Key("files", fileID.String())
	require.NoError(t, err)

	sum := sha256.Sum256([]byte("a test file"))
	file, err := st.Files.Create(ctx, store.File{
		ID: fileID, Filename: "test.bin", StoredPath: key,
		SizeBytes: 11, SHA256: sum[:],
	})
	require.NoError(t, err)

	tx, err := st.Transmissions.Create(ctx, store.Transmission{FileID: file.ID})
	require.NoError(t, err)

	// Create may impose its own starting status, so the state under test is set explicitly.
	require.NoError(t, st.Transmissions.SetStatus(ctx, tx.ID, status, ""))
	return tx.ID
}

// TestStartupResumesAnInterruptedDisplay is the bug: a transfer left transmitting by a restart gets a display
// loop again, without an operator having to find the pause-then-resume path.
func TestStartupResumesAnInterruptedDisplay(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)

	id := seedTransmission(t, st, store.TxTransmitting)

	var transmit recordingTransmit
	resumed := resumeInterruptedDisplays(context.Background(), st, transmit.fn, zap.NewNop())

	require.Equal(t, 1, resumed)
	require.Equal(t, []uuid.UUID{id}, transmit.seen(),
		"a transmitting row with no display loop behind it is the state a restart leaves, and the one the API cannot rescue")
}

// TestStartupResumesEveryInterruptedDisplay: concurrent transfers are supported — the sink assigns the display
// sequence centrally so their frames interleave rather than overwrite — so a restart owes every one of them a
// loop, not just the first.
func TestStartupResumesEveryInterruptedDisplay(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)

	first := seedTransmission(t, st, store.TxTransmitting)
	second := seedTransmission(t, st, store.TxTransmitting)

	var transmit recordingTransmit
	resumed := resumeInterruptedDisplays(context.Background(), st, transmit.fn, zap.NewNop())

	require.Equal(t, 2, resumed)
	want := []uuid.UUID{first, second}
	sort.Slice(want, func(i, j int) bool { return want[i].String() < want[j].String() })
	require.Equal(t, want, transmit.seen())
}

// TestStartupLeavesEveryOtherStatusAlone is the other half of the guarantee, and the reason this resumes one
// status rather than everything unfinished. Only a transmitting row asserts that something is displaying it,
// so only a transmitting row is a broken promise after a restart.
//
// Ready is the case worth being careful about: a transfer prepared with autostart=false is deliberately
// waiting for an operator to press start, and displaying it because the process happened to restart would
// take that decision away from them — putting a file on a screen they had chosen not to put there yet.
func TestStartupLeavesEveryOtherStatusAlone(t *testing.T) {
	for _, status := range []store.TransmissionStatus{
		store.TxPending, store.TxPreparing, store.TxReady,
		store.TxPaused, store.TxCompleted, store.TxFailed, store.TxCancelled,
	} {
		t.Run(string(status), func(t *testing.T) {
			pool := testdb.New(t)
			st := store.New(pool)
			seedTransmission(t, st, status)

			var transmit recordingTransmit
			resumed := resumeInterruptedDisplays(context.Background(), st, transmit.fn, zap.NewNop())

			require.Zero(t, resumed)
			require.Empty(t, transmit.seen())
		})
	}
}

// TestStartupWithNothingInFlightDoesNothing — the ordinary restart, which must stay silent.
func TestStartupWithNothingInFlightDoesNothing(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)

	var transmit recordingTransmit
	resumed := resumeInterruptedDisplays(context.Background(), st, transmit.fn, zap.NewNop())

	require.Zero(t, resumed)
	require.Empty(t, transmit.seen())
}
