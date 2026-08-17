// Package api is the receiver's HTTP surface.
//
// The receiver has no request that starts work — frames arrive whether anybody asks or not — so this is
// almost entirely a read surface. What it exposes is the thing an operator standing next to a camera
// actually needs: is it decoding, how well, what is missing, and did the file that was supposed to
// arrive arrive intact.
package api

import (
	"archive/zip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/mediatype"
	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/receiver/internal/pipeline"
	"github.com/opticaltransport/otp/receiver/internal/store"
)

// Server is the HTTP API.
type Server struct {
	store   *store.Store
	objects objectstore.Store
	// acks is the acknowledgement channel's own store, rooted at a different volume than
	// objects. The API otherwise never touches it — acknowledgements are the pipeline's
	// business — but deleting a transmission means removing its acks/<id>/ objects too, and
	// only this server can be handed both stores at once.
	acks objectstore.Store
	cfg  *config.Watcher
	log  *zap.Logger

	// session reports the capture session currently running, so the dashboard can show live figures
	// without being told which session to look at.
	session func() uuid.UUID

	// capture reports the deepest backlog of unread frames. Injected because only the source can know it,
	// and the API must not reach into the pipeline to ask.
	capture func() int64

	// switchSource replaces the running capture source. Injected rather than called directly for the same
	// reason: the API's job is to decide whether a request is allowed, not to own the capture loop.
	switchSource func(config.Capture) error

	// pushFrame hands a frame captured by a browser to the running source. Nil when this build or deployment
	// cannot take them, which the handler reports rather than accepting frames into nothing.
	pushFrame func(image.Image, []byte) (bool, error)

	// browserStats reports what the browser source has seen. It exists because none of it left the process
	// before: the source counted every frame handed to it and every one the blank-screen gate turned away, and a
	// reader of this API could not tell "posting hard and every frame rejected" from "not posting at all". Both
	// showed flat session counters, no stored frames and an empty log, and three debugging sessions dead-ended on
	// that ambiguity. The browser shows these numbers to its own user; the receiver could not see them.
	browserStats func() (posted, gated, dropped int64)

	// browserActive reports whether the browser capture source has heard from a page recently. Selecting
	// "browser" as the source is not the same as a page actually posting frames to it — unlike the camera
	// source, which opens hardware the moment it is selected, the browser source just sits and waits. Nil
	// when this build has no way to ask, which is treated as "no".
	browserActive func() bool

	// alignment reports how the camera was pointed for the most recent frame. It backs the aiming
	// display: someone holding a phone at a monitor otherwise has nothing to go on but a decode rate
	// that arrives seconds later and says nothing about which way to move.
	alignment func() (pipeline.Alignment, bool)

	// recovery reports what the soft-decision retry has done this session. See Options.Recovery.
	recovery func() pipeline.RecoveryStats

	// ingest runs one uploaded frame image through the live pipeline: store, decode, apply — the same
	// path a captured frame takes, so acknowledgements, merge, and delivery all fire as normal. Nil when
	// this build or deployment cannot take imports.
	//
	// One result per frame the image held. Usually one; a photograph of a sheet printed several-up
	// carries several, the same way a photograph of a tiled display does.
	ingest func(context.Context, image.Image, []byte) ([]pipeline.IngestResult, error)

	// probe reports whether an image decodes as a frame, without storing anything. The import handler
	// uses it to tell a composite of two stacked frames from a single ordinary one — deciding that is a
	// decode question, and the API package must not know how decoding works, so it is injected exactly
	// like ingest is.
	probe func(image.Image) bool

	// importMu serializes /api/v1/import: the endpoint is unauthenticated, reads up to maxImportBytes
	// into memory per request, and feeds the same single-threaded applier every captured frame also
	// depends on. A second request arriving mid-import would double that memory cost for no benefit —
	// the applier was going to serialize the actual work anyway — so it is refused outright rather than
	// queued or left to run concurrently.
	importMu sync.Mutex
}

