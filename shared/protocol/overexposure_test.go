package protocol_test

import (
	"image"
	"image/color"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// overexpose reproduces what a phone camera does to a monitor in a dim room.
//
// The camera meters for the bezel and the wall, both far darker than the screen, and drives the
// screen itself up into the clip. Gain then ceiling, in that order, because that is the order the
// sensor applies them: values above the ceiling are not compressed, they are lost, and the whole
// difficulty of this case is that the loss is unrecoverable rather than merely inconvenient.
func overexpose(src image.Image, gain float64) image.Image {
	b := src.Bounds()
	out := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			clip := func(v uint32) uint8 {
				scaled := float64(v>>8) * gain
				if scaled > 255 {
					return 255
				}
				return uint8(scaled)
			}
			out.Set(x, y, color.RGBA{clip(r), clip(g), clip(bl), 255})
		}
	}
	return out
}

// fillPayload writes bright noise across the data area.
//
// renderBands leaves the payload blank, which is fine for testing geometry on a clean frame but
// removes the very thing that makes overexposure dangerous: it is the *payload* next to a fiducial
// that clips, welding the fiducial's bright outer ring to the field beside it so that connected
// component analysis sees one shape instead of two. A blank payload cannot reproduce that, so the
// noise is what makes this test about the real failure.
func fillPayload(src image.Image, l protocol.Layout) image.Image {
	b := src.Bounds()
	out := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(x, y, src.At(x, y))
		}
	}

	// Strictly the data area: everything between the two fixed bands. The fiducials and the
	// descriptor blocks live inside those bands, so staying between them leaves every cell the
	// geometry search reads exactly as rendered, and only the payload is made bright.
	//
	// One value per cell rather than per pixel. Per-pixel noise is not what a camera sees — the
	// sender renders whole cells — and it invents high-frequency detail that produces spurious
	// components of its own, which would make this test fail for a reason that has nothing to do
	// with exposure.
	rng := rand.New(rand.NewSource(1))
	topCell, bottomCell := l.HeaderBandRows, l.GridHeight-l.FooterBandRows
	for cy := topCell; cy < bottomCell; cy++ {
		for cx := 0; cx < l.GridWidth; cx++ {
			// Biased bright, as a colour payload photographed off a screen is: the dark levels of an
			// eight-colour palette survive a camera far less often than the light ones.
			v := uint8(150 + rng.Intn(106))
			for y := 0; y < l.CellPixels; y++ {
				for x := 0; x < l.CellPixels; x++ {
					out.Set(b.Min.X+cx*l.CellPixels+x, b.Min.Y+cy*l.CellPixels+y, color.RGBA{v, v, v, 255})
				}
			}
		}
	}
	return out
}

// A frame whose payload has been driven into the clip must still be located.
//
// This is the case that took the receiver to nought decoded frames in a row: the fiducials are
// present and sharp in the capture, and the detector cannot find four of them because the bright
// field beside them has been flattened to the same white as their outer ring.
func TestLocateThroughOverexposure(t *testing.T) {
	l, err := protocol.NewLayout(protocol.DefaultGridWidth, protocol.DefaultGridHeight, protocol.DefaultCellPixels)
	require.NoError(t, err)
	want := sampleHeader()

	// A real lens and sensor, then the clip on top. Clipping a pristine render proves nothing:
	// perfect square edges survive almost any photometry, and it is the combination of ordinary
	// defocus with a payload driven to white that defeats the detector in practice.
	img := simulate.Typical.Apply(fillPayload(renderBands(t, l, want), l))

	// Sanity: the same optics without the clip must locate, or this test is measuring the blur
	// rather than the exposure.
	_, err = protocol.Locate(img, protocol.LocateOptions{CellPixelsHint: l.CellPixels})
	require.NoError(t, err, "the un-clipped frame must locate, otherwise this test proves nothing")

	blown := overexpose(img, 1.7)
	g, err := protocol.Locate(blown, protocol.LocateOptions{CellPixelsHint: l.CellPixels})
	require.NoError(t, err, "a clipped frame must still be located")
	assert.Equal(t, want, g.Header, "the header must survive the clip")
}
