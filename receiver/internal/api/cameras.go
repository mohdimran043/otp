package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/receiver/internal/camera"
	"github.com/opticaltransport/otp/receiver/internal/pipeline"
)

// The camera surface: which capture devices this machine has, which one is in use, and how to change it.
//
// It is here rather than folded into the configuration endpoint because it is the one part of the
// receiver's settings that describes hardware rather than policy. What is attached is discovered, not
// configured, and the difference matters when something is wrong: "no camera is attached" and "the camera
// is attached but this build cannot capture from it" are different faults with different fixes, and a
// settings page that could only say "capture is not working" would send an operator looking in the wrong
// place.

// cameraView is one device as the settings page sees it.
type cameraView struct {
	camera.Device

	// Selected marks the device capture will use.
	Selected bool `json:"selected"`
}

// camerasResponse is the whole camera situation.
type camerasResponse struct {
	// Supported is whether this platform can enumerate devices at all. False on anything but Linux,
	// where Video4Linux does not exist — which is a different statement from having no cameras.
	Supported bool `json:"supported"`

	// Source is the capture source in force. A camera selection has no effect unless it is "gocv":
	// saying so is the difference between a settings page that is honest and one that appears to work.
	Source string `json:"source"`

	// SourceUsesCamera is whether the active source opens a camera at all.
	SourceUsesCamera bool `json:"source_uses_camera"`

	// KnownSources is what the source may be set to, so the page offers a choice rather than a text field
	// in which a typo becomes a receiver that captures nothing.
	KnownSources []string `json:"known_sources"`

	Devices []cameraView `json:"devices"`

	// Selection is what is configured now, and Effective is what would actually be opened — they differ
	// when a configured camera is no longer attached.
	Selection camera.Selection `json:"selection"`
	Effective camera.Selection `json:"effective"`

	// Substituted is true when the configured device is missing and another would be used instead. It is
	// reported rather than smoothed over, because capturing from a different camera than the one an
	// operator chose is exactly the kind of thing that silently ruins an installation.
	Substituted bool `json:"substituted"`

	// Error is why enumeration failed, if it did. Inside a container the usual cause is that no device
	// was passed through, which is a compose file to fix rather than a camera to buy.
	Error string `json:"error,omitempty"`
}

// liveCameraSource is the capture source that opens a real camera.
const liveCameraSource = "camera"

// listCameras reports the capture devices attached to this machine.
func (s *Server) listCameras(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Current()
	configured := camera.Selection{
		Device: cfg.Capture.Device,
		Format: cfg.Capture.Format,
		Width:  cfg.Capture.Width,
		Height: cfg.Capture.Height,
		FPS:    cfg.Capture.FPS,
	}

	response := camerasResponse{
		Supported:        camera.Available(),
		Source:           cfg.Capture.Source,
		SourceUsesCamera: cfg.Capture.Source == liveCameraSource,
		KnownSources:     pipeline.AvailableSources(),
		Selection:        configured,
	}

	devices, err := camera.List()
	if err != nil {
		response.Error = err.Error()
		s.respond(w, http.StatusOK, response)
		return
	}

	// The effective selection is resolved here rather than left to the client, because the fallback rule
	// — a named device, else the default, else nothing — is the receiver's behaviour and a second
	// implementation of it in the browser would be a second thing to get wrong.
	chosen, substituted, ok := camera.Selected(devices, configured.Device)
	response.Substituted = substituted
	if ok {
		response.Effective = configured
		response.Effective.Device = chosen.Path
		if response.Effective.Width == 0 {
			if best, found := camera.Best(chosen.Modes); found {
				response.Effective.Format = best.Format
				response.Effective.Width = best.Width
				response.Effective.Height = best.Height
				if len(best.FPS) > 0 {
					response.Effective.FPS = best.FPS[0]
				}
			}
		}
	}

	response.Devices = make([]cameraView, 0, len(devices))
	for _, device := range devices {
		response.Devices = append(response.Devices, cameraView{
			Device:   device,
			Selected: ok && device.Path == chosen.Path,
		})
	}

	s.respond(w, http.StatusOK, response)
}

// maxCameraRequestBytes bounds the selection body. It is a handful of fields; anything larger is not one.
const maxCameraRequestBytes = 4 << 10

