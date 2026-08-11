package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// What the display does once every chunk has been acknowledged.
//
// It used to stop, and that lost transfers over a camera. Acknowledging every chunk does not mean the receiver
// can assemble anything — it also needs the manifest, which carries the filename, the size and the hash. The
// manifest is one frame in a cycle re-emitted every 64 frames, and a five-chunk transfer is fully acknowledged
// in about two seconds at ten frames a second, well before the next manifest is due. So the display stopped
// with the receiver holding every chunk, unable to merge, and the sender waited for a completion report that
// could never come. Nothing in the system recovered.
//
// Once the chunks are in, the manifest is the *only* thing the receiver can still be missing, so showing it is
// the one useful thing left to do. It stops as soon as the receiver reports the file merged, and it is bounded
// so a sender with nobody watching does not display forever.

func TestAfterLastAckShowsTheManifestWhileWaitingToBeTold(t *testing.T) {
	require.Equal(t, showManifest, afterLastAck(true, false, time.Second, 30*time.Second))
}

// The receiver has reported the merge, so there is nothing left to say.
func TestAfterLastAckFinishesOnceTheReceiverHasReported(t *testing.T) {
	require.Equal(t, finishNow, afterLastAck(true, true, time.Second, 30*time.Second))
}

// Bounded, because a sender displaying to an empty room would otherwise never stop and would hold a display
// loop and its goroutine open for ever.
func TestAfterLastAckGivesUpAfterTheWindow(t *testing.T) {
	require.Equal(t, finishNow, afterLastAck(true, false, 31*time.Second, 30*time.Second))
}

func TestAfterLastAckFinishesAtExactlyTheWindow(t *testing.T) {
	require.Equal(t, finishNow, afterLastAck(true, false, 30*time.Second, 30*time.Second))
}

// Nothing to show means nothing to wait for. A transmission with no manifest frame rendered cannot help a
// receiver by staying on screen.
func TestAfterLastAckFinishesWhenThereIsNoManifestToShow(t *testing.T) {
	require.Equal(t, finishNow, afterLastAck(false, false, time.Second, 30*time.Second))
}

// A zero window disables the trailer, which is what an operator setting the acknowledgement timeout to nothing
// is asking for.
func TestAfterLastAckFinishesImmediatelyWhenTheWindowIsZero(t *testing.T) {
	require.Equal(t, finishNow, afterLastAck(true, false, 0, 0))
}