// Options configure a server.
type Options struct {
	Store   *store.Store
	Objects objectstore.Store
	// Acks is the acknowledgement channel's store, separate from Objects because it is rooted
	// at its own volume. Nil is fine for any handler that never deletes a transmission.
	Acks    objectstore.Store
	Config  *config.Watcher
	Log     *zap.Logger
	Session func() uuid.UUID
	Behind  func() int64
	Switch  func(config.Capture) error
	Push    func(image.Image, []byte) (bool, error)
	Ingest  func(context.Context, image.Image, []byte) ([]pipeline.IngestResult, error)
	Probe   func(image.Image) bool
	// BrowserActive reports whether the browser capture source has taken a frame recently. See the field
	// of the same purpose on Server.
	BrowserActive func() bool

	// BrowserStats reports what the browser source has seen: frames handed to it, frames the blank-screen gate
	// turned away, and frames dropped because the queue was full. Nil when this build has no way to ask.
	BrowserStats func() (posted, gated, dropped int64)

	// Alignment reports how the camera was pointed for the most recent frame, for the aiming display on
	// the camera page. The bool is false before any frame has been decoded.
	Alignment func() (pipeline.Alignment, bool)

	// Recovery reports what the soft-decision retry has done this session. Nil when this build has no
	// way to ask, which the session view reports as an absent object rather than as zeroes — "not
	// wired up" and "attempted nothing" are different facts about a receiver.
	Recovery func() pipeline.RecoveryStats
}

