package api

import (
	"net/http"

	"github.com/opticaltransport/otp/receiver/internal/pipeline"
)

// The aiming surface: what the last captured frame says about where the camera is pointed.
//
// It is separate from the capture statistics because it answers a different question at a different
// cadence. The statistics say how a run went and are read after the fact; this is read several
// times a second by someone physically holding a camera, and every field exists to turn "it is not
// working" into a direction to move in.

// alignmentResponse is the aiming state as the camera page sees it.
type alignmentResponse struct {
	// Live is whether any frame has been measured in this capture session. False both before the
	// camera starts and immediately after it is restarted, and the page must not present a stale
	// reading as current — the whole value of this is that it describes now.
	Live bool `json:"live"`

	pipeline.Alignment
}

// getAlignment reports how the camera was pointed for the most recent frame.
func (s *Server) getAlignment(w http.ResponseWriter, r *http.Request) {
	// A build without the hook answers honestly rather than 404ing: the page asks for this on a
	// timer, and a route that sometimes does not exist is harder to write against than one that
	// always answers "nothing measured".
	if s.alignment == nil {
		s.respond(w, http.StatusOK, alignmentResponse{
			Alignment: pipeline.Alignment{
				Status: pipeline.StatusSearching,
				Advice: "This receiver cannot report camera alignment.",
			},
		})
		return
	}

	a, live := s.alignment()
	if !live {
		a.Status = pipeline.StatusSearching
		a.Advice = "No frames captured yet. Press Start and point the camera at the display."
	}
	s.respond(w, http.StatusOK, alignmentResponse{Live: live, Alignment: a})
}
