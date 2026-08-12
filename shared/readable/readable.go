// Package readable answers one question before a transfer is spent: can a camera of this resolution read
// a frame of this geometry at all?
//
// It exists because the answer is arithmetic and was being discovered by experiment. A frame is square, so
// its width on the sensor is bounded by the *short* side of the picture however the camera is held. Divide
// that by the cells spanning it — the grid plus its quiet zone — and you have the most pixels per cell that
// capture can ever produce, at any distance, with perfect aim. If that figure is below what the encoding
// needs, the transfer cannot work and no amount of moving, focusing or processing changes it.
//
// Measured, this is not a marginal effect. A 128-cell grid photographed in portrait at 1080x1920 tops out at
// 1080/132 = 8.2 pixels a cell; colour8 needs about ten. The same grid in landscape gets 1920/132 = 14.5 and
// reads every frame. The difference between "never works" and "always works" is which way the phone was
// held, and nothing in the numbers an operator sees says so — the aiming display reported "move closer" at
// 86% of the view filled, advice that could not be followed.
//
// The model is deliberately coarse. It assumes the frame fills the short side, which is optimistic, and it
// ignores blur, exposure and compression, which are not free. So a geometry this package calls readable may
// still fail for other reasons; one it calls unreadable will certainly fail. That asymmetry is the useful
// one: it is a floor, and a floor is what a refusal should rest on.
package readable

import "fmt"

// Pixels per cell each modulation needs, measured on this project's own captures.
const (
	// BinaryPixels is the smallest cell a one-bit frame reads reliably. A binary cell is thresholded —
	// above or below a level — so it survives on very little.
	BinaryPixels = 4.0

	// ColourPixels is what a multi-level cell needs, and it is several times the binary figure for a
	// reason that is not obvious. A colour cell is not thresholded but *measured*, against eight palette
	// entries a fixed distance apart, so its accuracy is set by how many pixels were averaged into it.
	// Measured: 12 pixels a cell decoded every frame, 8.5 decoded about one in a hundred, 5.9 never
	// decoded at all.
	ColourPixels = 10.0

	// MaxUsefulPixels is where more resolution starts to hurt. Past about this, a sensor resolves the
	// display's own pixel structure — the subpixel stripes of an LCD — and a cell stops being one colour.
	// Closer is not better.
	MaxUsefulPixels = 13.0
)

// Required is the pixels per cell a frame at this bit depth needs.
func Required(bitDepth uint8) float64 {
	if bitDepth > 1 {
		return ColourPixels
	}
	return BinaryPixels
}

// Assessment is what a given capture can do with a given geometry.
type Assessment struct {
	// ModulePixels is the best this capture could resolve, with the frame filling the short side.
	ModulePixels float64

	// Rotated is the same figure with the camera turned ninety degrees, so the long side of the picture
	// bounds the frame instead of the short one.
	Rotated float64

	// Required is what the encoding needs.
	Required float64

	// Readable reports whether ModulePixels reaches Required.
	Readable bool

	// RotationHelps is true when turning the camera would be enough on its own. It is worth reporting
	// separately because it is the cheapest fix there is — a wrist movement, no reconfiguration, no
	// second transfer.
	RotationHelps bool

	// MaxGrid is the largest grid this capture could read at this bit depth, and MaxGridRotated the same
	// held the other way. Reported as cell counts because that is what an operator chooses from.
	MaxGrid        int
	MaxGridRotated int

	// BinaryWouldWork reports whether dropping to one bit a cell would make this grid readable. Large
	// grids are reachable in binary and not in colour, because binary needs less than half the pixels —
	// which is the trade an operator wanting a dense grid actually has to make.
	BinaryWouldWork bool
}

// Assess measures a geometry against a capture resolution.
//
// captureW and captureH are the pixel dimensions of the photograph, in either order — the short side is
// taken, since that is what bounds a square frame.
func Assess(gridWidth, quietZone int, bitDepth uint8, captureW, captureH int) Assessment {
	cells := float64(gridWidth + 2*quietZone)
	short, long := float64(captureW), float64(captureH)
	if short > long {
		short, long = long, short
	}

	a := Assessment{Required: Required(bitDepth)}
	if cells <= 0 || short <= 0 {
		return a
	}

	a.ModulePixels = short / cells
	a.Rotated = long / cells
	a.Readable = a.ModulePixels >= a.Required
	a.RotationHelps = !a.Readable && a.Rotated >= a.Required

	a.MaxGrid = int(short/a.Required) - 2*quietZone
	a.MaxGridRotated = int(long/a.Required) - 2*quietZone
	a.BinaryWouldWork = !a.Readable && a.ModulePixels >= BinaryPixels

	return a
}

// Explain states the problem and the ways out, in the order they cost an operator least.
//
// Rotation first when it suffices, because it costs a wrist movement. Then dropping to binary, which keeps
// the grid and trades bits per cell. Then a smaller grid, which is the last resort because it is the one
// that reduces what a frame carries.
func (a Assessment) Explain(gridWidth int, bitDepth uint8) string {
	if a.Readable {
		return fmt.Sprintf(
			"A %d-cell grid resolves to about %.1f pixels a cell on this camera, against the %.0f this "+
				"encoding needs.", gridWidth, a.ModulePixels, a.Required)
	}

	msg := fmt.Sprintf(
		"A %d-cell grid cannot be read by this camera: cells reach only %.1f pixels even with the frame "+
			"filling the view, and this encoding needs about %.0f. A frame is square, so its width is "+
			"bounded by the short side of the picture — moving closer cannot change the figure.",
		gridWidth, a.ModulePixels, a.Required)

	switch {
	case a.RotationHelps:
		msg += fmt.Sprintf(
			" Turn the camera sideways and cells become %.1f pixels, which is enough on its own — that is "+
				"the whole fix, and it costs nothing.", a.Rotated)
	case a.BinaryWouldWork && bitDepth > 1:
		msg += fmt.Sprintf(
			" At one bit a cell this grid would read, since binary needs about %.0f pixels rather than "+
				"%.0f — the same grid, a third of the payload. Otherwise use %d cells or fewer.",
			BinaryPixels, a.Required, max(a.MaxGrid, 0))
	default:
		msg += fmt.Sprintf(" Use %d cells or fewer", max(a.MaxGrid, 0))
		if a.MaxGridRotated > a.MaxGrid {
			msg += fmt.Sprintf(", or %d with the camera sideways", a.MaxGridRotated)
		}
		msg += "."
	}
	return msg
}