// New returns a server.
func New(opts Options) *Server {
	return &Server{
		store:         opts.Store,
		objects:       opts.Objects,
		acks:          opts.Acks,
		cfg:           opts.Config,
		log:           opts.Log.Named("api"),
		session:       opts.Session,
		capture:       opts.Behind,
		switchSource:  opts.Switch,
		pushFrame:     opts.Push,
		ingest:        opts.Ingest,
		probe:         opts.Probe,
		browserActive: opts.BrowserActive,
		browserStats:  opts.BrowserStats,
		alignment:     opts.Alignment,
		recovery:      opts.Recovery,
	}
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/session", s.getSession)
	mux.HandleFunc("GET /api/v1/transmissions", s.listTransmissions)
	mux.HandleFunc("GET /api/v1/transmissions/{id}", s.getTransmission)
	mux.HandleFunc("GET /api/v1/transmissions/{id}/chunks", s.listChunks)
	mux.HandleFunc("GET /api/v1/transmissions/{id}/missing", s.listMissing)
	mux.HandleFunc("GET /api/v1/transmissions/{id}/file", s.downloadMerged)
	// Where the file was sent and whether it got there. The receiver is the only side that knows: the URL
	// crossed the optical channel in the manifest, and the delivery was made from here.
	mux.HandleFunc("GET /api/v1/transmissions/{id}/deliveries", s.listDeliveries)
	// Removing one entirely: its rows across every table that carries its id, and every object
	// its layout named — chunks, the merged file, and its acknowledgements.
	mux.HandleFunc("DELETE /api/v1/transmissions/{id}", s.handleDeleteTransmission)
	mux.HandleFunc("GET /api/v1/frames/failed", s.listFailedFrames)
	// The newest captures, decoded or not: what a live page needs to show frames arriving.
	mux.HandleFunc("GET /api/v1/frames/recent", s.listRecentFrames)
	// Frames posted by a browser holding the camera — the path that can actually ask permission.
	mux.HandleFunc("POST /api/v1/capture/frames", s.postFrame)
	mux.HandleFunc("GET /api/v1/frames/{id}/image", s.frameImage)
	mux.HandleFunc("GET /api/v1/frames/{id}", s.getFrame)
	mux.HandleFunc("GET /api/v1/transmissions/{id}/frames", s.listTransmissionFrames)
	mux.HandleFunc("GET /api/v1/transmissions/{id}/frames.zip", s.downloadTransmissionFrames)
	// The sneakernet path: a frame archive replayed into the live pipeline exactly as though a
	// camera had seen each frame.
	mux.HandleFunc("POST /api/v1/import", s.postImport)
	mux.HandleFunc("GET /api/v1/config", s.getConfig)

	// The decryption keyring: keys go in and fingerprints come out, never the key itself.
	// The certificates this side identifies itself with, and the one it trusts. See certificates.go.
	mux.HandleFunc("GET /api/v1/certificates", s.getCertificates)
	mux.HandleFunc("POST /api/v1/certificates/generate", s.generateCertificate)
	// The public half as a file, to carry to the other side. There is no equivalent for the private
	// key, deliberately — see the handler.
	mux.HandleFunc("GET /api/v1/certificates/local.pem", s.downloadLocalCertificate)
	mux.HandleFunc("PUT /api/v1/certificates/peer", s.installPeerCertificate)
	mux.HandleFunc("DELETE /api/v1/certificates/peer", s.deletePeerCertificate)

	mux.HandleFunc("GET /api/v1/keys", s.listKeys)
	mux.HandleFunc("POST /api/v1/keys", s.addKey)
	mux.HandleFunc("DELETE /api/v1/keys/{id}", s.deleteKey)

	// The camera: what is attached, and which of it to use. Discovered rather than configured, which is
	// why it is not part of the configuration endpoint.
	mux.HandleFunc("GET /api/v1/cameras", s.listCameras)
	mux.HandleFunc("PUT /api/v1/cameras/selection", s.setCamera)

	// The blank-screen threshold. Adjustable while aiming a camera, because when it is set too high there is
	// nothing to look at: rejected frames reach neither the decoder nor the failure log.
	// How the camera is pointed, for aiming it. Polled while someone is physically moving a phone,
	// so it reports the last frame rather than an average: an average describes where the camera was.
	mux.HandleFunc("GET /api/v1/capture/alignment", s.getAlignment)

	mux.HandleFunc("GET /api/v1/capture/rgb", s.getRGBCamera)
	mux.HandleFunc("PUT /api/v1/capture/rgb", s.setRGBCamera)
	mux.HandleFunc("GET /api/v1/capture/gate", s.getCaptureGate)
	mux.HandleFunc("PUT /api/v1/capture/gate", s.setCaptureGate)

	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)

	return s.withCORS(s.withLogging(mux))
}

// decodeWorkers reports how many frames are decoded at once, resolving the zero default the same way the
// pipeline does so a settings page shows the number actually in force rather than the word "default".
func (s *Server) decodeWorkers() int {
	if configured := s.cfg.Current().Capture.DecodeWorkers; configured > 0 {
		return configured
	}
	return max(1, runtime.NumCPU()-1)
}

// behind reports the deepest backlog of unread frames, when the source can say.
func (s *Server) behind() int64 {
	if s.capture == nil {
		return 0
	}
	return s.capture()
}

