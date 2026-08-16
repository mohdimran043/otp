package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
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
//
// The sink sits to one side of that split: it is guarded the same way geometry is, but for neither
// reason above. It is refused while transmitting because moving the channel mid-transfer strands the
// receiver watching the old one, and even once it is written into the running configuration it does not
// take hold immediately — the process opens its sink once at startup and nothing here re-opens it, so
// the effect is deferred to the next restart. That is the same limitation the configuration file itself
// already has for this field, not a new one this endpoint introduces.

// settingsView is the display configuration as the settings page sees it.
type settingsView struct {
	FPS        float64 `json:"fps"`
	Brightness float64 `json:"brightness"`
	Gamma      float64 `json:"gamma"`
	WindowSize int     `json:"window_size"`
	KeepAlive  bool    `json:"keep_alive"`
	Sink       string  `json:"sink"`

	Lanes      int    `json:"lanes"`
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

	Lanes      *int    `json:"lanes,omitempty"`
	GridWidth  *int    `json:"grid_width,omitempty"`
	GridHeight *int    `json:"grid_height,omitempty"`
	CellPixels *int    `json:"cell_pixels,omitempty"`
	QuietZone  *int    `json:"quiet_zone,omitempty"`
	Encoder    *string `json:"encoder,omitempty"`
	BitDepth   *int    `json:"bit_depth,omitempty"`

	// Sink chooses the display channel: "file" writes into the shared directory the receiver's file
	// camera reads, "none" writes nothing so the receiver can watch the physical display with a real
	// camera instead. It is not reloadable — the sink is opened once at startup — so a change here
	// takes effect on the next restart, the same as the frame geometry below.
	Sink *string `json:"sink,omitempty"`
}

// touchesGeometry reports whether this change alters what a frame looks like.
//
// Lanes is deliberately absent. It decides how many frames are shown at once and where each sits,
// not what any of them contains: every lane is an ordinary frame, rendered and checksummed exactly as
// it would be if displayed alone. So it can change while a transfer is in flight — the frames already
// rendered are equally valid one at a time or four at a time — where the fields below cannot, because
// they are written into every frame header and the chunk size is derived from them.
func (r settingsRequest) touchesGeometry() bool {
	return r.GridWidth != nil || r.GridHeight != nil || r.CellPixels != nil ||
		r.QuietZone != nil || r.Encoder != nil || r.BitDepth != nil
}

// touchesChannel reports whether this change alters where a frame goes rather than what it looks like.
func (r settingsRequest) touchesChannel() bool {
	return r.Sink != nil
}

