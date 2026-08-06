package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/encoding"
)

// The display settings: how fast the frames go out and how much each one carries.
//
// These are the two knobs that set the transfer rate, and they are not symmetrical. The frame rate can
// change at any moment — it is a delay between two writes, and nothing already rendered depends on it. The
// geometry cannot: the grid and cell size are written into every frame header, and the chunk size was
// derived from the encoder's capacity at that grid. Changing it mid-transmission would produce frames the
// receiver reassembles into the wrong file.
//
// So the frame rate applies immediately, and geometry is refused while anything is in flight. That
// asymmetry is the whole design of this file.

// settingsView is the display configuration as the settings page sees it.
type settingsView struct {
	FPS        float64 `json:"fps"`
	Brightness float64 `json:"brightness"`
	Gamma      float64 `json:"gamma"`
	WindowSize int     `json:"window_size"`
	KeepAlive  bool    `json:"keep_alive"`
	Sink       string  `json:"sink"`

	GridWidth  int    `json:"grid_width"`
	GridHeight int    `json:"grid_height"`
	CellPixels int    `json:"cell_pixels"`
	QuietZone  int    `json:"quiet_zone"`
	Encoder    string `json:"encoder"`
	BitDepth   int    `json:"bit_depth"`

	// ImageWidth and ImageHeight are what the geometry actually comes to in pixels, which is the number
	// that has to fit on the panel. An operator choosing 256×256 cells at 8 px is really choosing 2080
	// pixels, and that is the figure to compare against a display.
	ImageWidth  int `json:"image_width_px"`
	ImageHeight int `json:"image_height_px"`

	// BytesPerFrame is the payload one frame carries at this geometry and encoding, and Rate is that
	// times the frame rate. Together they are the answer to "how fast will this go", computed from the
	// encoder rather than estimated.
	BytesPerFrame  int     `json:"bytes_per_frame"`
	BytesPerSecond float64 `json:"bytes_per_second"`

	// Transmitting is how many transfers are in flight. Geometry cannot change while it is above zero,
	// and the page needs to know so it can say why rather than only refusing.
	Transmitting int `json:"transmitting"`
}

// settingsRequest is a change to the display.
//
// Every field is a pointer so that "not mentioned" is distinguishable from "set to zero". A form that sent
// its whole state back would otherwise reset a field it never showed — and with a frame rate, zero means
// "never display anything", which is a way to stop a transfer without appearing to.
type settingsRequest struct {
	FPS        *float64 `json:"fps,omitempty"`
	Brightness *float64 `json:"brightness,omitempty"`
	Gamma      *float64 `json:"gamma,omitempty"`
	WindowSize *int     `json:"window_size,omitempty"`

	GridWidth  *int    `json:"grid_width,omitempty"`
	GridHeight *int    `json:"grid_height,omitempty"`
	CellPixels *int    `json:"cell_pixels,omitempty"`
	QuietZone  *int    `json:"quiet_zone,omitempty"`
	Encoder    *string `json:"encoder,omitempty"`
	BitDepth   *int    `json:"bit_depth,omitempty"`
}

// touchesGeometry reports whether this change alters what a frame looks like.
func (r settingsRequest) touchesGeometry() bool {
	return r.GridWidth != nil || r.GridHeight != nil || r.CellPixels != nil ||
		r.QuietZone != nil || r.Encoder != nil || r.BitDepth != nil
}

// maxFPS is the highest frame rate that may be configured.
//
// Above any panel anyone will attach, deliberately: the real ceiling is the display's refresh rate and the
// receiver's decode throughput, neither of which this process can measure. The settings page measures the
// refresh rate in the browser, where it can be measured, and warns. This constant only stops a typo — 3000
// instead of 30 — from becoming a display loop that never sleeps.
const maxFPS = 480

// maxImagePixels bounds the rendered frame's edge.
//
// 4320 is the height of an 8K panel. A frame larger than the display cannot be shown without scaling, and
// scaling a cell grid is what the display page goes to some trouble to avoid — so a geometry that cannot
// fit any real panel is a mistake rather than an ambition.
const maxImagePixels = 4320