// setCamera chooses the camera to capture from.
//
// The selection is validated against the devices actually attached before it is applied, because a V4L2
// driver handed a mode it does not support substitutes one rather than refusing — so a receiver that asked
// for 1920×1080 and was quietly given 640×480 would fail to resolve the cell grid and report it as an
// optical fault. Refusing here turns an invisible misconfiguration into a sentence.
func (s *Server) setCamera(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCameraRequestBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "could not read the request", err)
		return
	}
	if len(body) > maxCameraRequestBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, "the request is too large to be a camera selection", nil)
		return
	}

	var selection camera.Selection
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil {
		s.fail(w, http.StatusBadRequest, "the request is not a camera selection", err)
		return
	}

	// Checked against what this binary can open, not against what the protocol knows about. A source that
	// cannot be opened must be refused here, because it would be persisted and the next start could not
	// honour it — which is how a settings page came to be able to stop the receiver.
	if err := camera.CheckSource(selection.Source, pipeline.AvailableSources()); err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err.Error(), err)
		return
	}

	devices, err := camera.List()
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, "the capture devices on this machine could not be listed", err)
		return
	}

	// Naming a device without a mode is a request to be configured, not an incomplete request.
	//
	// An operator picking a camera has expressed the whole of their intent — use that one — and the best mode
	// for it is something the receiver can determine better than they can: the largest frame, and among equal
	// frames the fastest. So it is filled in here rather than demanded from them. Anyone who wants a
	// different mode says so and this leaves it alone.
	var autoConfigured bool
	if selection.Width == 0 && selection.Height == 0 && selection.FPS == 0 && selection.Format == "" {
		if best, ok := camera.PreferredFor(devices, selection.Device); ok && best.Width > 0 {
			best.Source = selection.Source
			selection = best
			autoConfigured = true
		}
	}

	if err := selection.Validate(devices); err != nil {
		// A 422 rather than a 400: the request was well formed and the mode was refused on its merits.
		s.fail(w, http.StatusUnprocessableEntity, err.Error(), err)
		return
	}

	// Applied before it is persisted, and persisted before the response. If persistence fails the
	// selection is still in force — which is the right order, because an operator watching a camera they
	// just chose should see it take effect, and a warning about it not surviving a restart is a smaller
	// problem than a click that appeared to do nothing.
	cfg := s.cfg.SetCamera(selection.Device, selection.Format, selection.Width, selection.Height, selection.FPS)
	s.log.Info("camera selected", zap.String("selection", selection.String()))

	sourceChanged := selection.Source != "" && selection.Source != cfg.Capture.Source

	// Persisted so the choice is in force at the next start as well as now.
	persisted := selection
	if persisted.Source == "" {
		persisted.Source = cfg.Capture.Source
	}

	var warning string
	if err := camera.SaveSelection(cfg.Storage.Root, persisted); err != nil {
		warning = "the camera is in use now, but the choice could not be saved and will not survive a restart"
		s.log.Warn("could not persist the camera selection", zap.Error(err))
	}

	response := map[string]any{
		"selection":       selection,
		"source":          cfg.Capture.Source,
		"applied":         true,
		"auto_configured": autoConfigured,
	}
	if autoConfigured {
		response["configured"] = "Configured automatically at " + selection.String() +
			" — the largest frame this camera offers, and the fastest at that size."
	}
	if warning != "" {
		response["warning"] = warning
	}
	// Applied to the running receiver, not merely recorded. Selecting a camera should mean the camera starts;
	// being told to restart the service is being asked to do work the service should do.
	//
	// The new source is opened before the old one is closed, so a camera that will not open leaves the receiver
	// capturing from whatever it had — a failed switch must not turn a working receiver into one with no
	// source at all.
	target := cfg.Capture
	if selection.Source != "" {
		target.Source = selection.Source
	}
	target.Device, target.Format = selection.Device, selection.Format
	target.Width, target.Height, target.FPS = selection.Width, selection.Height, selection.FPS

	if s.switchSource != nil {
		if err := s.switchSource(target); err != nil {
			// The selection is already saved, so the operator's choice is not lost — but the switch failed and
			// saying which is which matters: "recorded but not running" is a different situation from either
			// "running" or "refused".
			s.log.Warn("could not switch the capture source", zap.Error(err))
			response["applied"] = false
			response["error_detail"] = err.Error()
			response["note"] = "The choice is saved and will be used at the next start, but the source could " +
				"not be opened now: " + err.Error() + ". Capture continues from " + cfg.Capture.Source + "."
			s.respond(w, http.StatusOK, response)
			return
		}
		response["capturing_from"] = target.Source
		switch {
		case sourceChanged && target.Source == liveCameraSource:
			response["note"] = "The camera is open and capturing now. It waits quietly until frames appear on " +
				"the display — nothing is recorded while there is nothing to see."
		case sourceChanged:
			response["note"] = "Now capturing from " + target.Source + "."
		case target.Source == liveCameraSource:
			response["note"] = "The camera was reopened in the mode you chose."
		}
	}
	s.respond(w, http.StatusOK, response)
}
