package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/mediatype"

	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/optical"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// This file is the sender's *channel* surface, as opposed to the job surface in api.go: what is on the
// display right now, and what was on it earlier.
//
// The two are worth keeping apart because they answer to different clients. The job surface answers to
// whoever submitted a file. This one answers to a camera — or to the page a camera is pointed at, or to
// a receiver pulling frames over a network cable instead of photographing them — and its defining
// property is that it is about the present moment rather than about a record.

// maxDisplayPoll bounds how long a long-poll may hold a request open.
//
// It is well under nginx's read timeout on purpose. A request that outlives the proxy's patience is
// reported to the operator as a gateway error, which would make a working display look broken.
const maxDisplayPoll = 25 * time.Second

// displayFrame is the frame on screen, as a client sees it.
type displayFrame struct {
	// Sequence is the display counter. A client passes the last one it saw back as `after`, which is
	// what makes following the display a chain rather than a poll-and-hope.
	Sequence int64 `json:"sequence"`

	FrameNumber    int    `json:"frame_number"`
	TransmissionID string `json:"transmission_id,omitempty"`
	IsManifest     bool   `json:"is_manifest"`
	WidthPx        int    `json:"width_px"`
	HeightPx       int    `json:"height_px"`
	Bytes          int    `json:"bytes"`

	// ShownAt and AgeMS are both given because they answer different questions. The timestamp is for a
	// record; the age is for a viewer deciding whether the display has stalled, which it can then do
	// without trusting its own clock to agree with the sender's.
	ShownAt time.Time `json:"shown_at"`
	AgeMS   int64     `json:"age_ms"`

	// ImageURL is where to fetch the pixels. The sequence is in the query string so that each frame is
	// a distinct URL — a browser will not reuse a cached image for it, and a stale one is the failure
	// mode that would be hardest to notice, because the page would look like it was working.
	ImageURL string `json:"image_url"`

	// ImagePNG is the frame itself, base64, when the caller asked for it with `include=image`.
	//
	// Inlining an image in a JSON document is normally the wrong thing, and it is the right thing here
	// for one reason: a client following the display at frame rate cannot afford a second round trip per
	// frame. At 30 frames a second there are 33 milliseconds between frames, and a fetch that costs even
	// a few of those turns a display into a slideshow. Base64 costs a third more bytes over a link that
	// is either loopback or a cable, which is the cheaper of the two prices by a wide margin.
	ImagePNG string `json:"image_png,omitempty"`
}

// displayStatus is the display as a whole.
type displayStatus struct {
	// Sink names the real channel: a directory, or a monitor.
	Sink string `json:"sink"`

	// Live is whether anything is on the display at all.
	Live bool `json:"live"`

	// FPS and CellPx come from configuration, and are what a camera has to be fast enough and close
	// enough for. Reporting them beside the frame means the page a camera watches can also be the page
	// that tells an operator whether the settings are achievable.
	FPS      float64 `json:"fps"`
	CellPx   int     `json:"cell_px"`
	QuietPx  int     `json:"quiet_zone_px"`
	GridCols int     `json:"grid_cols"`
	GridRows int     `json:"grid_rows"`
	Encoder  string  `json:"encoder"`
	BitDepth int     `json:"bit_depth"`

	// Shown is the running total for this process, so an operator can see the display advancing even
	// while looking at a frame that has not changed — a keep-alive repeat looks identical otherwise.
	Shown int64 `json:"frames_shown"`

	Frame *displayFrame `json:"frame,omitempty"`
}

// frameView describes the frame on the display, or nil if there is none.
func (s *Server) frameView(frame optical.Frame, at time.Time, have, withImage bool) *displayFrame {
	if !have {
		return nil
	}
	view := &displayFrame{
		Sequence:    frame.Sequence,
		FrameNumber: frame.Number,
		IsManifest:  frame.Manifest,
		WidthPx:     frame.WidthPx,
		HeightPx:    frame.HeightPx,
		Bytes:       len(frame.PNG),
		ShownAt:     at,
		AgeMS:       time.Since(at).Milliseconds(),
		ImageURL:    "/api/v1/display/frame.png?sequence=" + strconv.FormatInt(frame.Sequence, 10),
	}
	if frame.Transmission != uuid.Nil {
		view.TransmissionID = frame.Transmission.String()
	}
	if withImage {
		view.ImagePNG = base64.StdEncoding.EncodeToString(frame.PNG)
	}
	return view
}

