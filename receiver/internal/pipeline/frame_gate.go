package pipeline

import "image"

// defaultMinToneFraction is the historical threshold: a twelfth of the samples dark and a twelfth light.
//
// Named here rather than left as a literal so config.Default and the tests refer to the same thing, and a
// change to one cannot silently disagree with the other.
const defaultMinToneFraction = 1.0 / 12.0

// looksLikeAFrame is a cheap test for "is there anything on that screen".
//
// It exists to keep a camera that is waiting from filling the failure log. Locating a frame properly means
// finding fiducials, fitting a homography and sampling every cell — hundreds of milliseconds — and doing it on
// every image of a blank screen would spend the whole receiver on discovering nothing, as well as storing a
// picture of nothing each time.
//
// The test is a property every frame this protocol renders has and almost nothing else does: **a lot of pure
// black and a lot of pure white, at once**. The quiet zone, the four fiducials and the always-binary header and
// footer bands guarantee both. A dark room has the black and none of the white; a bright blank screen has the
// white and none of the black; a photograph of a desk has plenty of midtones and little of either.
//
// It is a gate, not a decision. Anything that passes still goes through the real decoder, which will reject it
// on its checksums if it was a false positive — so the threshold is set low enough to let a badly lit or
// off-axis frame through and accept the occasional wasted decode.
//
// minFraction is how much of the image must be dark, and how much must be light, each as a fraction of the
// samples taken. It is a parameter rather than the constant it used to be because being wrong here is invisible:
// a rejected image reaches neither the decoder nor the failure log, so frames are posted, counted as "held", and
// then simply disappear. An operator cannot tell that from a decode failure, since neither leaves evidence.
//
// It bites hardest on the case it was meant to serve. A binary frame is pure black and white at source, but
// with only two levels it averages toward flat grey as soon as it is small in the viewfinder or slightly soft,
// collapsing both tails — a binary frame filling 65% of a 1920x1080 shot with mild blur already fails a
// twelfth. Relaxing it costs wasted decode attempts, which this gate's own reasoning already accepts, and
// nothing else. Zero disables the tone test entirely, leaving the decoder's checksums as the only filter, which
// is what someone debugging a camera actually wants.
func looksLikeAFrame(img image.Image, minFraction float64) bool {
	if img == nil {
		return false
	}
	bounds := img.Bounds()
	if bounds.Dx() < 32 || bounds.Dy() < 32 {
		return false
	}

	// Every eighth pixel in each direction: a sixty-fourth of the work, and a frame's bands are thousands of
	// pixels across so nothing that matters is missed.
	const step = 8
	var dark, light, total int
	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			r, g, b, _ := img.At(x, y).RGBA()
			// Rec. 601 luma on the 16-bit values the image package returns, shifted back to 0..255.
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(b>>8)) / 1000
			switch {
			case luma < 64:
				dark++
			case luma > 192:
				light++
			}
			total++
		}
	}
	if total == 0 {
		return false
	}

	// Zero, or anything meaningless, turns the tone test off and lets everything through to the decoder. The
	// size check above still applies: a 16-pixel thumbnail cannot be a frame whatever the threshold says.
	if minFraction <= 0 {
		return true
	}

	need := minFraction * float64(total)
	return float64(dark) >= need && float64(light) >= need
}

// toneGated is a source whose blank-screen threshold can be adjusted while it runs.
//
// A setter rather than a value passed at construction, because this is the one gate setting an operator changes
// *while* aiming a camera: a value fixed when the source opened would need the source reopened to take effect,
// which drops the camera the browser is holding. Sources that do not gate on tone simply do not implement it.
type toneGated interface {
	SetMinToneFraction(float64)
}

// applyToneFraction pushes the configured threshold onto a source that gates on it.
//
// Silently does nothing for a source that does not — the file source reads images that were written, not
// photographed, so there is no blank screen to detect.
func applyToneFraction(s Source, fraction float64) {
	if g, ok := s.(toneGated); ok {
		g.SetMinToneFraction(fraction)
	}
}
