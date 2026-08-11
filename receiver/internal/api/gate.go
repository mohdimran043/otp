package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"go.uber.org/zap"
)

// maxGateRequestBytes bounds the blank-screen threshold request. It carries one number.
const maxGateRequestBytes = 256

// gateRequest is a change to the blank-screen threshold.
type gateRequest struct {
	// MinToneFraction is how much of a captured image must be dark, and how much light, before it is worth
	// decoding. A pointer so that an absent field is distinguishable from a deliberate zero — and zero is the
	// value that turns the gate off, which is exactly what someone aiming a camera reaches for.
	MinToneFraction *float64 `json:"min_tone_fraction"`
}

// gateResponse reports the threshold in force.
type gateResponse struct {
	MinToneFraction float64 `json:"min_tone_fraction"`

	// Note explains what the number does, because a bare fraction on a settings page is not self-describing and
	// this one can hide every frame if it is set wrong.
	Note string `json:"note"`
}

// setCaptureGate adjusts how much contrast a captured image needs before the receiver tries to decode it.
//
// The threshold exists so a camera pointed at a blank screen does not spend the whole receiver discovering
// nothing, and storing a picture of nothing each time. It was a hard-coded twelfth, and being wrong there is
// invisible in a way no other setting is: a rejected image reaches neither the decoder nor the failure log, so
// frames are posted, counted as accepted, and then simply disappear. There is nothing to look at and no way to
// tell it from a decode failure.
//
// It bites hardest on the case it was meant to serve. A binary frame is pure black and white at source, but two
// levels average toward flat grey as soon as the cells blur together, collapsing both tails at once — a binary
// frame filling 65% of a 1920x1080 shot with mild blur already fails a twelfth. So the one moment an operator
// most needs to see what the camera sees is the moment this is most likely to hide it.
//
// Live, taking effect on the next frame read, and deliberately not persisted: the configured value is what a
// restart returns to. That is the right shape for a debugging override, and it is safe here in a way it was not
// for the sender's display sink — that setting was read only at startup, so a change that did not survive a
// restart could never take effect at all. This one applies immediately.
func (s *Server) setCaptureGate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGateRequestBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "could not read the request", err)
		return
	}
	if len(body) > maxGateRequestBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, "the request is too large to be a threshold", nil)
		return
	}

	var request gateRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		s.fail(w, http.StatusBadRequest, "the request is not a capture gate setting", err)
		return
	}
	if request.MinToneFraction == nil {
		s.fail(w, http.StatusBadRequest, "min_tone_fraction is required", nil)
		return
	}

	applied := s.cfg.SetMinToneFraction(*request.MinToneFraction)
	fraction := applied.Capture.MinToneFraction

	s.log.Info("capture gate changed", zap.Float64("min_tone_fraction", fraction))
	s.respond(w, http.StatusOK, gateResponse{
		MinToneFraction: fraction,
		Note:            gateNote(fraction),
	})
}

// getCaptureGate reports the threshold in force.
func (s *Server) getCaptureGate(w http.ResponseWriter, r *http.Request) {
	fraction := s.cfg.Current().Capture.MinToneFraction
	s.respond(w, http.StatusOK, gateResponse{
		MinToneFraction: fraction,
		Note:            gateNote(fraction),
	})
}

// gateNote says what the current value means in terms of what an operator will observe.
func gateNote(fraction float64) string {
	switch {
	case fraction <= 0:
		return "off: every captured image goes to the decoder, which filters on its own checksums. " +
			"Costs wasted decode attempts and hides nothing."
	case fraction < 0.04:
		return "relaxed: a small or slightly soft frame still reaches the decoder. Use this while aiming."
	case fraction < 0.09:
		return "default: suits a frame that fills a good part of the view."
	default:
		return "strict: only a frame with strong black and white reaches the decoder. " +
			"A distant or blurred frame will be discarded before you can see why."
	}
}