// wantsImage reports whether the caller asked for the pixels inline.
func wantsImage(r *http.Request) bool {
	for _, value := range r.URL.Query()["include"] {
		for part := range strings.SplitSeq(value, ",") {
			if strings.TrimSpace(part) == "image" {
				return true
			}
		}
	}
	return false
}

// getDisplay reports what is on the display and how it is configured.
func (s *Server) getDisplay(w http.ResponseWriter, r *http.Request) {
	if s.display == nil {
		s.fail(w, http.StatusNotImplemented, "this build has no live display", nil)
		return
	}
	cfg := s.cfg.Current()
	frame, at, have := s.display.Current()

	s.respond(w, http.StatusOK, displayStatus{
		Sink:     s.display.Name(),
		Live:     have,
		FPS:      cfg.Display.FPS,
		CellPx:   cfg.Optical.CellPixels,
		QuietPx:  cfg.Optical.QuietZone,
		GridCols: cfg.Optical.GridWidth,
		GridRows: cfg.Optical.GridHeight,
		Encoder:  cfg.Optical.Encoder,
		BitDepth: cfg.Optical.BitDepth,
		Shown:    s.display.Shown(),
		Frame:    s.frameView(frame, at, have, wantsImage(r)),
	})
}

// nextDisplayFrame blocks until the display moves past the sequence the caller last saw.
//
// Long-polling rather than an interval: at the frame rates this is built for, a poll fast enough not to
// miss frames would spend most of its requests learning nothing, and a poll slow enough to be cheap
// would miss them. Holding the request open costs one idle connection and reports the change the
// instant it happens, which is what a camera-facing page and an HTTP receiver both need.
//
// A timeout answers 204 rather than an error or an empty frame: nothing is wrong, and the caller's
// correct response is to ask again with the same sequence.
func (s *Server) nextDisplayFrame(w http.ResponseWriter, r *http.Request) {
	if s.display == nil {
		s.fail(w, http.StatusNotImplemented, "this build has no live display", nil)
		return
	}

	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "after must be a display sequence number", err)
			return
		}
		after = parsed
	}

	wait := maxDisplayPoll
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "timeout must be a duration, such as 10s", err)
			return
		}
		wait = min(max(parsed, time.Second), maxDisplayPoll)
	}

	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()

	frame, at, ok := s.display.Next(ctx, after)
	if !ok {
		// Distinguish the client going away from the poll simply expiring. Neither is an error, but
		// only one of them leaves anybody to tell.
		if r.Context().Err() == nil {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	s.respond(w, http.StatusOK, s.frameView(frame, at, true, wantsImage(r)))
}

// getDisplayFrameImage serves the pixels currently on the display.
//
// A `sequence` may be given, and is honoured when it is still the frame on screen. When it is not, the
// current frame is served anyway and the header says which — because the only client for this endpoint
// is something looking at the display now, and handing it a frame that has already gone would be worse
// than handing it the wrong sequence number. The header is how the caller finds out.
func (s *Server) getDisplayFrameImage(w http.ResponseWriter, r *http.Request) {
	if s.display == nil {
		s.fail(w, http.StatusNotImplemented, "this build has no live display", nil)
		return
	}
	frame, at, have := s.display.Current()
	if !have {
		s.fail(w, http.StatusNotFound, "nothing is on the display", nil)
		return
	}
	s.writePNG(w, frame.PNG, map[string]string{
		"X-OTP-Sequence":     strconv.FormatInt(frame.Sequence, 10),
		"X-OTP-Frame-Number": strconv.Itoa(frame.Number),
		"X-OTP-Shown-At":     at.UTC().Format(time.RFC3339Nano),
	})
}

// getFrameImage serves a stored frame, long after it was displayed.
//
// This is the audit path, and it is deliberately separate from the live one. A frame that decoded
// wrongly, was retransmitted, or was rebuilt from parity is a thing an operator will want to look at
// afterwards — and the useful question is "what exactly did we put on the screen", which only the
// stored bytes can answer. The frame is addressed by its number within the transmission rather than by
// a display sequence, because a retransmitted frame has many sequences and one identity.
func (s *Server) getFrameImage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number < 0 {
		s.fail(w, http.StatusBadRequest, "the frame number must be a non-negative integer", err)
		return
	}

	frame, err := s.store.Frames.GetByNumber(r.Context(), id, number)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such frame", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read the frame", err)
		return
	}

	body, err := objectstore.GetBytes(r.Context(), s.objects, frame.StoredPath, maxFrameImageBytes)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the frame image", err)
		return
	}

	// A stored frame never changes, so it is safe to cache hard — and worth it, because auditing a
	// transmission means paging through hundreds of these.
	s.writePNG(w, body, map[string]string{
		"Cache-Control":      "public, max-age=31536000, immutable",
		"X-OTP-Frame-Number": strconv.Itoa(frame.FrameNumber),
		"X-OTP-Displays":     strconv.Itoa(frame.DisplayedCount),
	})
}

