// Package readable answers one question before a transfer is spent: can a camera of this resolution read
// a frame of this geometry at all?
//
// It exists because the answer is arithmetic and was being discovered by experiment. A frame is square, so
// its width on the sensor is bounded by the *short* side of the picture however the camera is held. Divide
// that by the cells spanning it — the grid plus its quiet zone — and you have the most pixels per cell that
// capture can ever produce, at any distance, with perfect aim. If that figure is below what the encoding
// needs, the transfer cannot work and no amount of moving, focusing or processing changes it.
//
// Measured, this is not a small effect. A 128-cell grid against a 1080-pixel capture tops out at 1080/132 =
// 8.2 pixels a cell where colour8 wants ten, and the transfers at that geometry acknowledged 3 and 24 chunks
// of 74 before exhausting their retransmissions. The aiming display meanwhile said "move closer" at 86% of
// the view filled, which is advice that cannot be followed.
//
// Only the short side counts, and an earlier version of this package got that wrong in a way worth recording.
// It reported what the long side would give and advised turning the camera — 8.2 against 14.5 — which does
// nothing at all: a square inscribed in a 1920x1080 picture is 1080 across, and so is one in a 1080x1920
// picture. The capture is requested as 1920x1080 either way. That advice was given to an operator who was
// about to act on it, and the only reason it was caught is that they asked why the numbers had not moved.
//
// So the two things that actually change the figure are the grid and the capture resolution. Nothing else.
//
// The model is deliberately coarse. It assumes the frame fills the short side, which is optimistic, and it
// ignores blur, exposure and compression, which are not free. So a geometry this package calls readable may
// still fail for other reasons; one it calls unreadable will certainly fail. That asymmetry is the useful
// one: it is a floor, and a floor is what a refusal should rest on.
package readable

import (
	"fmt"
	"math"
)

