package pipeline

import (
	"image"
	"image/color"
	"testing"

	"github.com/stretchr/testify/require"
)

// The frame gate's threshold, made adjustable.
//
// It was a hard-coded twelfth, which is a sound default and a bad constant. The gate discards an image before
// the decoder or the failure log ever sees it, so when it is wrong there is nothing at all to look at: frames
// are posted, "held", and vanish. An operator debugging a camera cannot tell that case from a decode failure,
// because neither produces evidence.
//
// It bites hardest on the thing it was meant to help. A binary frame is pure black and white at source, but
// with only two levels it averages to flat grey as soon as it is small in the viewfinder or slightly soft —
// so both tails collapse and the gate rejects the very frames someone is trying to get working. Measured: a
// binary frame filling 65% of a 1920x1080 shot with mild blur already fails.
//
// Lowering it costs wasted decode attempts, which the gate's own comment says are acceptable, and nothing else.

// tone builds an image with a controlled share of pure black and pure white, the rest mid-grey.
func tone(darkFrac, lightFrac float64) image.Image {
	const w, h = 320, 320
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	total := w * h
	darkUntil := int(float64(total) * darkFrac)
	lightUntil := darkUntil + int(float64(total)*lightFrac)

	for i := 0; i < total; i++ {
		x, y := i%w, i/w
		switch {
		case i < darkUntil:
			img.Set(x, y, color.RGBA{0, 0, 0, 255})
		case i < lightUntil:
			img.Set(x, y, color.RGBA{255, 255, 255, 255})
		default:
			img.Set(x, y, color.RGBA{128, 128, 128, 255})
		}
	}
	return img
}

func TestTheGateAtItsDefaultStillNeedsATwelfthOfEach(t *testing.T) {
	const aTwelfth = 1.0 / 12.0

	require.True(t, looksLikeAFrame(tone(0.20, 0.20), aTwelfth), "plenty of both passes")
	require.False(t, looksLikeAFrame(tone(0.20, 0.02), aTwelfth), "too little white fails")
	require.False(t, looksLikeAFrame(tone(0.02, 0.20), aTwelfth), "too little black fails")
}

// TestALowerThresholdAdmitsAWashedOutFrame is the point of the change: the image a blurred, distant binary
// frame produces — a little black, a little white, mostly grey — is exactly what the default turns away.
func TestALowerThresholdAdmitsAWashedOutFrame(t *testing.T) {
	washedOut := tone(0.04, 0.03)

	require.False(t, looksLikeAFrame(washedOut, 1.0/12.0), "the default rejects it")
	require.True(t, looksLikeAFrame(washedOut, 0.02), "a relaxed threshold lets it reach the decoder")
}

// TestAHigherThresholdIsStricter — adjustable in both directions, so a deployment drowning in false positives
// can tighten it rather than only loosen it.
func TestAHigherThresholdIsStricter(t *testing.T) {
	modest := tone(0.10, 0.10)

	require.True(t, looksLikeAFrame(modest, 1.0/12.0))
	require.False(t, looksLikeAFrame(modest, 0.15))
}

// TestAZeroThresholdDisablesTheGate: the escape hatch. Everything reaches the decoder, which is what someone
// debugging a camera wants — the decoder's own checksums are the real filter, and the gate is only an
// optimisation.
func TestAZeroThresholdDisablesTheGate(t *testing.T) {
	require.True(t, looksLikeAFrame(tone(0, 0), 0), "flat grey passes when the gate is off")
}

// TestTheGateStillRejectsWhatCannotBeAFrame — the size guard is not a tone question and must survive any
// threshold, including zero.
func TestTheGateStillRejectsWhatCannotBeAFrame(t *testing.T) {
	tiny := image.NewRGBA(image.Rect(0, 0, 16, 16))
	require.False(t, looksLikeAFrame(tiny, 0))
	require.False(t, looksLikeAFrame(nil, 0))
}
