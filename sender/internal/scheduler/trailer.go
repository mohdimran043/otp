package scheduler

import "time"

// trailerDecision is what the display should do once every chunk has been acknowledged.
type trailerDecision int

const (
	// finishNow ends the display loop and reports the transmission displayed.
	finishNow trailerDecision = iota

	// showManifest keeps the display alive, showing the manifest, because the receiver may still need it.
	showManifest
)

// afterLastAck decides whether the display is finished once no chunk is outstanding.
//
// Stopping there was wrong, and it lost transfers over a camera outright. Every chunk acknowledged does not
// mean the receiver can assemble anything: it also needs the manifest, which carries the filename, the size and
// the hash it verifies against. The manifest is one frame in a cycle re-emitted every ManifestInterval frames,
// and a small transfer is fully acknowledged long before the next one is due — five chunks take about two
// seconds at ten frames a second, against a 6.4 second manifest interval. So the display stopped with the
// receiver holding every chunk and unable to do anything with them, and the sender then waited for a completion
// report that could never arrive. Nothing recovered it; the transfer had to be cancelled and sent again.
//
// Once the chunks are in, the manifest is the only thing the receiver can still be missing, which makes showing
// it the one useful thing left to do. Two things bound it. The receiver reporting the merge ends it at once —
// that is the far side saying it needs nothing more. Failing that, the window does, because a sender displaying
// to an empty room must not hold a loop open for ever.
func afterLastAck(haveManifest, complete bool, elapsed, window time.Duration) trailerDecision {
	if complete {
		return finishNow
	}
	if !haveManifest {
		// Nothing to show, so nothing to wait for: staying on screen cannot help a receiver when there is no
		// manifest frame rendered to give it.
		return finishNow
	}
	if elapsed >= window {
		return finishNow
	}
	return showManifest
}