// Pixels per cell each modulation needs, measured on this project's own captures.
const (
	// BinaryPixels is the smallest cell a one-bit frame reads reliably, and it is the least trustworthy
	// number in this file. Read the history before relying on it.
	//
	// It was 3, taken from the aiming display's original constant, which reasoned that a binary cell only
	// has to land on the correct side of one threshold so a coarse read is still the right read. That is
	// sound as far as it goes. Then it was briefly 4, an estimate written here, and for a while the two
	// disagreed — the receiver told an operator a geometry needed 3 while the sender refused it for not
	// reaching 4.
	//
	// Six now, and from an observation rather than an argument: a 192-cell binary frame at 5.5 to 5.7
	// captured pixels a cell located its geometry cleanly on every frame and failed its payload checksum on
	// 41 of 41, with the recovery engine offered all of them and rescuing none. So the real floor is above
	// 5.7, and 3 was optimistic by at least a factor of two.
	//
	// Six is a lower bound, not a measured threshold. Unlike the colour figure — which has a decode-rate
	// sweep behind it, 12 reading every frame and 5.9 reading none — nothing here establishes where binary
	// actually becomes reliable, only that it is not yet reliable at 5.7. Treat it as provisional and run
	// the sweep for binary before quoting it as measured.
	BinaryPixels = 6.0

	// ColourPixels is what a multi-level cell needs, and it is several times the binary figure for a reason
	// worth stating, because three pixels looks sufficient and is not.
	//
	// A colour cell is matched against eight palette entries whose nearest neighbours sit 86 apart in
	// weighted distance, which makes reading it a measurement rather than a decision — and the accuracy of
	// that measurement is set by how many pixels were averaged to make it. The sampler averages a window a
	// quarter of a cell wide, so pixels per cell decides the noise directly: at 5.9 that window is nine
	// pixels, and the colour landed a mean weighted distance of 53 from its palette entry against a margin
	// of 86. Every one of those frames located its geometry perfectly and failed its payload CRC.
	//
	// Measured end to end: 12 pixels a cell decoded every frame, 8.5 about one in a hundred, 5.9 never. Ten
	// puts the sampling window at twenty-five pixels and cuts that noise by two thirds.
	ColourPixels = 10.0

	// MarginalFraction is where "reliable" ends and "occasionally" begins, as a fraction of Required.
	//
	// The distinction exists because collapsing it was measurably misleading. Below Required a frame does
	// not stop decoding, it decodes *rarely*: the figures this model is built from are 12 pixels a cell
	// decoding every frame, 8.5 decoding about one in a hundred, and 5.9 never decoding at all. A geometry
	// at 8.2 was reported as "cannot be read by this camera" and then acknowledged 24 chunks of 74, which
	// is not what "cannot" means — and an operator who is told a thing is impossible and then watches it
	// half-work stops trusting the rest of the advice.
	//
	// Eight tenths puts the colour boundary at 8 pixels a cell, which matches where the measurements turn
	// from "one in a hundred" toward "never".
	MarginalFraction = 0.8

	// MaxUsefulPixels is where being closer starts costing more than it gains, and it is the
	// counter-intuitive end of the same measurement.
	//
	// More pixels per cell is better right up until the camera resolves the panel's own pixel and subpixel
	// structure rather than the frame drawn on it, at which point a cell stops being one colour and becomes
	// a stripe pattern. Measured back to back on the same rig and encoding: at 11.2 px per cell six frames
	// of six decoded; at 13.6, closer and with more apparent detail, none of six did — while the decoder's
	// contrast figure fell from 176 to 160 as the camera moved *closer*, which is the tell. Contrast rising
	// as you approach is normal; contrast falling means the resolution is being spent on the panel.
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

	// Required is what the encoding needs.
	Required float64

	// Readable reports whether ModulePixels reaches Required — decoding reliably, not occasionally.
	Readable bool

	// Marginal reports a geometry below Required but not hopeless: it will decode a small percentage of
	// frames, so a transfer may crawl to completion on a short file and will time out on a long one.
	//
	// Kept apart from Readable because the two need opposite words. "Cannot be read" is wrong here, and
	// saying it once costs the credibility of every other message.
	Marginal bool

	// Hopeless is below even the marginal band: no frames at all, in the measurements this rests on.
	Hopeless bool

	// MaxGrid is the largest grid this capture could read at this bit depth, as a cell count because that
	// is what an operator chooses from.
	MaxGrid int

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
	// The short side, and only the short side. A frame is square, so the larger dimension cannot be used
	// however the camera is held: a square inscribed in a 1920x1080 picture is 1080 across, and the same
	// square in a 1080x1920 picture is also 1080 across. An earlier version of this reported what the long
	// side would give and advised turning the camera, which is advice that does nothing — the number does
	// not change, and an operator who follows it and sees no difference has been sent on an errand.
	cells := float64(gridWidth + 2*quietZone)
	short := math.Min(float64(captureW), float64(captureH))

	a := Assessment{Required: Required(bitDepth)}
	if cells <= 0 || short <= 0 {
		return a
	}

	a.ModulePixels = short / cells
	a.Readable = a.ModulePixels >= a.Required
	a.Marginal = !a.Readable && a.ModulePixels >= a.Required*MarginalFraction
	a.Hopeless = a.ModulePixels < a.Required*MarginalFraction

	a.MaxGrid = int(short/a.Required) - 2*quietZone
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

	// Two different sentences, because they describe two different situations and the wrong one is worse
	// than no advice. A marginal geometry does decode — a few frames in a hundred — and telling someone it
	// cannot be read, when they can watch chunks arriving, teaches them to ignore the message.
	var opening string
	if a.Marginal {
		opening = fmt.Sprintf(
			"A %d-cell grid is marginal on this camera: cells reach about %.1f pixels where this encoding "+
				"wants %.0f, so a few frames in a hundred will read and the rest will not. A short file may "+
				"crawl to the end; a long one will exhaust its retransmissions first.",
			gridWidth, a.ModulePixels, a.Required)
	} else {
		opening = fmt.Sprintf(
			"A %d-cell grid cannot be read by this camera: cells reach only %.1f pixels even with the frame "+
				"filling the view, against the %.0f this encoding needs, which is below the point where any "+
				"frames decode at all.",
			gridWidth, a.ModulePixels, a.Required)
	}

	opening += " A frame is square, so its width is bounded by the short side of the picture — moving " +
		"closer cannot change the figure."

	switch {
	case a.BinaryWouldWork && bitDepth > 1:
		return opening + fmt.Sprintf(
			" At one bit a cell this grid would read, since binary needs about %.0f pixels rather than "+
				"%.0f — the same grid, a third of the payload. Otherwise use %d cells or fewer.",
			BinaryPixels, a.Required, max(a.MaxGrid, 0))
	default:
		return opening + fmt.Sprintf(
			" Use %d cells or fewer, or raise the camera's capture resolution — those are the only two "+
				"things that move this number.", max(a.MaxGrid, 0))
	}
}

