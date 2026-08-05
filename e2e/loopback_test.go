package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/opticaltransport/otp/shared/simulate"

	receiver "github.com/opticaltransport/otp/receiver/harness"
	sender "github.com/opticaltransport/otp/sender/harness"

	"github.com/opticaltransport/otp/e2e"
)

// databaseURLs returns the two databases the loopback needs, or skips.
//
// Two of them, because the sender and the receiver have separate databases in every real deployment.
// Sharing one would hide any place the code had assumed otherwise, which is exactly the kind of
// assumption an end-to-end test exists to catch.
func databaseURLs(t *testing.T) (senderURL, receiverURL string) {
	t.Helper()
	senderURL = os.Getenv("OTP_TEST_SENDER_DATABASE_URL")
	receiverURL = os.Getenv("OTP_TEST_RECEIVER_DATABASE_URL")
	if senderURL == "" || receiverURL == "" {
		t.Skip("set OTP_TEST_SENDER_DATABASE_URL and OTP_TEST_RECEIVER_DATABASE_URL to run the loopback tests")
	}
	return senderURL, receiverURL
}

// start brings up a loopback for one test, on databases of its own.
func start(t *testing.T, opts e2e.Options) *e2e.Loopback {
	t.Helper()

	senderURL, receiverURL := databaseURLs(t)
	opts.SenderDatabaseURL = uniqueDatabase(t, senderURL, "sender")
	opts.ReceiverDatabaseURL = uniqueDatabase(t, receiverURL, "receiver")
	opts.Root = t.TempDir()
	if opts.Log == nil {
		// Errors only. A transfer logs a line per frame, and at two hundred frames a second the log
		// would be the slowest part of the test.
		opts.Log = zaptest.NewLogger(t, zaptest.Level(zap.ErrorLevel))
	}

	loop, err := e2e.Start(context.Background(), opts)
	require.NoError(t, err)
	t.Cleanup(loop.Stop)
	return loop
}

