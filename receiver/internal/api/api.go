// Package api is the receiver's HTTP surface.
//
// The receiver has no request that starts work — frames arrive whether anybody asks or not — so this is
// almost entirely a read surface. What it exposes is the thing an operator standing next to a camera
// actually needs: is it decoding, how well, what is missing, and did the file that was supposed to
// arrive arrive intact.
package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/receiver/internal/store"
)

// Server is the HTTP API.
type Server struct {
	store   *store.Store
	objects objectstore.Store
	cfg     *config.Watcher
	log     *zap.Logger

	// session reports the capture session currently running, so the dashboard can show live figures
	// without being told which session to look at.
	session func() uuid.UUID
}

// Options configure a server.
type Options struct {
	Store   *store.Store
	Objects objectstore.Store
	Config  *config.Watcher
	Log     *zap.Logger
	Session func() uuid.UUID
}

// New returns a server.
func New(opts Options) *Server {
	return &Server{
		store:   opts.Store,
		objects: opts.Objects,
		cfg:     opts.Config,
		log:     opts.Log.Named("api"),
		session: opts.Session,
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
	mux.HandleFunc("GET /api/v1/frames/failed", s.listFailedFrames)
	mux.HandleFunc("GET /api/v1/frames/{id}/image", s.frameImage)
	mux.HandleFunc("GET /api/v1/config", s.getConfig)

	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)

	return s.withCORS(s.withLogging(mux))
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
	s.respond(w, http.StatusOK, SessionView{
		ID:             session.ID,
		Status:         session.Status,
		Source:         session.Source,
		TransmissionID: session.TransmissionID,
		FramesCaptured: session.FramesCaptured,
		FramesDecoded:  session.FramesDecoded,
		FramesFailed:   session.FramesFailed,
		DecodeRate:     rate,
		StartedAt:      session.StartedAt,
		Uptime:         time.Since(session.StartedAt).Seconds(),
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
	ids, err := s.store.Manifests.Pending(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list transmissions", err)
		return
	}

	// Pending returns the incomplete ones; the merged table holds the finished ones. Both are wanted
	// here, because an operator's history is the interesting part after the fact.
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

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+merged.Filename+"\"")
	w.Header().Set("X-OTP-SHA256", hex.EncodeToString(merged.SHA256))
	w.Header().Set("Content-Length", strconv.FormatInt(merged.SizeBytes, 10))
	if _, err := w.Write(body); err != nil {
		s.log.Warn("could not write the merged file", zap.Error(err))
	}
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
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if _, err := w.Write(body); err != nil {
		s.log.Warn("could not write the frame", zap.Error(err))
	}
}

// getConfig reports the decoder settings in force, including the ones that can be changed while
// running.
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Current()
	s.respond(w, http.StatusOK, map[string]any{
		"protocol_version": protocol.Current,
		"capture": map[string]any{
			"source":        cfg.Capture.Source,
			"dir":           cfg.Capture.Dir,
			"idle_interval": cfg.Capture.IdleInterval.String(),
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
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
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