// SessionView is the live state of the capture.
//
// It is the receiver's front page, and the fields are chosen for what an operator does with them: a
// falling decode rate means the camera needs attention, a rising unreadable count means it needs it
// now, and the frame counts together say whether anything is arriving at all.
type SessionView struct {
	ID             uuid.UUID  `json:"id"`
	Status         string     `json:"status"`
	Source         string     `json:"source"`
	TransmissionID *uuid.UUID `json:"transmission_id,omitempty"`
	FramesCaptured int64      `json:"frames_captured"`
	FramesDecoded  int64      `json:"frames_decoded"`
	FramesFailed   int64      `json:"frames_failed"`
	DecodeRate     float64    `json:"decode_rate"`
	StartedAt      time.Time  `json:"started_at"`
	Uptime         float64    `json:"uptime_seconds"`

	// Recovery is what the soft-decision retry did this session, and how frames finished by stage.
	//
	// Omitted rather than zeroed when the build cannot report it: an operator reading "0 attempted"
	// would conclude the channel is healthy, when the truth may be that nothing is asking.
	Recovery *pipeline.RecoveryStats `json:"recovery,omitempty"`
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id := uuid.Nil
	if s.session != nil {
		id = s.session()
	}
	if id == uuid.Nil {
		s.respond(w, http.StatusOK, map[string]any{"capturing": false})
		return
	}

	session, err := s.store.Sessions.Get(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the capture session", err)
		return
	}

	rate := 0.0
	if session.FramesCaptured > 0 {
		rate = float64(session.FramesDecoded) / float64(session.FramesCaptured)
	}
	// The source reported is the one in force now, not the one the session opened with.
	//
	// The row records what it started from, which is historically true and unhelpful on a live page: switching
	// to a camera mid-session left the page saying "file" while the camera was running. An operator reading
	// "Source" on a page called Live capture means "what is it reading from at this moment".
	source := session.Source
	if configured := s.cfg.Current().Capture.Source; configured != "" {
		source = configured
	}

	var recovery *pipeline.RecoveryStats
	if s.recovery != nil {
		stats := s.recovery()
		recovery = &stats
	}

	s.respond(w, http.StatusOK, SessionView{
		ID:             session.ID,
		Status:         session.Status,
		Source:         source,
		TransmissionID: session.TransmissionID,
		FramesCaptured: session.FramesCaptured,
		FramesDecoded:  session.FramesDecoded,
		FramesFailed:   session.FramesFailed,
		DecodeRate:     rate,
		StartedAt:      session.StartedAt,
		Uptime:         time.Since(session.StartedAt).Seconds(),
		Recovery:       recovery,
	})
}

// TransmissionView is one transfer as the receiver sees it.
type TransmissionView struct {
	TransmissionID uuid.UUID `json:"transmission_id"`
	Filename       string    `json:"filename"`
	OriginalSize   int64     `json:"original_size"`
	ExpectedSHA256 string    `json:"expected_sha256"`
	ChunkCount     int       `json:"chunk_count"`
	ChunkSize      int       `json:"chunk_size"`
	CallbackURL    string    `json:"callback_url,omitempty"`

	ChunksArrived   int     `json:"chunks_arrived"`
	ChunksRecovered int     `json:"chunks_recovered"`
	MissingCount    int     `json:"missing_count"`
	Progress        float64 `json:"progress"`

	// Merged is the reassembly's outcome, present once every chunk has arrived. Verified is the field
	// that matters: it is the comparison of the merged file against the hash the manifest declared,
	// and the only thing that can say the transfer actually worked.
	Merged   *MergedView `json:"merged,omitempty"`
	Received time.Time   `json:"manifest_received_at"`
}

