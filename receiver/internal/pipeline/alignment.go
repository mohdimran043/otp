package pipeline

import (
	"fmt"
	"image"
	"time"

	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/readable"
)

// Alignment is what the last captured frame says about how the camera is pointed.
//
// It exists because aiming a camera at a display is otherwise done blind. The operator holds a
// phone, the receiver reports a decode rate some seconds later, and nothing connects the two: no
// part of "0 decoded of 478" says whether to step forward, step back, or turn slightly. Every
// figure here is one the decoder already computes on its way to reading a frame, so reporting them
// costs nothing beyond keeping the most recent one.
//
// It describes the last frame only. A camera being aimed is moving, and an average over the last
// minute would describe where it used to be pointed.
type Alignment struct {
	// Locked is whether the four fiducials were found and a geometry fitted. Everything below is
	// meaningless when it is false, because all of it is measured from that geometry.
	Locked bool `json:"locked"`

	// Decoded is whether the frame went on to read as a valid frame. Locked without Decoded is the
	// interesting middle state: aimed well enough to find the grid, not well enough to read it.
	Decoded bool `json:"decoded"`

	// Fill is how much of the view the grid spans, as a fraction of the tighter image dimension.
	// This is the distance signal: too small and the cells fall below what the sensor resolves,
	// too large and the fiducials leave the frame entirely.
	Fill float64 `json:"fill"`

	// ModulePixels is the apparent width of one cell in the capture. Fill says how much of the
	// view the grid occupies; this says whether that is enough pixels to read, which depends on
	// how many cells the sender is packing into it.
	ModulePixels float64 `json:"module_pixels"`

	// RequiredModulePixels is what this frame's own encoding needs, which is several times larger
	// for a colour payload than a binary one. Reported next to the measurement rather than left
	// implicit, so someone aiming a camera can see the target they are aiming at.
	RequiredModulePixels float64 `json:"required_module_pixels"`

	// MaxModulePixels is the other end of that target, and zero when the encoding has no upper
	// bound worth reporting. A colour payload has one: past it the camera resolves the panel's own
	// pixel structure and a cell stops being one colour. See maxColourModulePixels.
	MaxModulePixels float64 `json:"max_module_pixels"`

	// AchievableModulePixels is the most this capture could resolve at this geometry, with the frame
	// filling the short side. When it is below RequiredModulePixels no amount of aiming will do, and the
	// fault is the sender's grid rather than the operator's hands.
	AchievableModulePixels float64 `json:"achievable_module_pixels"`

	// MaxGridForCapture is the largest grid this capture could resolve at this encoding.
	MaxGridForCapture int `json:"max_grid_for_capture"`

	// GeometryMarginal is a grid a little below what the encoding wants: it decodes a few frames in a
	// hundred rather than none. Distinguished from hopeless because the page must not paint a geometry red
	// and call it unreadable while the operator watches chunks being acknowledged.
	GeometryMarginal bool `json:"geometry_marginal"`

	// Perspective is 0 for a square-on view and rises as the camera moves off-axis.
	Perspective float64 `json:"perspective"`

	// FinderScore, TimingScore and Contrast are the decoder's own confidence figures, passed
	// through so the page can show what it is grading on rather than only the verdict.
	FinderScore float64 `json:"finder_score"`
	TimingScore float64 `json:"timing_score"`
	Contrast    float64 `json:"contrast"`

	// Corners are the fiducial centres normalised to 0..1 of the frame, in the order top-left,
	// top-right, bottom-left, bottom-right. Normalised rather than in pixels because the page
	// draws them over a preview element whose size has nothing to do with the capture's.
	Corners [4][2]float64 `json:"corners"`

	// Status is the single machine-readable verdict the page keys its colours off.
	Status AlignmentStatus `json:"status"`

	// Advice is that verdict as an instruction. The receiver phrases it rather than the page, so
	// that the thresholds and the words describing them cannot drift apart.
	Advice string `json:"advice"`

	// At is when this frame was measured, so a page can tell a live reading from a stale one after
	// the camera has stopped posting.
	At time.Time `json:"at"`
}

// AlignmentStatus is the verdict on one frame.
type AlignmentStatus string

const (
	// StatusSearching means no grid was found. It is deliberately not called an error: it is also
	// what a camera pointed at the wall reports, and what every camera reports before it is aimed.
	StatusSearching AlignmentStatus = "searching"

	// StatusTooFar and StatusTooClose are the two distance faults.
	StatusTooFar   AlignmentStatus = "too_far"
	StatusTooClose AlignmentStatus = "too_close"

	// StatusOffAxis is a grid found at too steep an angle.
	StatusOffAxis AlignmentStatus = "off_axis"

	// StatusTooDense is a grid the camera cannot resolve at any distance.
	//
	// It exists because the advice for it is the opposite of the advice for too_far, and the two are
	// indistinguishable from the numbers alone: both are "cells are smaller than this encoding needs".
	// The difference is whether moving would help. A square frame's width is bounded by the capture's
	// short side, so once it fills the view the pixels per cell are fixed by arithmetic — grid plus quiet
	// zone against that short side — and no amount of walking forward changes it.
	//
	// Telling someone to move closer when they are already at 86% of the view is worse than saying
	// nothing: they will keep trying, the numbers will not move, and the fault is in the sender's grid.
	StatusTooDense AlignmentStatus = "too_dense"

	// StatusMarginal is aimed well enough to find the grid and not well enough to read it. This is
	// the state that most needs saying out loud, because it looks identical to failure from the
	// outside and is one small movement away from working.
	StatusMarginal AlignmentStatus = "marginal"

	// StatusGood is a frame that decoded.
	StatusGood AlignmentStatus = "good"
)

