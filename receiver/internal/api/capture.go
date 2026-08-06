package api

import (
	"bytes"
	"image"
	_ "image/jpeg" // registered so a posted JPEG decodes
	_ "image/png"  // and a posted PNG
	"io"
	"net/http"
)

// Frames posted by a browser that is holding the camera.
//
// This is the other half of asking permission properly. A server process opening /dev/video0 gets no prompt and
// no operating-system indicator — the permission was granted when somebody passed the device into the container.
// A browser asking for a camera produces the dialog everybody recognises and the light the operating system
// controls, and it can do it from any machine that can reach the receiver rather than only the one the receiver
// runs on.
//
// The frames arrive here and go into the same pipeline as everything else. Nothing downstream knows or cares
// which source produced an image, which is what the Source interface is for.

// maxPostedFrameBytes bounds one posted frame.
//
// Generous for a JPEG of a 4K display and far below anything that could be mistaken for an upload: a frame is
// tens to hundreds of kilobytes, and something arriving at this size is not a frame.
const maxPostedFrameBytes = 16 << 20

// postFrame accepts one captured frame from a browser.
//
// The body is the image itself rather than a multipart form or JSON, because that is what a browser has after
// canvas.toBlob and wrapping it would mean encoding it twice. The content type is not trusted: the image is
// decoded to find out what it is, which is the only reliable way and is needed anyway.
func (s *Server) postFrame(w http.ResponseWriter, r *http.Request) {
	if s.pushFrame == nil {
		s.fail(w, http.StatusConflict,
			"this receiver is not taking posted frames; set the capture source to \"browser\" first", nil)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPostedFrameBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "could not read the frame", err)
		return
	}
	if len(body) == 0 {
		s.fail(w, http.StatusBadRequest, "the request carried no image", nil)
		return
	}
	if len(body) > maxPostedFrameBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, "that is too large to be a captured frame", nil)
		return
	}

	img, format, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		s.fail(w, http.StatusUnsupportedMediaType,
			"the body is not a decodable image; post a JPEG or a PNG", err)
		return
	}

	accepted, err := s.pushFrame(img, body)
	if err != nil {
		// The error's own message, not a summary of it. Every way this fails is a statement about the receiver's
		// state that the caller can act on — the source is not the one that takes posted frames, or it has been
		// closed — and replacing that with "the frame could not be queued" threw away the only useful part.
		// A conflict rather than a server error: nothing is broken, the receiver is just not in that state.
		s.fail(w, http.StatusConflict, err.Error(), err)
		return
	}

	// Accepted and "held nothing" are both successes and are reported apart, because they mean different things
	// to the page posting: one says keep going, the other says the display has nothing on it yet — and a page
	// that treated the second as an error would stop when it should wait.
	bounds := img.Bounds()
	s.respond(w, http.StatusOK, map[string]any{
		"accepted": accepted,
		"reason":   map[bool]string{true: "queued for decoding", false: "nothing on the display yet"}[accepted],
		"format":   format,
		"width":    bounds.Dx(),
		"height":   bounds.Dy(),
		"bytes":    len(body),
	})
}