// MergedView is a reassembled file.
type MergedView struct {
	Filename    string     `json:"filename"`
	SizeBytes   int64      `json:"size_bytes"`
	SHA256      string     `json:"sha256"`
	Verified    bool       `json:"verified"`
	VerifyError string     `json:"verify_error,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
}

func (s *Server) listTransmissions(w http.ResponseWriter, r *http.Request) {
	// Every transmission, not only the incomplete ones. Listing from Pending was a bug that made a file
	// disappear from the interface at the moment it arrived successfully, because Pending excludes anything
	// already merged and verified — which is precisely what an operator is looking for.
	ids, err := s.store.Manifests.All(r.Context(), 100)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list transmissions", err)
		return
	}
	views := make([]TransmissionView, 0, len(ids))
	for _, id := range ids {
		view, err := s.view(r, id)
		if err != nil {
			continue
		}
		views = append(views, view)
	}
	s.respond(w, http.StatusOK, map[string]any{"transmissions": views})
}

func (s *Server) getTransmission(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}
	view, err := s.view(r, id)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "nothing known about that transmission", nil)
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the transmission", err)
		return
	}
	s.respond(w, http.StatusOK, view)
}

func (s *Server) view(r *http.Request, id uuid.UUID) (TransmissionView, error) {
	ctx := r.Context()

	manifest, err := s.store.Manifests.Get(ctx, id)
	if err != nil {
		return TransmissionView{}, err
	}
	arrived, recovered, err := s.store.Chunks.Counts(ctx, id)
	if err != nil {
		return TransmissionView{}, err
	}
	missing, err := s.store.Chunks.Missing(ctx, id)
	if err != nil {
		return TransmissionView{}, err
	}

	progress := 0.0
	if manifest.ChunkCount > 0 {
		progress = float64(arrived) / float64(manifest.ChunkCount)
	}

	view := TransmissionView{
		TransmissionID:  id,
		Filename:        manifest.Filename,
		OriginalSize:    manifest.OriginalSize,
		ExpectedSHA256:  hex.EncodeToString(manifest.OriginalSHA256),
		ChunkCount:      manifest.ChunkCount,
		ChunkSize:       manifest.ChunkSize,
		CallbackURL:     manifest.CallbackURL,
		ChunksArrived:   arrived,
		ChunksRecovered: recovered,
		MissingCount:    len(missing),
		Progress:        progress,
		Received:        manifest.ReceivedAt,
	}

	if merged, err := s.store.Merged.Get(ctx, id); err == nil {
		view.Merged = &MergedView{
			Filename:    merged.Filename,
			SizeBytes:   merged.SizeBytes,
			SHA256:      hex.EncodeToString(merged.SHA256),
			Verified:    merged.Verified,
			VerifyError: merged.VerifyError,
			VerifiedAt:  merged.VerifiedAt,
		}
	}
	return view, nil
}

func (s *Server) listChunks(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}
	chunks, err := s.store.Chunks.List(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list chunks", err)
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"chunks": chunks})
}

func (s *Server) listMissing(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}
	missing, err := s.store.Chunks.Missing(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list the outstanding chunks", err)
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"missing": missing, "count": len(missing)})
}

// downloadMerged serves a reassembled file.
//
// A file that failed verification is refused rather than served. It exists on disk — it is the only
// evidence of what went wrong — but handing it out on the same endpoint as a verified one would let a
// caller treat corrupt data as the file they asked for.
func (s *Server) downloadMerged(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}

	merged, err := s.store.Merged.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "nothing has been merged for that transmission", nil)
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the merged file", err)
		return
	}
	if !merged.Verified {
		s.fail(w, http.StatusConflict,
			"the merged file did not match the hash the sender declared: "+merged.VerifyError, nil)
		return
	}

	body, err := objectstore.GetBytes(r.Context(), s.objects, merged.StoredPath, merged.SizeBytes+1)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the merged file", err)
		return
	}

	serveFile(w, body, merged.Filename, hex.EncodeToString(merged.SHA256),
		r.URL.Query().Get("inline") == "1", s.log)
}

// serveFile writes a transferred file, either as a download or for showing in place.
//
// Two modes rather than one, because they answer different needs and only one of them is safe by default.
// The download is an opaque byte stream with an attachment disposition: no browser will interpret it,
// which is the right treatment for a file that arrived from outside. Inline is opt-in per request and
// bounded by the allowlist in shared/mediatype — a file that may not be rendered is served as a download
// whatever the query string asked for, so a caller that forgets to check cannot serve script from this
// origin.
//
// nosniff matters as much as the type. Without it a browser may disregard a declared type it disagrees
// with and guess from the bytes, which is precisely the decision this is trying to take away from it.
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
	// The filename is quoted and its quotes and backslashes escaped. It is validated as a bare name on the
	// way in, so this is belt and braces — but a header built by concatenation is worth escaping anyway,
	// because the validation and this line are far apart and only one of them is obviously load-bearing.
	h.Set("Content-Disposition", disposition+`; filename="`+escapeHeaderFilename(filename)+`"`)
	h.Set("X-OTP-SHA256", sha256Hex)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if _, err := w.Write(body); err != nil {
		log.Warn("could not write the file", zap.Error(err))
	}
}

// escapeHeaderFilename makes a filename safe inside a quoted header parameter.
func escapeHeaderFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, `\\`)
	name = strings.ReplaceAll(name, `"`, `\"`)
	// A newline in a header would split it. The name cannot contain one by the time it gets here, and this
	// costs nothing.
	name = strings.ReplaceAll(name, "\r", "")
	return strings.ReplaceAll(name, "\n", "")
}