// stored is this change as the keys to persist, carrying only the fields the request actually named.
//
// Sparse for a reason: storing all eleven fields whenever any one of them changed would pin the whole
// configuration on the first edit, and an operator who later changed the frame rate in sender.yaml would find
// it ignored because a months-old sink change had frozen everything alongside it. Keys match
// config.SettingKeys, and a test compares the two lists so they cannot drift apart.
func (r settingsRequest) stored() map[string]string {
	out := map[string]string{}
	if r.FPS != nil {
		out["fps"] = strconv.FormatFloat(*r.FPS, 'f', -1, 64)
	}
	if r.Brightness != nil {
		out["brightness"] = strconv.FormatFloat(*r.Brightness, 'f', -1, 64)
	}
	if r.Gamma != nil {
		out["gamma"] = strconv.FormatFloat(*r.Gamma, 'f', -1, 64)
	}
	if r.WindowSize != nil {
		out["window_size"] = strconv.Itoa(*r.WindowSize)
	}
	if r.Lanes != nil {
		out["lanes"] = strconv.Itoa(*r.Lanes)
	}
	if r.GridWidth != nil {
		out["grid_width"] = strconv.Itoa(*r.GridWidth)
	}
	if r.GridHeight != nil {
		out["grid_height"] = strconv.Itoa(*r.GridHeight)
	}
	if r.CellPixels != nil {
		out["cell_pixels"] = strconv.Itoa(*r.CellPixels)
	}
	if r.QuietZone != nil {
		out["quiet_zone"] = strconv.Itoa(*r.QuietZone)
	}
	if r.Encoder != nil {
		out["encoder"] = *r.Encoder
	}
	if r.BitDepth != nil {
		out["bit_depth"] = strconv.Itoa(*r.BitDepth)
	}
	if r.Sink != nil {
		out["sink"] = *r.Sink
	}
	return out
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
		Lanes:      cfg.Optical.Lanes,
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
	//
	// The sink is guarded the same way, for a different reason: swapping the channel mid-transfer would
	// move the remaining frames somewhere the receiver on the other end is not watching, which looks from
	// there exactly like the display having stopped.
	if request.touchesGeometry() || request.touchesChannel() {
		active, err := s.store.Transmissions.CountActive(r.Context())
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "could not check for transfers in flight", err)
			return
		}
		if active > 0 {
			what, why := "the frame geometry", "it is written into every frame header and the chunk size is derived from it"
			if !request.touchesGeometry() {
				what, why = "the display sink", "the remaining frames would go somewhere the receiver is not watching"
			}
			s.fail(w, http.StatusConflict, fmt.Sprintf(
				"%s cannot change while %d transfer(s) are in flight: %s. The frame rate can be "+
					"changed at any time.", what, active, why), nil)
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
	if request.Lanes != nil {
		// Validated here rather than left to the display, because a count that cannot be tiled evenly
		// would produce a ragged arrangement whose last row is half empty — and the failure would
		// appear at the next frame rather than at the click that caused it.
		if _, err := protocol.NewLaneLayout(protocol.Layout{}, *request.Lanes, 0); err != nil {
			s.fail(w, http.StatusUnprocessableEntity, err.Error(), err)
			return
		}
		next.Optical.Lanes = *request.Lanes
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

		// The depth follows the encoding when the request does not name one.
		//
		// Each encoding accepts its own set — binary one bit a cell, color8 three, color16 four, grayscale two or
		// three — and the depth used to be applied independently, so a depth left over from the previous encoding
		// was carried into the new one and validation refused the whole change with a message naming a field the
		// operator never touched: "optical.bit_depth 1 is not one the color8 encoder offers ([3])". Choosing an
		// encoding from a list should not require knowing its depths, and with settings now persisted a stale
		// depth survives a restart, so the deployment stayed wedged until someone sent both fields together.
		//
		// Only when the request is silent about depth. A depth named explicitly is the operator saying something
		// specific, and is left to validation to accept or refuse on its own terms — papering over a genuine
		// mistake by substituting something that happens to work would be worse than the error.
		// Adopted onto the request, not just onto the configuration, so that the depth is stored beside the
		// encoder that chose it. The two constrain each other, and the store is written from the fields the
		// request named — so deriving the depth here and leaving the request silent kept the pair valid in
		// memory while the row still held the previous encoding's depth. That divergence is invisible until a
		// restart, and then it is total: the startup overlay validates the stored settings as a whole and
		// drops all of them when they do not hold together, so one stale depth silently reverts every setting
		// the operator ever changed — the frame rate, the geometry, and the display sink with them.
		if request.BitDepth == nil {
			if enc, err := encoding.ByName(next.Optical.Encoder); err == nil {
				next.Optical.BitDepth = int(enc.DefaultBitDepth())
				adopted := next.Optical.BitDepth
				request.BitDepth = &adopted
			}
		}
	}
	if request.BitDepth != nil {
		next.Optical.BitDepth = *request.BitDepth
	}
	if request.Sink != nil {
		next.Display.Sink = *request.Sink
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

	// Stored before it is applied, and the request fails if the store fails.
	//
	// Applying without storing is what made the transfer-channel toggle useless: the change reached the
	// running configuration and nothing else, so the display sink — read only at startup — was discarded by
	// the very restart it needed to take effect. Ordering it this way means the response can never report a
	// change that will not come back, which is the failure an operator has no way to detect.
	if err := s.store.DisplaySettings.Set(r.Context(), request.stored()); err != nil {
		s.fail(w, http.StatusInternalServerError, "the settings could not be saved", err)
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
		zap.String("encoder", applied.Optical.Encoder),
		zap.String("sink", applied.Display.Sink))

	s.getSettings(w, r)
}