// payload builds a file that compresses like a real one: mostly structure, with enough entropy that the
// chunk count is not an artefact of how well it happens to compress.
func payload(size int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	out := make([]byte, 0, size+64)
	lines := []string{
		"acknowledgements travel through shared storage and never optically\n",
		"every captured frame is written to disk before it is decoded\n",
		"a chunk is sized so that exactly one chunk fits in exactly one frame\n",
		"the sender does not move on until the receiver says the chunk arrived\n",
	}
	for len(out) < size {
		out = append(out, lines[r.Intn(len(lines))]...)
		var noise [24]byte
		r.Read(noise[:])
		out = append(out, noise[:]...)
	}
	return out[:size]
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestLoopbackDeliversAFileWithNoLoss is the whole platform in one test.
//
// A caller posts a file and a callback URL to the sender's API. The file is compressed, chunked,
// error-coded, and rendered to frames; the frames cross a virtual optical channel; the receiver decodes
// them, acknowledges each chunk, merges the file, checks it against the hash the manifest declared,
// delivers it to the callback URL, and reports back. The assertions are the claims that matter: nothing
// was lost, the bytes are identical, and the callback received them.
func TestLoopbackDeliversAFileWithNoLoss(t *testing.T) {
	loop := start(t, e2e.Options{})
	ctx := context.Background()

	original := payload(64<<10, 1)
	accepted, err := loop.Transfer(ctx, "quarterly-report.tar", original, loop.Callback.URL())
	require.NoError(t, err)
	require.Equal(t, hashOf(original), accepted.SHA256,
		"the sender must hash exactly the bytes it was given")
	require.Equal(t, loop.Callback.URL(), accepted.CallbackURL)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 4*time.Minute)
	require.NoError(t, err, "the receiver never reported on the transfer")

	// The receiver's own verdict: it merged the file and checked it against the manifest.
	require.True(t, result.Verified, "the receiver could not verify the merged file: %s", result.Error)
	require.Equal(t, hashOf(original), result.SHA256, "the merged file must hash to what was uploaded")
	require.Equal(t, uint64(len(original)), result.Size)
	require.Equal(t, result.ChunksExpected, result.ChunksReceived,
		"every chunk must have arrived: %d of %d", result.ChunksReceived, result.ChunksExpected)

	// The sender's view: nothing outstanding. This is the zero-loss claim from the transmitting side, and
	// it is independent of the receiver's — the sender only marks a chunk acknowledged when a signed
	// record for it arrives.
	unacked, err := loop.Sender.UnackedChunks(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Empty(t, unacked, "chunks were still outstanding: %v", unacked)

	transfer, err := loop.Sender.Transfer(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Equal(t, "completed", transfer.Status)
	require.Equal(t, transfer.ChunkCount, transfer.AckedChunks)

	// The receiver's copy, byte for byte. The hash agreeing is strong evidence; the bytes agreeing is the
	// thing itself.
	merged, err := loop.Receiver.MergedBytes(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Equal(t, original, merged, "the receiver's file must be byte-identical")

	// And the callback: the file arrived at the URL the caller nominated, with a hash the endpoint
	// recomputed for itself rather than taking the receiver's word for.
	require.True(t, result.CallbackDelivered, "the callback was not delivered: %s", result.CallbackError)
	require.Equal(t, 200, result.CallbackStatus)

	deliveries := loop.Callback.Deliveries()
	require.Len(t, deliveries, 1)
	require.Equal(t, accepted.TransmissionID.String(), deliveries[0].TransmissionID)
	require.Equal(t, "quarterly-report.tar", deliveries[0].Filename)
	require.Equal(t, hashOf(original), deliveries[0].DeclaredSHA256)
	require.Equal(t, hashOf(original), deliveries[0].ActualSHA256,
		"the delivered body must hash to what was uploaded")
	require.Equal(t, original, deliveries[0].Body)

	// The sender records what became of the callback, so a caller can ask it rather than reaching across
	// the air gap.
	callbacks, err := loop.Sender.Callbacks(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Len(t, callbacks, 1)
	require.True(t, callbacks[0].Delivered)
	require.Equal(t, "delivered", callbacks[0].Status)

	t.Logf("transferred %d bytes in %s (%.0f bytes/second), %d chunks, %d frames captured, %d unreadable",
		result.Size, result.Duration(), result.ThroughputBytesPerSecond(),
		result.ChunksReceived, result.FramesCaptured, result.FramesFailed)
}

// TestLoopbackRecoversFromDroppedFrames is the zero-loss claim under a channel that loses frames.
//
// One frame in four is discarded before the receiver ever sees it, which is what a tear, a hand across
// the lens, or a refresh caught mid-scan does. Nothing about a dropped frame is detectable when it
// happens — there is no report, only silence — so the only things that can recover it are parity and the
// sender declining to move on.
func TestLoopbackRecoversFromDroppedFrames(t *testing.T) {
	loop := start(t, e2e.Options{
		Drop: func(sequence int64) bool { return sequence%4 == 0 },
	})
	ctx := context.Background()

	original := payload(48<<10, 2)
	accepted, err := loop.Transfer(ctx, "lossy.bin", original, loop.Callback.URL())
	require.NoError(t, err)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 5*time.Minute)
	require.NoError(t, err)

	require.True(t, result.Verified, "the file must still verify: %s", result.Error)
	require.Equal(t, hashOf(original), result.SHA256)
	require.Equal(t, result.ChunksExpected, result.ChunksReceived)

	unacked, err := loop.Sender.UnackedChunks(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Empty(t, unacked)

	merged, err := loop.Receiver.MergedBytes(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Equal(t, original, merged)

	// The transfer had to work for it: either frames were re-displayed, or the gaps were filled from
	// parity. Both are legitimate answers to loss, and asserting that neither happened would mean the
	// dropping was not taking effect.
	transfer, err := loop.Sender.Transfer(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Greater(t, transfer.Retransmits+int(result.ChunksRecovered), 0,
		"a quarter of the frames were dropped, so something must have had to make up for it")

	t.Logf("one frame in four dropped: %d retransmissions, %d chunks recovered from parity, %d frames captured",
		transfer.Retransmits, result.ChunksRecovered, result.FramesCaptured)
}

// TestRetransmissionAloneRecoversEveryChunk isolates the acknowledgement mechanism.
//
// Error correction is switched off, so parity cannot quietly cover the losses, and a third of the frames
// are discarded before the receiver sees them. The only thing left that can deliver the file is the
// sender declining to move on: a chunk stays in the window, and keeps being displayed, until a signed
// acknowledgement for it arrives.
//
// It also demonstrates the other half of that rule. Once a chunk is acknowledged it is never displayed
// again, so the display converges on what is still missing instead of cycling through what has already
// arrived — which is why the frame count stays bounded rather than growing until the retry ceiling.
func TestRetransmissionAloneRecoversEveryChunk(t *testing.T) {
	loop := start(t, e2e.Options{
		// One frame in three never reaches the receiver.
		Drop: func(sequence int64) bool { return sequence%3 == 0 },
		TuneSender: func(o *sender.Options) {
			o.FECCodec = "none"
			o.FECDataShards = 0
			o.FECParityShards = 0
			// A short timeout, so the retransmission path is exercised many times over rather than once.
			o.AckTimeout = 300 * time.Millisecond
		},
	})
	ctx := context.Background()

	original := payload(40<<10, 9)
	accepted, err := loop.Transfer(ctx, "retransmit-only.bin", original, loop.Callback.URL())
	require.NoError(t, err)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 5*time.Minute)
	require.NoError(t, err)

	require.True(t, result.Verified, "the file must arrive intact on retransmission alone: %s", result.Error)
	require.Equal(t, hashOf(original), result.SHA256)
	require.Equal(t, result.ChunksExpected, result.ChunksReceived)
	require.Zero(t, result.ChunksRecovered, "with no parity, nothing can be recovered from it")

	unacked, err := loop.Sender.UnackedChunks(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Empty(t, unacked, "chunks were still outstanding: %v", unacked)

	merged, err := loop.Receiver.MergedBytes(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Equal(t, original, merged)

	transfer, err := loop.Sender.Transfer(ctx, accepted.TransmissionID)
	require.NoError(t, err)

	stats, err := loop.Sender.WaitForDisplay(ctx, accepted.TransmissionID, time.Minute)
	require.NoError(t, err)

	// Something had to show those chunks again, and the scheduler has two ways of doing it: a
	// retransmission once the acknowledgement timeout has passed, or a keep-alive repeat of the oldest
	// outstanding frame when the window is waiting and there is nothing new to show. On a small transfer
	// the keep-alive usually gets there first, which is the better outcome — it recovers the loss without
	// waiting out a timeout — so what is asserted is that a repeat happened, not which kind.
	require.Positive(t, stats.Retransmissions+stats.KeepAlives,
		"a third of the frames were dropped with no parity, so frames must have been shown again")
	require.Greater(t, stats.FramesShown, int64(transfer.ChunkCount),
		"more frames must have been shown than there are chunks")

	// And acknowledged chunks stop being shown, measured where it can actually be seen: no frame went to
	// the display long after its chunk was known to have arrived.
	late, err := loop.Sender.LateDisplays(ctx, accepted.TransmissionID, 2*time.Second)
	require.NoError(t, err)
	require.Empty(t, late, "frames were displayed after their chunk was acknowledged: %+v", late)

	t.Logf("no parity, one frame in three dropped: %d chunks, %d retransmissions, %d keep-alives, %d frames shown, %d captured",
		transfer.ChunkCount, stats.Retransmissions, stats.KeepAlives, stats.FramesShown, result.FramesCaptured)
}

// TestAcknowledgedChunksAreNotShownAgain checks the skipping directly on a clean channel.
//
// With nothing being lost, every chunk is acknowledged the first time it is displayed, so the display
// should show each frame about once and stop. A scheduler that ignored acknowledgements would keep
// cycling until its retry ceiling instead — the difference is visible in a single number.
func TestAcknowledgedChunksAreNotShownAgain(t *testing.T) {
	loop := start(t, e2e.Options{})
	ctx := context.Background()

	original := payload(48<<10, 10)
	accepted, err := loop.Transfer(ctx, "clean.bin", original, loop.Callback.URL())
	require.NoError(t, err)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 4*time.Minute)
	require.NoError(t, err)
	require.True(t, result.Verified)

	transfer, err := loop.Sender.Transfer(ctx, accepted.TransmissionID)
	require.NoError(t, err)

	stats, err := loop.Sender.WaitForDisplay(ctx, accepted.TransmissionID, time.Minute)
	require.NoError(t, err)

	// Nothing was dropped, so nothing needed resending.
	require.Zero(t, transfer.Retransmits, "nothing was dropped, so nothing should have been resent")

	// The claim itself: not one frame went to the display more than a couple of seconds after its chunk
	// had been acknowledged. The display does keep busy while a window is waiting — that is deliberate,
	// since a camera pointed at a blank screen learns nothing — but the frames it repeats are always ones
	// still outstanding.
	late, err := loop.Sender.LateDisplays(ctx, accepted.TransmissionID, 2*time.Second)
	require.NoError(t, err)
	require.Empty(t, late, "frames were displayed after their chunk was acknowledged: %+v", late)

	t.Logf("clean channel: %d chunks, %d frames rendered, %d frames shown, %d keep-alives",
		transfer.ChunkCount, transfer.FrameCount, stats.FramesShown, stats.KeepAlives)
}

// TestLoopbackThroughADegradedChannel runs the transfer through the optics rather than over a perfect
// wire: blur, sensor noise, an off-axis camera, vignetting, and JPEG artefacts.
func TestLoopbackThroughADegradedChannel(t *testing.T) {
	loop := start(t, e2e.Options{
		Degrade: simulate.Typical,
		// A larger cell for a realistic capture, which is what the operating envelope says a normal
		// installation needs.
		TuneSender: func(o *sender.Options) { o.CellPixels = 8 },
	})
	ctx := context.Background()

	original := payload(32<<10, 3)
	accepted, err := loop.Transfer(ctx, "through-optics.bin", original, loop.Callback.URL())
	require.NoError(t, err)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 5*time.Minute)
	require.NoError(t, err)

	require.True(t, result.Verified, "the file must verify through a degraded channel: %s", result.Error)
	require.Equal(t, hashOf(original), result.SHA256)

	merged, err := loop.Receiver.MergedBytes(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Equal(t, original, merged)

	stats, err := loop.Receiver.CaptureStats(ctx)
	require.NoError(t, err)
	t.Logf("through a typical optical path: %d frames captured, %d decoded, %d unreadable",
		stats.FramesCaptured, stats.FramesDecoded, stats.FramesFailed)
}

// TestRepeatedTransfersLoseNothing runs the same transfer several times over.
//
// A single passing run of a system with this many moving parts — a display loop, a capture loop, an
// acknowledgement directory, retransmission timers — is weak evidence. Loss and races are probabilistic,
// so the claim worth making is that many runs in a row lose nothing, and each run uses different data so
// one cannot pass on something the previous left behind.
func TestRepeatedTransfersLoseNothing(t *testing.T) {
	const runs = 5
	loop := start(t, e2e.Options{})
	ctx := context.Background()

	for i := 0; i < runs; i++ {
		t.Run(fmt.Sprintf("run-%d", i+1), func(t *testing.T) {
			original := payload(24<<10, int64(100+i))
			name := fmt.Sprintf("repeat-%d.bin", i+1)

			accepted, err := loop.Transfer(ctx, name, original, loop.Callback.URL())
			require.NoError(t, err)

			result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 4*time.Minute)
			require.NoError(t, err)

			require.True(t, result.Verified, "run %d did not verify: %s", i+1, result.Error)
			require.Equal(t, hashOf(original), result.SHA256)
			require.Equal(t, result.ChunksExpected, result.ChunksReceived)

			unacked, err := loop.Sender.UnackedChunks(ctx, accepted.TransmissionID)
			require.NoError(t, err)
			require.Empty(t, unacked, "run %d left chunks outstanding: %v", i+1, unacked)

			merged, err := loop.Receiver.MergedBytes(ctx, accepted.TransmissionID)
			require.NoError(t, err)
			require.Equal(t, original, merged, "run %d merged the wrong bytes", i+1)

			require.True(t, result.CallbackDelivered)
			t.Logf("run %d: %d bytes, %d chunks, %.0f bytes/second",
				i+1, result.Size, result.ChunksReceived, result.ThroughputBytesPerSecond())
		})
	}

	// Every run delivered exactly once. A duplicate delivery would mean the receiver had merged a
	// transmission twice, which is what a retransmission arriving after completion would cause.
	require.Len(t, loop.Callback.Deliveries(), runs)
}

// TestEveryEncodingCarriesTheFile runs the loop under each optical encoding, so a new encoding cannot be
// added to the protocol without the whole path being shown to work with it.
func TestEveryEncodingCarriesTheFile(t *testing.T) {
	for _, encoder := range []string{"binary", "grayscale", "color8", "color16", "rolling"} {
		t.Run(encoder, func(t *testing.T) {
			name := encoder
			loop := start(t, e2e.Options{
				TuneSender: func(o *sender.Options) { o.Encoder = name },
			})
			ctx := context.Background()

			original := payload(16<<10, 4)
			accepted, err := loop.Transfer(ctx, encoder+".bin", original, loop.Callback.URL())
			require.NoError(t, err)

			result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 4*time.Minute)
			require.NoError(t, err)
			require.True(t, result.Verified, "%s did not verify: %s", encoder, result.Error)
			require.Equal(t, hashOf(original), result.SHA256)

			unacked, err := loop.Sender.UnackedChunks(ctx, accepted.TransmissionID)
			require.NoError(t, err)
			require.Empty(t, unacked)
		})
	}
}

// TestEncryptedTransferIsDelivered runs the loop with payload encryption on, so the frames on the channel
// carry ciphertext and the receiver has to hold the matching key.
func TestEncryptedTransferIsDelivered(t *testing.T) {
	const key = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

	loop := start(t, e2e.Options{
		TuneSender:   func(o *sender.Options) { o.EncryptionKeyHex = key },
		TuneReceiver: func(o *receiver.Options) { o.EncryptionKeyHex = key },
	})
	ctx := context.Background()

	original := payload(24<<10, 5)
	accepted, err := loop.Transfer(ctx, "confidential.bin", original, loop.Callback.URL())
	require.NoError(t, err)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 4*time.Minute)
	require.NoError(t, err)
	require.True(t, result.Verified, "the encrypted transfer did not verify: %s", result.Error)
	require.Equal(t, hashOf(original), result.SHA256)

	merged, err := loop.Receiver.MergedBytes(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Equal(t, original, merged)

	deliveries := loop.Callback.Deliveries()
	require.Len(t, deliveries, 1)
	require.Equal(t, original, deliveries[0].Body)
}

// TestCallbackFailureIsReportedNotHidden covers the endpoint rejecting a delivery.
//
// The transfer itself worked — every chunk arrived and the file verified — so the sender must be told two
// separate things: that the file is intact, and that handing it on did not succeed. Collapsing those into
// one answer would mean a caller could not tell a transport failure from an endpoint that was down.
func TestCallbackFailureIsReportedNotHidden(t *testing.T) {
	loop := start(t, e2e.Options{})
	ctx := context.Background()

	// Every attempt the receiver makes during this transfer is rejected.
	loop.Callback.FailFirst(100)

	original := payload(16<<10, 6)
	accepted, err := loop.Transfer(ctx, "undeliverable.bin", original, loop.Callback.URL())
	require.NoError(t, err)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 4*time.Minute)
	require.NoError(t, err)

	require.True(t, result.Verified, "the file itself must still be intact")
	require.Equal(t, hashOf(original), result.SHA256)
	require.False(t, result.CallbackDelivered, "a rejected delivery must not be reported as delivered")
	require.Equal(t, 500, result.CallbackStatus)
	require.NotEmpty(t, result.CallbackError)

	// And the receiver kept the merged file, so the delivery can be retried rather than the transfer
	// having to be repeated across the optical channel.
	merged, err := loop.Receiver.MergedFile(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.True(t, merged.Verified)
	require.Equal(t, hashOf(original), merged.SHA256)

	callbacks, err := loop.Sender.Callbacks(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Len(t, callbacks, 1)
	require.False(t, callbacks[0].Delivered)
	require.Equal(t, "failed", callbacks[0].Status)
}

// TestCallbackHostMustBeAllowed is a security test. The callback URL crosses the optical channel from
// outside the receiver's trust boundary and is then turned into an outbound request — so a receiver that
// acted on any URL it was handed would be a request-forgery proxy for whoever controls the sender.
func TestCallbackHostMustBeAllowed(t *testing.T) {
	loop := start(t, e2e.Options{
		TuneReceiver: func(o *receiver.Options) {
			// An allowlist that does not include the callback endpoint's host.
			o.AllowedCallbackHosts = []string{"deliveries.example.com"}
		},
	})
	ctx := context.Background()

	original := payload(8<<10, 7)
	accepted, err := loop.Transfer(ctx, "refused.bin", original, loop.Callback.URL())
	require.NoError(t, err)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 4*time.Minute)
	require.NoError(t, err)

	require.True(t, result.Verified, "the transfer itself must still succeed")
	require.False(t, result.CallbackDelivered)
	require.Contains(t, result.CallbackError, "not in the allowed-hosts list")
	require.Empty(t, loop.Callback.Deliveries(), "nothing may be delivered to a host that is not allowed")
}

// TestTransferSurvivesADegradedAndLossyChannel is the hardest case: the optics are poor *and* frames are
// being missed.
func TestTransferSurvivesADegradedAndLossyChannel(t *testing.T) {
	loop := start(t, e2e.Options{
		Degrade:    simulate.Typical,
		Drop:       func(sequence int64) bool { return sequence%5 == 0 },
		TuneSender: func(o *sender.Options) { o.CellPixels = 8 },
	})
	ctx := context.Background()

	original := payload(24<<10, 8)
	accepted, err := loop.Transfer(ctx, "hard.bin", original, loop.Callback.URL())
	require.NoError(t, err)

	result, err := loop.WaitForResult(ctx, accepted.TransmissionID, 6*time.Minute)
	require.NoError(t, err)

	require.True(t, result.Verified, "the file must verify: %s", result.Error)
	require.Equal(t, hashOf(original), result.SHA256)
	require.Equal(t, result.ChunksExpected, result.ChunksReceived)

	merged, err := loop.Receiver.MergedBytes(ctx, accepted.TransmissionID)
	require.NoError(t, err)
	require.Equal(t, original, merged)

	stats, err := loop.Receiver.CaptureStats(ctx)
	require.NoError(t, err)
	t.Logf("degraded and lossy: %d frames captured, %d unreadable, %d chunks recovered from parity",
		stats.FramesCaptured, stats.FramesFailed, result.ChunksRecovered)
}