// listFailedFrames returns the frames that could not be read.
//
// They are the receiver's most useful diagnostic: a rising count says the channel is degrading, and the
// frames themselves say how — a blur, a tear, a frame half off the sensor. Keeping them is why captures
// are written to disk before they are decoded.
func (s *Server) listFailedFrames(w http.ResponseWriter, r *http.Request) {
	id := uuid.Nil
	if s.session != nil {
		id = s.session()
	}
	if id == uuid.Nil {
		s.respond(w, http.StatusOK, map[string]any{"frames": []any{}})
		return
	}
	frames, err := s.store.Frames.Failed(r.Context(), id, queryInt(r, "limit", 50))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list the unreadable frames", err)
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"frames": frames})
}

// frameImage serves a stored capture, so an operator can look at what the camera actually saw.
func (s *Server) frameImage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the frame id is not a UUID", err)
		return
	}

	// The stored path comes from the database rather than from the request, so a caller cannot name an
	// arbitrary object and have it served back.
	var path string
	var size int64
	err = s.store.Pool().QueryRow(r.Context(),
		`SELECT stored_path, 0 FROM captured_frames WHERE id = $1`, id).Scan(&path, &size)
	if err != nil {
		s.fail(w, http.StatusNotFound, "no such frame", err)
		return
	}

	body, err := objectstore.GetBytes(r.Context(), s.objects, path, 64<<20)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the frame", err)
		return
	}
	// Sniffed rather than declared. Captures are keyed .png because that is what the object layer
	// names them, but they hold whatever the camera posted — a browser posts JPEG — and serving a JPEG
	// labelled image/png works in a browser by luck and fails everywhere stricter.
	w.Header().Set("Content-Type", http.DetectContentType(body))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if _, err := w.Write(body); err != nil {
		s.log.Warn("could not write the frame", zap.Error(err))
	}
}

// getFrame reports everything recorded about one captured frame.
//
// The siblings are the point as much as the frame itself. A tiled display puts several independent
// frames in one photograph and each becomes its own row against the same stored image, so "what
// happened to this picture" is a question about all of them: one lane reading and another failing at
// the same instant, under the same exposure, is the single most useful thing this view can show.
func (s *Server) getFrame(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the frame id is not a UUID", err)
		return
	}

	frame, err := s.store.Frames.Get(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusNotFound, "no such frame", err)
		return
	}

	// Everything else read out of the same photograph, this frame included, in lane order.
	siblings, err := s.store.Frames.SharingImage(r.Context(), frame.SessionID, frame.StoredPath)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the other lanes", err)
		return
	}

	s.respond(w, http.StatusOK, map[string]any{
		"frame": frame,
		"lanes": siblings,
	})
}

// listTransmissionFrames reports the frames captured for one transfer.
func (s *Server) listTransmissionFrames(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}

	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = min(parsed, 5000)
		}
	}

	frames, err := s.store.Frames.ForTransmission(r.Context(), id, limit)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the transfer's frames", err)
		return
	}
	if frames == nil {
		frames = []store.CapturedFrame{}
	}
	s.respond(w, http.StatusOK, map[string]any{"frames": frames})
}

