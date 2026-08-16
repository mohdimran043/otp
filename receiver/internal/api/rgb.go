package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// The production camera, and whether the geometry it is pointed at can be read.
//
// This receiver does not open a GigE Vision or CoaXPress camera. Those are reached through their vendors'
// SDKs, and a frame from one arrives here exactly as a browser's does — a POST to /api/v1/capture/frames
// from a small grabber process that owns the SDK. Keeping it that way is deliberate: it is the difference
// between a receiver that supports three cameras and one that supports any camera somebody can write forty
// lines of Python against, and it keeps a vendor runtime out of this image.
//
// What this endpoint is for is the part the receiver *can* answer, and the part that decides whether an
// installation works: given a sensor, a panel and how many frames the sender tiles onto it, how many camera
// pixels land on one cell. That question is settled before the hardware is bought and is nearly impossible
// to eyeball — a five-megapixel camera photographing a 4K panel resolves well under half a sensor pixel per
// display pixel once the aspect mismatch and the framing margin are paid, and a four-lane tiling divides
// what is left again.

const maxRGBRequestBytes = 4096

// rgbView is the configured camera and what it can resolve.
type rgbView struct {
	Enabled        bool    `json:"enabled"`
	Model          string  `json:"model"`
	SensorWidth    int     `json:"sensor_width"`
	SensorHeight   int     `json:"sensor_height"`
	FPS            float64 `json:"fps"`
	ExposureMicros float64 `json:"exposure_micros"`
	PixelFormat    string  `json:"pixel_format"`
	Interface      string  `json:"interface"`
	TriggerMode    string  `json:"trigger_mode"`
	PanelWidth     int     `json:"panel_width"`
	PanelHeight    int     `json:"panel_height"`
	PanelFill      float64 `json:"panel_fill"`

	// Lanes is what this receiver is currently looking for, echoed because the feasibility below depends on
	// it and an answer whose inputs are not visible beside it invites arguing with the wrong number.
	Lanes int `json:"lanes"`

	// Feasibility is absent rather than zeroed when the camera has not been described. "Not configured" and
	// "resolves nothing" are different facts, and a planner reporting the second for the first would be
	// telling an operator their hardware cannot work when they have simply not entered it yet.
	Feasibility *config.RGBFeasibility `json:"feasibility,omitempty"`

	// Note says what this endpoint is and is not, because the natural reading of "configure a camera" is
	// that it opens one.
	Note string `json:"note"`
}

// rgbRequest is a partial update: absent fields are left as they are.
type rgbRequest struct {
	Enabled        *bool    `json:"enabled,omitempty"`
	Model          *string  `json:"model,omitempty"`
	SensorWidth    *int     `json:"sensor_width,omitempty"`
	SensorHeight   *int     `json:"sensor_height,omitempty"`
	FPS            *float64 `json:"fps,omitempty"`
	ExposureMicros *float64 `json:"exposure_micros,omitempty"`
	PixelFormat    *string  `json:"pixel_format,omitempty"`
	Interface      *string  `json:"interface,omitempty"`
	TriggerMode    *string  `json:"trigger_mode,omitempty"`
	PanelWidth     *int     `json:"panel_width,omitempty"`
	PanelHeight    *int     `json:"panel_height,omitempty"`
	PanelFill      *float64 `json:"panel_fill,omitempty"`

	// Grid, Quiet and BitDepth ask "what would this camera do with that geometry" without changing
	// anything. They are the planner's inputs and are never stored: an operator comparing a 64 grid against
	// a 96 should not have to change the deployment to find out.
	Grid     *int   `json:"grid,omitempty"`
	Quiet    *int   `json:"quiet_zone,omitempty"`
	BitDepth *uint8 `json:"bit_depth,omitempty"`
	Lanes    *int   `json:"lanes,omitempty"`
}

const rgbNote = "These settings describe the production camera; they do not drive it. A GigE Vision or " +
	"CoaXPress camera is opened by a grabber process using its vendor's SDK, which posts frames to " +
	"/api/v1/capture/frames exactly as the browser camera does. What is computed here is whether the " +
	"geometry the sender is displaying can be resolved by the camera described."