// getSettings reports the display configuration and what it comes to.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Current()

	view := settingsView{
		FPS:        cfg.Display.FPS,
		Brightness: cfg.Display.Brightness,
		Gamma:      cfg.Display.Gamma,
		WindowSize: cfg.Display.WindowSize,
		KeepAlive:  cfg.Display.KeepAlive,
		Sink:       cfg.Display.Sink,
		GridWidth:  cfg.Optical.GridWidth,
		GridHeight: cfg.Optical.GridHeight,
		CellPixels: cfg.Optical.CellPixels,
		QuietZone:  cfg.Optical.QuietZone,
		Encoder:    cfg.Optical.Encoder,
		BitDepth:   cfg.Optical.BitDepth,
	}

	if layout, err := cfg.Layout(); err == nil {
		view.ImageWidth, view.ImageHeight = layout.ImageWidth(), layout.ImageHeight()
		if enc, err := encoding.ByName(cfg.Optical.Encoder); err == nil {
			if capacity, err := enc.EstimateCapacity(layout, uint8(cfg.Optical.BitDepth)); err == nil {
				view.BytesPerFrame = capacity.PayloadBytes
				view.BytesPerSecond = float64(capacity.PayloadBytes) * cfg.Display.FPS
			}
		}
	}

	if active, err := s.store.Transmissions.CountActive(r.Context()); err == nil {
		view.Transmitting = active
	}

	s.respond(w, http.StatusOK, view)
}

// maxSettingsRequestBytes bounds the body. It is a dozen numbers.
const maxSettingsRequestBytes = 4 << 10

// updateSettings changes the display.
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSettingsRequestBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "could not read the request", err)
		return
	}
	if len(body) > maxSettingsRequestBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, "the request is too large to be a settings change", nil)
		return
	}

	var request settingsRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		s.fail(w, http.StatusBadRequest, "the request is not a settings change", err)
		return
	}

	// Geometry is refused while anything is transmitting, and the reason is not caution: the grid and cell
	// size are written into every frame header, and the chunk size was derived from the encoder's capacity
	// at that grid. A transfer that changed geometry halfway would have its remaining chunks rendered to a
	// different shape than its manifest declared, and the receiver would reassemble the wrong file.
	if request.touchesGeometry() {
		active, err := s.store.Transmissions.CountActive(r.Context())
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "could not check for transfers in flight", err)
			return
		}
		if active > 0 {
			s.fail(w, http.StatusConflict, fmt.Sprintf(
				"the frame geometry cannot change while %d transfer(s) are in flight: it is written into "+
					"every frame header and the chunk size is derived from it. The frame rate can be "+
					"changed at any time.", active), nil)
			return
		}
	}

	next := s.cfg.Current()
	if request.FPS != nil {
		if *request.FPS <= 0 || *request.FPS > maxFPS {
			s.fail(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("the frame rate must be above 0 and at most %d", maxFPS), nil)
			return
		}
		next.Display.FPS = *request.FPS
	}
	if request.Brightness != nil {
		next.Display.Brightness = *request.Brightness
	}
	if request.Gamma != nil {
		next.Display.Gamma = *request.Gamma
	}
	if request.WindowSize != nil {
		next.Display.WindowSize = *request.WindowSize
	}
	if request.GridWidth != nil {
		next.Optical.GridWidth = *request.GridWidth
	}
	if request.GridHeight != nil {
		next.Optical.GridHeight = *request.GridHeight
	}
	if request.CellPixels != nil {
		next.Optical.CellPixels = *request.CellPixels
	}
	if request.QuietZone != nil {
		next.Optical.QuietZone = *request.QuietZone
	}
	if request.Encoder != nil {
		next.Optical.Encoder = *request.Encoder
	}
	if request.BitDepth != nil {
		next.Optical.BitDepth = *request.BitDepth
	}

	// Validated as a whole configuration rather than field by field, because the fields constrain each
	// other: an encoder has bit depths it supports, a grid has to be large enough for the encoder to carry
	// anything, and the chunk size falls out of both.
	if err := next.Validate(); err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err.Error(), err)
		return
	}

	layout, err := next.Layout()
	if err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err.Error(), err)
		return
	}
	if layout.ImageWidth() > maxImagePixels || layout.ImageHeight() > maxImagePixels {
		s.fail(w, http.StatusUnprocessableEntity, fmt.Sprintf(
			"that geometry renders a %d×%d pixel frame, which is larger than any panel: reduce the grid or "+
				"the cell size", layout.ImageWidth(), layout.ImageHeight()), nil)
		return
	}

	applied, err := s.cfg.Apply(next)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "the settings could not be applied", err)
		return
	}
	s.log.Info("display settings changed",
		zap.Float64("fps", applied.Display.FPS),
		zap.Int("grid_width", applied.Optical.GridWidth),
		zap.Int("grid_height", applied.Optical.GridHeight),
		zap.Int("cell_pixels", applied.Optical.CellPixels),
		zap.String("encoder", applied.Optical.Encoder))

	s.getSettings(w, r)
}