// The thresholds below are the aiming envelope. They are deliberately tighter than the decoder's
// own limits: this is advice given while someone is still moving the camera, and guiding them to
// the middle of the working range is more useful than telling them they are just inside its edge.
const (
	// minFill is where the grid is small enough in the view that cells start to be lost.
	minFill = 0.35

	// maxFill is where it is close enough that a small hand movement takes a fiducial out of frame.
	maxFill = 0.92

	// maxPerspective is how far off-square is still comfortable. Perspective runs 0..1 and the
	// decoder copes well past this; the point of stopping here is that squaring up is usually the
	// easiest correction to make and it buys margin against everything else.
	maxPerspective = 0.20
)

// measureAlignment turns one frame's decode result into aiming advice.
//
// The image is needed as well as the geometry because Fill is a ratio against the frame, and the
// geometry alone does not know how large a frame it was found in.
func measureAlignment(img image.Image, g *protocol.Geometry, decoded bool) Alignment {
	a := Alignment{At: time.Now()}

	if g == nil {
		a.Status = StatusSearching
		a.Advice = "Looking for the frame. Point the camera at the display and let the whole grid into view."
		return a
	}

	bounds := img.Bounds()
	w, h := float64(bounds.Dx()), float64(bounds.Dy())
	if w <= 0 || h <= 0 {
		a.Status = StatusSearching
		a.Advice = "Looking for the frame."
		return a
	}

	a.Locked = true
	a.Decoded = decoded
	a.ModulePixels = g.ModuleSize
	a.Perspective = g.Perspective()
	a.FinderScore, a.TimingScore, a.Contrast = g.FinderScore, g.TimingScore, g.Contrast

	minX, minY := g.Corners[0].X, g.Corners[0].Y
	maxX, maxY := minX, minY
	for i, c := range g.Corners {
		a.Corners[i] = [2]float64{
			clampUnit((c.X - float64(bounds.Min.X)) / w),
			clampUnit((c.Y - float64(bounds.Min.Y)) / h),
		}
		minX, maxX = min(minX, c.X), max(maxX, c.X)
		minY, maxY = min(minY, c.Y), max(maxY, c.Y)
	}

	// Against the tighter dimension, because that is the one the grid runs out of room in first.
	// A square grid in a portrait phone frame is bounded by the width long before the height.
	a.Fill = max((maxX-minX)/w, (maxY-minY)/h)

	// What this frame's own encoding needs, which is several times the binary figure when the
	// payload is in colour. Reported so the page can show the target alongside the measurement.
	// From the shared model, not a local constant. Two copies of "how many pixels does a cell need" is
	// exactly the drift this was consolidated to prevent, and it drifted anyway: the receiver's own
	// constant said 3 for binary while the sender's check said 4, so an operator could be told a geometry
	// was fine by one and refused by the other for the same frame.
	a.RequiredModulePixels = readable.Required(g.Header.BitDepth)
	if g.Header.BitDepth > 1 {
		a.MaxModulePixels = readable.MaxUsefulPixels
	}

	// What this capture can do at this geometry, from the shared model both sides use. Computed here
	// rather than inline so the sender's pre-flight check and this aiming display cannot disagree about
	// whether a geometry is possible — they were separate arithmetic once and that is how "move closer"
	// came to be shown at 86% of the view filled.
	cap := readable.Assess(g.Layout.GridWidth, g.Layout.QuietZone, g.Header.BitDepth,
		bounds.Dx(), bounds.Dy())
	a.AchievableModulePixels = cap.ModulePixels
	a.GeometryMarginal = cap.Marginal
	a.MaxGridForCapture = cap.MaxGrid

	switch {
	case a.Fill > maxFill:
		a.Status = StatusTooClose
		a.Advice = "Move back a little — the frame is filling the view and a corner marker is about to be cut off."
	case a.MaxModulePixels > 0 && a.ModulePixels > a.MaxModulePixels:
		a.Status = StatusTooClose
		a.Advice = fmt.Sprintf(
			"Move back a little — cells are %.1f pixels across and past about %.0f the camera starts "+
				"resolving the screen's own pixels rather than the frame on it, which stops each cell "+
				"being one colour. Closer is not better here.",
			a.ModulePixels, a.MaxModulePixels)
	// Tested before too_far, because when both apply this is the true fault and the other is advice
	// that cannot be followed.
	case a.AchievableModulePixels > 0 && !cap.Readable:
		a.Status = StatusTooDense
		a.Advice = cap.Explain(g.Layout.GridWidth, g.Header.BitDepth)
	case a.Fill < minFill || a.ModulePixels < a.RequiredModulePixels:
		a.Status = StatusTooFar
		a.Advice = fmt.Sprintf(
			"Move closer — cells are %.1f pixels across and this frame needs about %.0f. "+
				"Colour needs several times the size plain black and white does, because each cell is "+
				"matched against eight shades rather than put on one side of a threshold.",
			a.ModulePixels, a.RequiredModulePixels)
	case a.Perspective > maxPerspective:
		a.Status = StatusOffAxis
		a.Advice = "Square up to the screen — you are looking at it from too steep an angle."
	case decoded:
		a.Status = StatusGood
		a.Advice = "Holding well. Keep it here."
	default:
		a.Status = StatusMarginal
		a.Advice = "Almost — the grid is found but not readable. Hold steady, and try tapping the screen to focus."
	}
	return a
}

func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