// getRGBCamera reports the configured production camera and what it can resolve.
//
// The query string carries the planner's inputs, so a page can ask about a geometry without saving one:
// ?grid=96&bit_depth=3&lanes=4.
func (s *Server) getRGBCamera(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Current()

	grid := queryInt(r, "grid", 80)
	quiet := queryInt(r, "quiet_zone", 2)
	depth := uint8(queryInt(r, "bit_depth", 3))
	lanes := queryInt(r, "lanes", cfg.Capture.Lanes)

	s.respond(w, http.StatusOK, rgbViewOf(cfg, grid, quiet, depth, lanes))
}

// setRGBCamera records what the production camera is.
//
// A partial update, and deliberately so: an operator correcting the exposure should not have to resend the
// sensor dimensions, and a page that sent every field would overwrite anything set from the environment
// that it had not been taught about.
func (s *Server) setRGBCamera(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRGBRequestBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "could not read the request", err)
		return
	}
	if len(body) > maxRGBRequestBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, "the request is too large to be a camera description", nil)
		return
	}

	var request rgbRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		s.fail(w, http.StatusBadRequest, "the request is not a camera description", err)
		return
	}

	next := s.cfg.Current().Capture.RGB
	apply(request.Enabled, &next.Enabled)
	apply(request.Model, &next.Model)
	apply(request.SensorWidth, &next.SensorWidth)
	apply(request.SensorHeight, &next.SensorHeight)
	apply(request.FPS, &next.FPS)
	apply(request.ExposureMicros, &next.ExposureMicros)
	apply(request.PixelFormat, &next.PixelFormat)
	apply(request.Interface, &next.Interface)
	apply(request.TriggerMode, &next.TriggerMode)
	apply(request.PanelWidth, &next.PanelWidth)
	apply(request.PanelHeight, &next.PanelHeight)
	apply(request.PanelFill, &next.PanelFill)

	if problem := validateRGB(next); problem != "" {
		s.fail(w, http.StatusBadRequest, problem, nil)
		return
	}

	applied := s.cfg.SetRGBCamera(next)

	grid := 80
	quiet := 2
	depth := uint8(3)
	lanes := applied.Capture.Lanes
	apply(request.Grid, &grid)
	apply(request.Quiet, &quiet)
	apply(request.BitDepth, &depth)
	apply(request.Lanes, &lanes)

	s.log.Info("production camera described",
		zap.Bool("enabled", applied.Capture.RGB.Enabled),
		zap.String("model", applied.Capture.RGB.Model),
		zap.Int("sensor_width", applied.Capture.RGB.SensorWidth),
		zap.Int("sensor_height", applied.Capture.RGB.SensorHeight))

	s.respond(w, http.StatusOK, rgbViewOf(applied, grid, quiet, depth, lanes))
}

// rgbViewOf builds the response, feasibility included when there is enough to compute one.
func rgbViewOf(cfg config.Config, grid, quiet int, depth uint8, lanes int) rgbView {
	c := cfg.Capture.RGB
	view := rgbView{
		Enabled:        c.Enabled,
		Model:          c.Model,
		SensorWidth:    c.SensorWidth,
		SensorHeight:   c.SensorHeight,
		FPS:            c.FPS,
		ExposureMicros: c.ExposureMicros,
		PixelFormat:    c.PixelFormat,
		Interface:      c.Interface,
		TriggerMode:    c.TriggerMode,
		PanelWidth:     c.PanelWidth,
		PanelHeight:    c.PanelHeight,
		PanelFill:      c.PanelFill,
		Lanes:          lanes,
		Note:           rgbNote,
	}
	if f, ok := c.Feasibility(grid, quiet, depth, lanes); ok {
		view.Feasibility = &f
	}
	return view
}

// validateRGB refuses a description that cannot be true, rather than computing on it.
func validateRGB(c config.RGBCamera) string {
	switch {
	case c.SensorWidth < 0 || c.SensorHeight < 0:
		return "a sensor cannot have a negative dimension"
	case c.PanelWidth < 0 || c.PanelHeight < 0:
		return "a panel cannot have a negative dimension"
	case c.FPS < 0:
		return "a frame rate cannot be negative"
	case c.ExposureMicros < 0:
		return "an exposure cannot be negative"
	case c.PanelFill < 0 || c.PanelFill > 1:
		return "panel_fill is the fraction of the frame the panel occupies, so it must be between 0 and 1"
	}
	return ""
}

// apply copies a present pointer over a destination, leaving it alone when absent.
func apply[T any](from *T, to *T) {
	if from != nil {
		*to = *from
	}
}