// getOriginalFile serves the file a caller uploaded, as the sender still holds it.
//
// It is here so that an operator can see both ends of a transfer and compare them: the receiver renders
// what it reassembled, and this renders what was sent. A hash matching is the proof, but two pictures side
// by side are what makes an operator believe the proof — and if they ever differ, the difference itself is
// the diagnosis.
//
// `?inline=1` asks for it to be shown in place rather than downloaded, and is honoured only for the types
// shared/mediatype allows. Everything else is an opaque download whatever the query string says.
func (s *Server) getOriginalFile(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}

	transfer, err := s.store.Transmissions.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such transfer", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read the transfer", err)
		return
	}

	file, err := s.store.Files.Get(r.Context(), transfer.FileID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "the uploaded file is no longer held", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read the file", err)
		return
	}

	// Bounded by the size recorded at upload rather than by a constant: the limit that matters is "this
	// object is not what the row says it is", and a fixed ceiling would either refuse a legitimate large
	// transfer or let a wrong one through.
	body, err := objectstore.GetBytes(r.Context(), s.objects, file.StoredPath, file.SizeBytes+1)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the file contents", err)
		return
	}

	serveFile(w, body, file.Filename, hex.EncodeToString(file.SHA256),
		r.URL.Query().Get("inline") == "1", s.log)
}

// serveFile writes a file for a browser, either as a download or for showing in place.
//
// The two modes exist because they answer different needs and only one of them is safe by default. A
// download is an opaque byte stream with an attachment disposition, which no browser will interpret — the
// right treatment for a file that came from outside. Inline is opt-in per request and bounded by the
// allowlist in shared/mediatype, so a caller that forgets to check the kind still cannot serve script from
// this origin.
//
// nosniff matters as much as the declared type: without it a browser may disregard a type it disagrees
// with and guess from the bytes, which is exactly the decision this takes away from it.
func serveFile(w http.ResponseWriter, body []byte, filename, sha256Hex string, inline bool, log *zap.Logger) {
	contentType := "application/octet-stream"
	disposition := "attachment"
	if inline && mediatype.Inline(filename) {
		_, contentType = mediatype.Of(filename)
		disposition = "inline"
	}

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", disposition+`; filename="`+escapeHeaderFilename(filename)+`"`)
	h.Set("X-OTP-SHA256", sha256Hex)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if _, err := w.Write(body); err != nil {
		log.Warn("could not write the file", zap.Error(err))
	}
}

// escapeHeaderFilename makes a filename safe inside a quoted header parameter. The name is validated as a
// bare name on upload, so this is belt and braces — but a header built by concatenation is worth escaping
// regardless, because the validation and this line are far apart and only one of them looks load-bearing.
func escapeHeaderFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, `\\`)
	name = strings.ReplaceAll(name, `"`, `\"`)
	name = strings.ReplaceAll(name, "\r", "")
	return strings.ReplaceAll(name, "\n", "")
}

// maxFrameImageBytes bounds a frame read. A frame is a few tens of kilobytes at the densest settings
// this supports; anything approaching this is a stored path that does not hold what it claims to.
const maxFrameImageBytes = 64 << 20

// writePNG writes an image response with the headers a caller needs to know what it received.
func (s *Server) writePNG(w http.ResponseWriter, body []byte, headers map[string]string) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if _, ok := headers["Cache-Control"]; !ok {
		// The live frame is the opposite of cacheable: the whole point is that it changes.
		h.Set("Cache-Control", "no-store")
	}
	for name, value := range headers {
		h.Set(name, value)
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		s.log.Debug("could not write a frame image", zap.Error(err))
	}
}