// downloadTransmissionFrames streams every captured photograph of a transfer as a zip.
//
// This exists so a transfer that went badly can be replayed offline against ai/corpus, which is the
// only honest way to judge a change that claims to improve decoding. Reproducing a real channel from
// a description is not possible; reproducing it from the actual photographs is exact.
//
// Streamed rather than assembled in memory. A session runs to hundreds of frames at half a megabyte
// each, and buffering that to set a Content-Length would spend hundreds of megabytes to save the
// client a progress bar.
//
// Each stored image appears once even though a tiled photograph has several rows against it, and a
// manifest names which lanes came out of which file, so the set can be read without the database.
func (s *Server) downloadTransmissionFrames(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}

	frames, err := s.store.Frames.ForTransmission(r.Context(), id, 5000)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the transfer's frames", err)
		return
	}
	if len(frames) == 0 {
		s.fail(w, http.StatusNotFound, "this transfer has no captured frames", nil)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "captures-"+id.String()+".zip"))

	zw := zip.NewWriter(w)
	defer func() {
		if err := zw.Close(); err != nil {
			s.log.Warn("could not finish the capture archive", zap.Error(err))
		}
	}()

	// The index is written first so a partial download is still readable: a transfer aborted halfway
	// leaves the manifest and the frames that made it, rather than a zip whose only description of
	// itself never arrived.
	index, err := json.MarshalIndent(map[string]any{
		"transmission": id,
		"frames":       frames,
	}, "", "  ")
	if err != nil {
		s.log.Warn("could not describe the capture archive", zap.Error(err))
		return
	}
	if err := writeZipEntry(zw, "frames.json", index); err != nil {
		s.log.Warn("could not write the capture index", zap.Error(err))
		return
	}

	// One entry per stored image. Several lanes share a photograph, and writing it once per lane would
	// multiply the archive by the lane count to carry identical copies.
	written := map[string]bool{}
	for _, f := range frames {
		if written[f.StoredPath] || r.Context().Err() != nil {
			continue
		}
		written[f.StoredPath] = true

		body, err := objectstore.GetBytes(r.Context(), s.objects, f.StoredPath, 64<<20)
		if err != nil {
			// One unreadable object does not spoil the archive. The frame is in frames.json, so its
			// absence is discoverable rather than silent.
			s.log.Warn("could not read a frame for the archive",
				zap.String("path", f.StoredPath), zap.Error(err))
			continue
		}

		name := fmt.Sprintf("%012d%s", f.Sequence, captureExtension(body))
		if err := writeZipEntry(zw, name, body); err != nil {
			s.log.Warn("could not add a frame to the archive", zap.Error(err))
			return
		}
	}
}

// writeZipEntry adds one already-compressed-or-not blob under a name.
//
// Stored rather than deflated: captures are JPEG or PNG, both already compressed, so deflating them
// spends CPU on every frame to save a percent or two of a download that is already streaming.
func writeZipEntry(zw *zip.Writer, name string, body []byte) error {
	entry, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err != nil {
		return err
	}
	_, err = entry.Write(body)
	return err
}