// PostRate is how many photographs a second the capture side offers, and it is the budget every
// frame rate below is spent out of.
//
// Ten, because that is what the browser camera posts. A direct camera source offers more, but this
// figure only has to be right about the ratio it produces: what matters is shots per displayed
// frame, and a source that supplies twice as many simply gets twice as many per frame.
const PostRate = 10.0

// Shots per displayed frame at each level of difficulty. See ShotsNeeded for why these numbers.
const (
	// ShotsComfortable is the floor, and it is two rather than one for a reason that has nothing to do
	// with how readable the geometry is. At one shot a frame, a single hand tremor, a rolling-shutter
	// tear, or one badly timed refresh loses that frame outright and it must be retransmitted. Two is
	// the smallest number at which a frame survives one bad photograph.
	ShotsComfortable = 2

	// ShotsReadable is for a geometry that clears its floor without much to spare.
	ShotsReadable = 3

	// ShotsMarginal is for one below its floor. This is where combining stops being insurance and
	// becomes the thing that makes the transfer work at all: measured on real captures at 6.95 pixels
	// a cell, combining shots read frames that no single photograph could.
	ShotsMarginal = 5

	// ShotsDesperate is for a geometry well below its floor. It will still mostly fail — no number of
	// shots recovers cells the optics never separated — but it is the most that can be tried.
	ShotsDesperate = 10
)

// ShotsNeeded is how many photographs of one displayed frame this geometry needs before it is worth
// moving on to the next.
//
// The tighter a geometry sits against what its encoding needs, the more photographs it takes to read
// one frame reliably, because the shortfall shows up as cells read wrongly and independent shots
// disagree about which ones. Combining them is what recovers the frame; how many it takes depends on
// how far short the geometry falls.
func ShotsNeeded(a Assessment) int {
	if a.Required <= 0 {
		return ShotsComfortable
	}
	switch ratio := a.ModulePixels / a.Required; {
	case ratio >= 1.5:
		return ShotsComfortable
	case ratio >= 1.0:
		return ShotsReadable
	case ratio >= 0.8:
		return ShotsMarginal
	default:
		return ShotsDesperate
	}
}

// DisplayFPS is how fast a frame of this geometry should be shown.
//
// It is a division and nothing cleverer: the capture side offers a fixed number of photographs a
// second, each displayed frame needs several of them, so the rate a frame can be replaced is the one
// divided by the other. Showing frames faster than that does not transfer more — it transfers the
// same frames with fewer photographs each, and fewer photographs is precisely what makes a frame
// fail.
//
// This is why the sender's old default of ten was close to unusable at any real geometry. Ten frames
// a second against ten photographs a second is one photograph per frame, so every frame rested on a
// single photograph being good, and one shake lost it.
//
// The counter-intuitive part is that a *denser* grid wants a *slower* rate, which sounds like it
// would transfer less. It does not: a denser frame carries proportionally more, so fewer frames a
// second move the same bytes. A 384-cell colour frame carries eleven times what an 80-cell one does,
// so four a second is a faster transfer than thirty of the smaller — and four a second leaves room
// for the shots it needs.
func DisplayFPS(gridWidth, quietZone int, bitDepth uint8, captureW, captureH int) float64 {
	return PostRate / float64(ShotsNeeded(Assess(gridWidth, quietZone, bitDepth, captureW, captureH)))
}
