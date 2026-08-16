package config

import (
	"fmt"

	"github.com/opticaltransport/otp/shared/readable"
)

// Answering the question a production camera is actually chosen on.
//
// Everything about whether an installation will work is decided before the hardware arrives, and it is
// decided by one number: how many camera pixels land on one of the sender's cells. A five-megapixel camera
// is not five megapixels of display — a 16:9 panel inside a 4:3-ish sensor is limited by width, the panel is
// framed with margin so a nudge does not lose a fiducial, and a tiled display divides what is left between
// its lanes. Each of those is a factor of somewhere between 0.6 and 0.5, and together they are the
// difference between a geometry that reads and one that cannot.
//
// This computes it from the same model the aiming display and the sender's pre-flight check use, so a
// planner and a running receiver cannot disagree about what is readable — which they did once, when the two
// were separate arithmetic.

// RGBFeasibility is what a configured camera can resolve.
type RGBFeasibility struct {
	// CameraPixelsPerDisplayPixel is the scale the optics impose: how much of a display pixel survives into
	// the sensor once the panel is fitted into the frame with its margin.
	CameraPixelsPerDisplayPixel float64 `json:"camera_pixels_per_display_pixel"`

	// PanelWidthInCamera and PanelHeightInCamera are the panel's size as the sensor sees it.
	PanelWidthInCamera  int `json:"panel_width_in_camera"`
	PanelHeightInCamera int `json:"panel_height_in_camera"`

	// LaneWidthInCamera and LaneHeightInCamera are one lane's share of that, which is what a decode
	// actually works from. On a tiled display this is the number that matters and the whole-panel figure is
	// the one that misleads.
	LaneWidthInCamera  int `json:"lane_width_in_camera"`
	LaneHeightInCamera int `json:"lane_height_in_camera"`

	// ModulePixels is how many camera pixels one cell resolves to at the grid asked about, and Required is
	// what the encoding needs. MaxGrid is the largest grid this camera can carry at this lane count.
	ModulePixels float64 `json:"module_pixels"`
	Required     float64 `json:"required_module_pixels"`
	MaxGrid      int     `json:"max_grid"`

	// Readable and Marginal are the verdict. Marginal means a few frames in a hundred rather than none,
	// which is deliberately not the same as failure — a geometry painted red while chunks are arriving is
	// what makes advice untrustworthy.
	Readable bool `json:"readable"`
	Marginal bool `json:"marginal"`

	// Explanation says what to do about it, in the operator's terms.
	Explanation string `json:"explanation"`
}

// laneGridFor is how a lane count is tiled, matching protocol.laneGrid.
//
// Duplicated as a small switch rather than imported, because the protocol's version is unexported and
// exporting it to serve a planner would widen the protocol's surface for a caller that only needs to know
// the shape. Wrong here costs an advisory figure; the tiling itself is decided by the protocol either way.
func laneGridFor(lanes int) (columns, rows int) {
	switch {
	case lanes >= 16:
		return 4, 4
	case lanes >= 12:
		return 4, 3
	case lanes >= 9:
		return 3, 3
	case lanes >= 8:
		return 4, 2
	case lanes >= 6:
		return 3, 2
	case lanes >= 4:
		return 2, 2
	case lanes >= 2:
		return 2, 1
	default:
		return 1, 1
	}
}

// Feasibility reports what the configured camera can resolve at a given geometry.
//
// grid is the sender's grid per lane, quiet its quiet zone, depth its bit depth, and lanes how many frames
// the sender tiles. Zero or absent camera dimensions yield a zero result rather than an invented one: a
// planner that answers before it has been told the hardware is worse than one that says nothing.
func (c RGBCamera) Feasibility(grid, quiet int, depth uint8, lanes int) (RGBFeasibility, bool) {
	if c.SensorWidth <= 0 || c.SensorHeight <= 0 || c.PanelWidth <= 0 || c.PanelHeight <= 0 {
		return RGBFeasibility{}, false
	}
	fill := c.PanelFill
	if fill <= 0 || fill > 1 {
		fill = 1
	}
	if lanes < 1 {
		lanes = 1
	}

	// Fitting the panel into the sensor, preserving aspect. Whichever axis runs out first decides the
	// scale, and for a wide panel on a squarer sensor that is the width — which is why a sensor's
	// megapixel count overstates what it will see.
	scale := min(
		float64(c.SensorWidth)/float64(c.PanelWidth),
		float64(c.SensorHeight)/float64(c.PanelHeight),
	) * fill

	panelW := int(float64(c.PanelWidth) * scale)
	panelH := int(float64(c.PanelHeight) * scale)

	columns, rows := laneGridFor(lanes)
	laneW, laneH := panelW/columns, panelH/rows

	assessed := readable.Assess(grid, quiet, depth, laneW, laneH)

	return RGBFeasibility{
		CameraPixelsPerDisplayPixel: scale,
		PanelWidthInCamera:          panelW,
		PanelHeightInCamera:         panelH,
		LaneWidthInCamera:           laneW,
		LaneHeightInCamera:          laneH,
		ModulePixels:                assessed.ModulePixels,
		Required:                    readable.Required(depth),
		MaxGrid:                     assessed.MaxGrid,
		Readable:                    assessed.Readable,
		Marginal:                    assessed.Marginal,
		Explanation:                 explainFeasibility(assessed, grid, depth, lanes),
	}, true
}

// explainFeasibility turns the verdict into the sentence an operator can act on.
func explainFeasibility(a readable.Assessment, grid int, depth uint8, lanes int) string {
	switch {
	case a.Readable && !a.Marginal:
		return fmt.Sprintf(
			"Comfortable. %d lane%s at grid %d resolves %.1f pixels a cell against the %.0f this encoding "+
				"needs, and this camera could carry a grid up to %d.",
			lanes, plural(lanes), grid, a.ModulePixels, readable.Required(depth), a.MaxGrid)
	case a.Marginal:
		return fmt.Sprintf(
			"Marginal. %.1f pixels a cell is a little under the %.0f this encoding wants, which reads a few "+
				"frames in a hundred rather than none. Drop to grid %d, halve the lanes, or use a binary "+
				"encoding — a colour cell is matched against eight shades where a binary one is put on one "+
				"side of a threshold, so it needs several times the pixels.",
			a.ModulePixels, readable.Required(depth), a.MaxGrid)
	default:
		return fmt.Sprintf(
			"Not readable. %.1f pixels a cell against the %.0f this encoding needs, and no amount of aiming "+
				"changes it — the figure is set by how many cells span the panel and how much of the sensor "+
				"the panel occupies. The largest grid this camera can carry at %d lane%s is %d.",
			a.ModulePixels, readable.Required(depth), lanes, plural(lanes), a.MaxGrid)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