// captureExtension names a stored capture by what it actually contains.
//
// Stored objects are keyed .png whatever the camera posted, and a browser posts JPEG. Naming the
// archive's entries by the key would produce a directory of .png files that are not PNGs, which is
// exactly the trap the corpus harness documents having fallen into.
func captureExtension(body []byte) string {
	switch http.DetectContentType(body) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

// getConfig reports the decoder settings in force, including the ones that can be changed while
// running.
// DeliveryView is one attempt to hand a merged file to its callback URL.
type DeliveryView struct {
	URL         string     `json:"url"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"max_attempts"`
	HTTPStatus  *int       `json:"http_status,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transmission id is not a UUID", err)
		return
	}
	list, err := s.store.Callbacks.ForTransmission(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the deliveries", err)
		return
	}

	views := make([]DeliveryView, 0, len(list))
	for _, c := range list {
		views = append(views, DeliveryView{
			URL:         c.URL,
			Status:      c.Status,
			Attempts:    c.Attempts,
			MaxAttempts: c.MaxAttempts,
			HTTPStatus:  c.LastStatus,
			LastError:   c.LastError,
			DeliveredAt: c.DeliveredAt,
		})
	}
	s.respond(w, http.StatusOK, map[string]any{"deliveries": views})
}

// listRecentFrames returns the newest captures of the running session.
//
// Both outcomes, decoded and not. A page showing only what decoded would look healthy while a camera drifted out
// of focus; one showing only failures would look broken during a perfect transfer.
func (s *Server) listRecentFrames(w http.ResponseWriter, r *http.Request) {
	id := uuid.Nil
	if s.session != nil {
		id = s.session()
	}
	if id == uuid.Nil {
		s.respond(w, http.StatusOK, map[string]any{"frames": []any{}, "capturing": false})
		return
	}

	limit := 40
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = min(parsed, 200)
		}
	}

	frames, err := s.store.Frames.Recent(r.Context(), id, limit)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the recent captures", err)
		return
	}
	// An empty list rather than null. A nil slice marshals as null, and a client that has to treat null and []
	// as the same thing will one day forget to.
	if frames == nil {
		frames = []store.CapturedFrame{}
	}
	s.respond(w, http.StatusOK, map[string]any{"frames": frames, "capturing": true})
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Current()
	s.respond(w, http.StatusOK, map[string]any{
		"protocol_version": protocol.Current,
		"capture": map[string]any{
			"source":            cfg.Capture.Source,
			"dir":               cfg.Capture.Dir,
			"idle_interval":     cfg.Capture.IdleInterval.String(),
			"min_tone_fraction": cfg.Capture.MinToneFraction,
			// The number that decides whether the receiver can keep up with the display. Decoding is what
			// it spends its time on, and it scales with cores almost exactly.
			"decode_workers":     cfg.Capture.DecodeWorkers,
			"decode_workers_now": s.decodeWorkers(),
			// The deepest backlog of unread frames: one means the receiver kept up, a large number means the
			// display is producing faster than it can decode.
			"frames_behind": s.behind(),
		},
		"decoder": map[string]any{
			"min_finder_score": cfg.Decoder.MinFinderScore,
			"min_timing_score": cfg.Decoder.MinTimingScore,
			"encrypted":        cfg.Decoder.EncryptionKeyHex != "",
		},
		"callback": map[string]any{
			"allowed_hosts":  cfg.Callback.AllowedHosts,
			"allow_any_host": cfg.Callback.AllowAnyHost,
		},
		// Where an operator can see the sending side of the same transfer. For their browser, not for this
		// process — the two applications still share only a protocol and a directory.
		"peer": map[string]any{
			"sender_ui_url": cfg.Peer.SenderUIURL,
		},
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.respond(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"protocol_version": protocol.Current,
	})
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	s.respond(w, http.StatusOK, map[string]any{"status": "alive"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Healthy(r.Context()); err != nil {
		s.respond(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not ready",
			"reason": "the database is unreachable",
		})
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"status": "ready"})
}

// withCORS lets the browser UI, served from its own origin in development, reach this API.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins := s.cfg.Current().Server.CORSOrigins
		origin := r.Header.Get("Origin")
		for _, allowed := range origins {
			if strings.EqualFold(origin, allowed) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				break
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", requestID)

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		// Debug rather than info: the UI polls these endpoints once a second, and at info they would
		// bury the lines that describe the capture.
		s.log.Debug("request",
			zap.String("request_id", requestID),
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", recorder.status),
			zap.Duration("took", time.Since(started)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *Server) respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Warn("could not write a response", zap.Error(err))
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, message string, err error) {
	if err != nil {
		s.log.Warn("request failed", zap.Int("status", status), zap.String("message", message), zap.Error(err))
	}
	s.respond(w, status, map[string]any{"error": message})
}

func queryInt(r *http.Request, field string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(field))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}
