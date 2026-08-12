// Package api is the sender's HTTP surface.
//
// The entry point is one request: a file and a callback URL. Everything else the platform does
// follows from it — compress, chunk, error-code, render, display, watch for acknowledgements — and the
// caller's only further involvement is the callback they nominated, which the receiver eventually
// delivers the file to. So this package's job is to take that request, refuse it if it cannot be
// honoured, and hand it to the pipeline in a form that cannot be misread.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/compress"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/fec"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/readable"

	"github.com/opticaltransport/otp/sender/internal/ackwatch"
	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/jobs"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/optical"
	"github.com/opticaltransport/otp/sender/internal/pipeline"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Server is the HTTP API.
type Server struct {
	store    *store.Store
	jobs     *jobs.Store
	objects  objectstore.Store
	pipeline *pipeline.Pipeline
	acks     *ackwatch.Watcher
	cfg      *config.Watcher
	log      *zap.Logger

	// display is the live view of the channel, and may be nil: a build or a deployment without one
	// still transmits perfectly well, and the display endpoints say so rather than pretending.
	display *optical.Live

	// transmit is called to start displaying a prepared transmission. It is injected rather than
	// called directly so the API does not own the display loop: an API handler that blocked until a
	// transfer finished would hold a request open for the length of a transmission.
	transmit func(ctx context.Context, transmissionID uuid.UUID)
}

// Options configure a server.
type Options struct {
	Store    *store.Store
	Jobs     *jobs.Store
	Objects  objectstore.Store
	Pipeline *pipeline.Pipeline
	Acks     *ackwatch.Watcher
	Config   *config.Watcher
	Display  *optical.Live
	Log      *zap.Logger
	Transmit func(ctx context.Context, transmissionID uuid.UUID)
}

// New returns a server.
func New(opts Options) *Server {
	return &Server{
		store:    opts.Store,
		jobs:     opts.Jobs,
		objects:  opts.Objects,
		pipeline: opts.Pipeline,
		acks:     opts.Acks,
		cfg:      opts.Config,
		display:  opts.Display,
		log:      opts.Log.Named("api"),
		transmit: opts.Transmit,
	}
}

// Routes returns the HTTP handler.
//
// The standard library's router is enough here. The route set is small and its shape is stable, and a
// framework would add a dependency whose main contribution would be a different way to spell the same
// three verbs.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/transfers", s.createTransfer)
	mux.HandleFunc("GET /api/v1/transfers/{id}", s.getTransfer)
	mux.HandleFunc("GET /api/v1/transfers", s.listTransfers)
	mux.HandleFunc("GET /api/v1/transfers/{id}/chunks", s.listChunks)
	mux.HandleFunc("GET /api/v1/transfers/{id}/frames", s.listFrames)
	mux.HandleFunc("GET /api/v1/transfers/{id}/frames/{number}/image", s.getFrameImage)
	// The sneakernet path: every frame as one download, for a USB stick instead of a camera.
	mux.HandleFunc("GET /api/v1/transfers/{id}/frames/archive", s.getFrameArchive)
	// The file as it was uploaded, so both ends of a transfer can be looked at side by side.
	mux.HandleFunc("GET /api/v1/transfers/{id}/file", s.getOriginalFile)
	mux.HandleFunc("GET /api/v1/transfers/{id}/jobs", s.listJobs)
	mux.HandleFunc("GET /api/v1/transfers/{id}/result", s.getResult)

	// Stopping one. A status change on the row, which the display loop re-reads every frame.
	mux.HandleFunc("POST /api/v1/transfers/{id}/cancel", s.cancelTransfer)
	mux.HandleFunc("POST /api/v1/transfers/{id}/pause", s.pauseTransfer)
	mux.HandleFunc("POST /api/v1/transfers/{id}/resume", s.resumeTransfer)
	mux.HandleFunc("POST /api/v1/transfers/{id}/start", s.startTransfer)

	// Removing one entirely: the row and every object the pipeline wrote for it, as opposed
	// to cancel, which stops a transfer but keeps its history.
	mux.HandleFunc("DELETE /api/v1/transfers/{id}", s.handleDeleteTransfer)

	// The channel, as opposed to the queue: what is on the display now, for a camera to watch and for a
	// receiver to follow.
	mux.HandleFunc("GET /api/v1/display", s.getDisplay)
	mux.HandleFunc("GET /api/v1/display/next", s.nextDisplayFrame)
	mux.HandleFunc("GET /api/v1/display/frame.png", s.getDisplayFrameImage)

	// Driving the display by hand: stop it, step it, let it go. Same verb style as the transfer
	// controls, because they are the same kind of thing — an operator taking over from a loop.
	mux.HandleFunc("POST /api/v1/display/hold", s.holdDisplay)
	mux.HandleFunc("POST /api/v1/display/release", s.releaseDisplay)
	mux.HandleFunc("POST /api/v1/display/frame", s.showFrame)

	// The display's own settings: frame rate now, geometry when nothing is in flight.
	mux.HandleFunc("GET /api/v1/settings", s.getSettings)
	mux.HandleFunc("PATCH /api/v1/settings", s.updateSettings)

	mux.HandleFunc("GET /api/v1/profiles", s.listProfiles)

	// Saved encryption keys: keys go in and fingerprints come out, never the key itself. A
	// transfer picks one of these by id (encryption_key_id) as an alternative to pasting its
	// hex into every request.
	mux.HandleFunc("GET /api/v1/keys", s.listKeys)
	mux.HandleFunc("POST /api/v1/keys", s.addKey)
	mux.HandleFunc("DELETE /api/v1/keys/{id}", s.deleteKey)

	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)

	return s.withLogging(mux)
}

// TransferRequest is the API's entry point: a file and where the result should go.
type TransferRequest struct {
	// Filename is the name the receiver will write the file under. It is validated as a bare name,
	// because it crosses the air gap and is then used to write a file on another machine.
	Filename string `json:"filename"`

	// CallbackURL is where the receiver delivers the merged file once it has verified it. It travels
	// in the manifest, so the receiver — the side that ends up holding the file — knows where it goes.
	CallbackURL string `json:"callback_url"`

	// The optical profile, all optional: anything left unset comes from configuration.
	Encoder      string `json:"encoder,omitempty"`
	BitDepth     int    `json:"bit_depth,omitempty"`
	Compression  string `json:"compression,omitempty"`
	Level        int    `json:"level,omitempty"`
	FECCodec     string `json:"fec_codec,omitempty"`
	DataShards   int    `json:"fec_data_shards,omitempty"`
	ParityShards int    `json:"fec_parity_shards,omitempty"`
	Priority     string `json:"priority,omitempty"`

	// Autostart displays the transmission as soon as it is prepared. Off means an operator starts it
	// from the UI, which is what a scheduled transfer window needs.
	Autostart bool `json:"autostart,omitempty"`

	// Encryption is the cipher name and EncryptionKeyHex its 32-byte key, both optional.
	// The default is no encryption; an omitted field with a globally configured key keeps
	// the pre-feature behaviour (AES-256-GCM under that key) so existing deployments do
	// not silently start sending plaintext.
	Encryption       string `json:"encryption,omitempty"`
	EncryptionKeyHex string `json:"encryption_key_hex,omitempty"`

	// EncryptionKeyID names a key already saved through the /api/v1/keys API, as an
	// alternative to EncryptionKeyHex: exactly one of the two may be supplied alongside a
	// concrete cipher. It carries only the id — parseTransferRequest resolves it against
	// SenderKeys and fills in EncryptionKey, so the key material itself never has to cross
	// the wire again once it has been saved once.
	EncryptionKeyID int64 `json:"encryption_key_id,omitempty"`

	// GridWidth and GridHeight override the configured frame geometry for this transfer
	// alone. Zero means the configured default.
	GridWidth  int `json:"grid_width,omitempty"`
	GridHeight int `json:"grid_height,omitempty"`

	// CellPixels overrides the configured cell size for this transfer alone. Zero means
	// the configured default. Unlike quiet zone — a property of the panel and camera —
	// cell size trades off against grid: a caller who wants a bigger grid than the
	// configured cell size lets fit on their panel asks for a smaller cell here, rather
	// than the deployment's default cell size being forced on every transfer.
	CellPixels int `json:"cell_pixels,omitempty"`

	// Resolved by parseTransferRequest; never read from the wire.
	EncryptionID  uint8  `json:"-"`
	EncryptionKey []byte `json:"-"`
}

// TransferResponse is what a caller gets back.
type TransferResponse struct {
	TransmissionID uuid.UUID `json:"transmission_id"`
	FileID         uuid.UUID `json:"file_id"`
	Filename       string    `json:"filename"`
	SizeBytes      int64     `json:"size_bytes"`
	SHA256         string    `json:"sha256"`
	CallbackURL    string    `json:"callback_url,omitempty"`
	Status         string    `json:"status"`

	// Jobs are the pipeline stages queued for this transfer, so a caller can watch the preparation
	// rather than only its outcome.
	Jobs []uuid.UUID `json:"jobs"`
}

// createTransfer accepts a file and a callback URL and starts everything.
//
// The upload is streamed to the object store rather than buffered, because these files do not fit in
// memory — that is the entire reason the platform exists. The hash is computed while the bytes go
// past, so the file is never read twice.
func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Current()
	ctx := r.Context()

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.fail(w, http.StatusBadRequest, "the request must be a multipart form carrying a file and a callback_url", err)
		return
	}

	request, err := s.parseTransferRequest(r, cfg)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	upload, header, err := r.FormFile("file")
	if err != nil {
		s.fail(w, http.StatusBadRequest, "no file was supplied under the \"file\" field", err)
		return
	}
	defer upload.Close()

	if request.Filename == "" {
		// The uploaded name is the fallback, and it is stripped to a bare name first: a multipart
		// filename is caller-controlled and may carry a path.
		request.Filename = filepath.Base(header.Filename)
	}
	if err := protocol.CheckCallbackURL(request.CallbackURL); err != nil {
		s.fail(w, http.StatusBadRequest, "the callback URL is not usable", err)
		return
	}

	fileID := uuid.New()
	key, err := objectstore.Key("files", fileID.String())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not build a storage key", err)
		return
	}

	// Hashed as it is stored, in one pass. The hash is what the receiver verifies the merged file
	// against, so it has to be the hash of exactly the bytes that were stored — computing it from a
	// second read would leave room for the two to differ.
	hasher := sha256.New()
	counter := &countingReader{r: io.TeeReader(io.LimitReader(upload, cfg.Server.MaxUploadBytes+1), hasher)}
	if err := s.objects.Put(ctx, key, counter); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not store the upload", err)
		return
	}
	if counter.n > cfg.Server.MaxUploadBytes {
		_ = s.objects.Delete(ctx, key)
		s.fail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the file exceeds the %d-byte limit", cfg.Server.MaxUploadBytes), nil)
		return
	}

	sum := hasher.Sum(nil)
	file, err := s.store.Files.Create(ctx, store.File{
		ID:          fileID,
		Filename:    request.Filename,
		StoredPath:  key,
		SizeBytes:   counter.n,
		SHA256:      sum,
		ContentType: header.Header.Get("Content-Type"),
	})
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not record the upload", err)
		return
	}

	transmission, err := s.store.Transmissions.Create(ctx, store.Transmission{
		FileID:           file.ID,
		Priority:         request.Priority,
		Encoder:          request.Encoder,
		BitDepth:         request.BitDepth,
		Compression:      request.Compression,
		CompressionLevel: request.Level,
		FECCodec:         request.FECCodec,
		FECDataShards:    request.DataShards,
		FECParityShards:  request.ParityShards,
		GridWidth:        request.GridWidth,
		GridHeight:       request.GridHeight,
		CellPixels:       request.CellPixels,
		QuietZone:        cfg.Optical.QuietZone,
		Encrypted:        request.EncryptionID != protocol.EncryptionNone,
		EncryptionID:     int(request.EncryptionID),
		EncryptionKey:    request.EncryptionKey,
		OriginalSize:     file.SizeBytes,
		CallbackURL:      request.CallbackURL,
	})
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not create the transmission", err)
		return
	}

	if request.CallbackURL != "" {
		// Recorded on the sender as well, even though the receiver is what delivers to it. A caller
		// asks the sender what became of their request, and the sender should be able to answer
		// without anybody reaching across the air gap.
		if _, err := s.store.Callbacks.Record(ctx, store.Callback{
			TransmissionID: &transmission.ID,
			URL:            request.CallbackURL,
			Event:          "file.delivered",
		}); err != nil {
			s.log.Warn("could not record the callback", zap.Error(err))
		}
	}

	queued, err := s.pipeline.Prepare(ctx, transmission.ID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not queue the pipeline", err)
		return
	}
	jobIDs := make([]uuid.UUID, len(queued))
	for i, job := range queued {
		jobIDs[i] = job.ID
	}

	if request.Autostart && s.transmit != nil {
		// Started in the background: preparation takes as long as the file is large, and a caller
		// should not hold a connection open for it.
		go s.transmit(context.WithoutCancel(ctx), transmission.ID)
	}

	s.log.Info("transfer accepted",
		zap.String("transmission", transmission.ID.String()),
		zap.String("file", file.Filename),
		zap.Int64("bytes", file.SizeBytes),
		zap.String("callback", request.CallbackURL))

	s.respond(w, http.StatusAccepted, TransferResponse{
		TransmissionID: transmission.ID,
		FileID:         file.ID,
		Filename:       file.Filename,
		SizeBytes:      file.SizeBytes,
		SHA256:         hex.EncodeToString(sum),
		CallbackURL:    request.CallbackURL,
		Status:         string(transmission.Status),
		Jobs:           jobIDs,
	})
}

// parseTransferRequest reads the form fields and fills in the configured defaults.
//
// Every named codec is checked against its registry here rather than being discovered later. A
// request naming an encoder that does not exist would otherwise be accepted, stored, queued, and fail
// during rendering — after the upload had been read and the caller had been told it was accepted.
func (s *Server) parseTransferRequest(r *http.Request, cfg config.Config) (TransferRequest, error) {
	request := TransferRequest{
		Filename:         strings.TrimSpace(r.FormValue("filename")),
		CallbackURL:      strings.TrimSpace(r.FormValue("callback_url")),
		Encoder:          strings.TrimSpace(r.FormValue("encoder")),
		Compression:      strings.TrimSpace(r.FormValue("compression")),
		FECCodec:         strings.TrimSpace(r.FormValue("fec_codec")),
		Priority:         strings.TrimSpace(r.FormValue("priority")),
		BitDepth:         formInt(r, "bit_depth", cfg.Optical.BitDepth),
		Level:            formInt(r, "level", cfg.Optical.Level),
		DataShards:       formInt(r, "fec_data_shards", cfg.Optical.FEC.DataShards),
		ParityShards:     formInt(r, "fec_parity_shards", cfg.Optical.FEC.ParityShards),
		Autostart:        formBool(r, "autostart", true),
		Encryption:       strings.TrimSpace(r.FormValue("encryption")),
		EncryptionKeyHex: strings.TrimSpace(r.FormValue("encryption_key_hex")),
		GridWidth:        formInt(r, "grid_width", cfg.Optical.GridWidth),
		GridHeight:       formInt(r, "grid_height", cfg.Optical.GridHeight),
		CellPixels:       formInt(r, "cell_pixels", cfg.Optical.CellPixels),
	}

	if request.Encoder == "" {
		request.Encoder = cfg.Optical.Encoder
	}
	if request.Compression == "" {
		request.Compression = cfg.Optical.Compression
	}
	if request.FECCodec == "" {
		request.FECCodec = cfg.Optical.FEC.Codec
	}
	if request.Priority == "" {
		request.Priority = "normal"
	}

	if request.Filename != "" {
		if err := protocol.CheckManifestFilename(request.Filename); err != nil {
			return request, err
		}
	}

	encoder, err := encoding.ByName(request.Encoder)
	if err != nil {
		return request, fmt.Errorf("encoder %q is not one of %s",
			request.Encoder, strings.Join(encoding.Names(), ", "))
	}
	if request.BitDepth != 0 {
		supported := false
		for _, d := range encoder.SupportedBitDepths() {
			if int(d) == request.BitDepth {
				supported = true
			}
		}
		if !supported {
			return request, fmt.Errorf("the %s encoder does not offer bit depth %d",
				request.Encoder, request.BitDepth)
		}
	}
	if _, err := compress.ByName(request.Compression); err != nil {
		return request, fmt.Errorf("compression %q is not one of %s",
			request.Compression, strings.Join(compress.Names(), ", "))
	}
	if request.Level < 0 || request.Level > 9 {
		return request, fmt.Errorf("level %d must be between 0 and 9", request.Level)
	}
	codec, err := fec.ByName(request.FECCodec)
	if err != nil {
		return request, fmt.Errorf("fec_codec %q is not one of %s",
			request.FECCodec, strings.Join(fec.Names(), ", "))
	}
	if err := codec.Validate(request.DataShards, request.ParityShards); err != nil {
		return request, err
	}
	switch request.Priority {
	case "high", "normal", "low":
	default:
		return request, fmt.Errorf("priority %q is not one of high, normal, low", request.Priority)
	}

	// Encryption. An omitted field means "whatever the deployment did before this
	// feature": encrypt under the global key if one is configured. An explicit "none"
	// always means none — and a key alongside it is refused rather than ignored,
	// because a caller who supplied a key believes the transfer is confidential.
	//
	// A key may be supplied two ways — hex directly, or the id of one already saved
	// through /api/v1/keys — and exactly one of the two is accepted alongside a concrete
	// cipher: both together cannot be honoured without silently picking one, and neither
	// is the existing "cipher without a key" refusal below.
	keyHex := request.EncryptionKeyHex
	keyIDRaw := strings.TrimSpace(r.FormValue("encryption_key_id"))
	hasKeyID := keyIDRaw != ""
	var keyID int64
	if hasKeyID {
		id, err := strconv.ParseInt(keyIDRaw, 10, 64)
		if err != nil {
			return request, fmt.Errorf("encryption_key_id %q is not a number", keyIDRaw)
		}
		keyID = id
		request.EncryptionKeyID = id
	}
	hasKey := keyHex != "" || hasKeyID

	switch {
	case request.Encryption == "" && !hasKey && cfg.Optical.EncryptionKeyHex != "":
		request.EncryptionID = protocol.EncryptionAES256GCM
		request.EncryptionKey = cfg.EncryptionKey()
	case request.Encryption == "" && hasKey:
		return request, fmt.Errorf("a key was supplied without an encryption type")
	default:
		id, err := protocol.EncryptionByName(request.Encryption)
		if err != nil {
			return request, err
		}
		if id == protocol.EncryptionNone && hasKey {
			return request, fmt.Errorf("encryption is \"none\" but a key was supplied; refusing to guess which was meant")
		}
		if id != protocol.EncryptionNone {
			switch {
			case keyHex != "" && hasKeyID:
				return request, fmt.Errorf("encryption_key_hex and encryption_key_id are mutually exclusive; supply exactly one")
			case hasKeyID:
				saved, err := s.store.SenderKeys.Get(r.Context(), keyID)
				if err != nil {
					return request, fmt.Errorf("encryption_key_id %d does not name a saved key", keyID)
				}
				request.EncryptionKey = saved.Key
			case keyHex != "":
				key, err := hex.DecodeString(keyHex)
				if err != nil || len(key) != protocol.KeySize {
					return request, fmt.Errorf("encryption %q requires a 64-hex-character key (32 bytes)", request.Encryption)
				}
				request.EncryptionKey = key
			default:
				return request, fmt.Errorf("encryption %q requires a 64-hex-character key (32 bytes)", request.Encryption)
			}
		}
		request.EncryptionID = id
	}

	// Grid. Validated against the request's own cell size, not the configured default:
	// a caller asking for a bigger grid than the default cell size lets fit on their
	// panel supplies a smaller cell_pixels instead, and it is that combination — not
	// the deployment's default — that has to fit. Quiet zone stays global; it is a
	// property of the panel and camera, not of one file.
	layout, err := protocol.NewLayoutQuiet(request.GridWidth, request.GridHeight,
		request.CellPixels, cfg.Optical.QuietZone)
	if err != nil {
		return request, fmt.Errorf("grid %dx%d at %d px/cell: %w",
			request.GridWidth, request.GridHeight, request.CellPixels, err)
	}
	depth := request.BitDepth
	if depth == 0 {
		depth = cfg.Optical.BitDepth
	}
	if _, err := encoder.EstimateCapacity(layout, uint8(depth)); err != nil {
		return request, fmt.Errorf("grid %dx%d cannot carry the %s encoding: %w",
			request.GridWidth, request.GridHeight, request.Encoder, err)
	}
	if layout.ImageWidth() > maxImagePixels || layout.ImageHeight() > maxImagePixels {
		return request, fmt.Errorf("grid %dx%d renders a %d×%d pixel frame, larger than any panel",
			request.GridWidth, request.GridHeight, layout.ImageWidth(), layout.ImageHeight())
	}
	if err := validateGeometryForCamera(request, cfg, uint8(depth)); err != nil {
		return request, err
	}

	return request, nil
}

// validateGeometryForCamera refuses a grid the receiving camera cannot resolve.
//
// Checked here, before a byte is encoded, because the alternative is discovering it afterwards and that has
// now happened twice with the same file: a 128-cell grid against a 1080-pixel capture tops out at 8.2 pixels
// a cell where colour8 needs 10, and the transfer retransmitted chunk 0 eleven times before failing. The
// aiming display meanwhile advised "move closer" at 86% of the view already filled, which is advice that
// cannot be followed — a frame is square, so its width is bounded by the short side of the picture.
//
// A floor rather than a prediction. It assumes the frame fills the camera's view and ignores blur, exposure
// and compression, so a geometry that passes may still fail for other reasons while one that fails cannot
// possibly work. That asymmetry is what makes it safe to refuse on: the check never rejects something that
// would have worked.
//
// Its own function so the cases worth testing — specific geometries against specific captures — can be
// exercised without going through the multipart form, which would test the parser instead.
func validateGeometryForCamera(request TransferRequest, cfg config.Config, depth uint8) error {
	short, long := cfg.Optical.CameraShortSidePixels, cfg.Optical.CameraLongSidePixels
	if long < short {
		// Given in either order, or only one given: a square frame is bounded by the smaller, and the
		// larger is only used to say whether turning the camera would help.
		long = short
	}
	if short <= 0 {
		// No camera resolution configured, which is correct for a file-loopback channel: there is no sensor
		// to be bounded by, and refusing a dense grid there would break a working development path.
		return nil
	}

	assessment := readable.Assess(request.GridWidth, cfg.Optical.QuietZone, depth, short, long)
	if assessment.Readable || assessment.Marginal {
		// Marginal is allowed through, and the first version of this refused it — which turned a slow
		// channel into a blocked one. An operator who has been told a geometry will decode a few frames in
		// a hundred may still have reasons to send it: a small file, a soak test, or simply wanting to see
		// it for themselves. Refusing that is deciding on their behalf, and it stopped an upload outright.
		//
		// Only the hopeless band is refused, where the measurements say no frames decode at all and the
		// transfer would be pure waste.
		return nil
	}
	return fmt.Errorf("%s", assessment.Explain(request.GridWidth, depth))
}

// TransferStatus is what a caller sees when they ask how a transfer is going.
type TransferStatus struct {
	TransmissionID uuid.UUID `json:"transmission_id"`
	Filename       string    `json:"filename"`
	Status         string    `json:"status"`
	CallbackURL    string    `json:"callback_url,omitempty"`

	// SHA256 is the hash the sender computed over the bytes it was given, and the one it declared in the
	// manifest. It is here so a page can put it beside the receiver's computed hash: the two agreeing is the
	// whole claim the platform makes, and it can only be checked if both are visible.
	SHA256 string `json:"sha256"`

	OriginalSize   int64 `json:"original_size"`
	CompressedSize int64 `json:"compressed_size"`
	ChunkCount     int   `json:"chunk_count"`
	ChunkSize      int   `json:"chunk_size"`
	FrameCount     int   `json:"frame_count"`

	AckedChunks int     `json:"acked_chunks"`
	Progress    float64 `json:"progress"`
	Retransmits int     `json:"retransmits"`

	Encoder     string `json:"encoder"`
	Compression string `json:"compression"`
	FECCodec    string `json:"fec_codec"`

	// Encryption is the cipher name, never the key: a status response is read by anyone
	// watching a transfer, and the key must never appear in it.
	Encryption string `json:"encryption"`
	GridWidth  int    `json:"grid_width"`
	GridHeight int    `json:"grid_height"`

	Error string `json:"error,omitempty"`

	// Result is the receiver's verdict, present once it has reported. It is the only part of this
	// that describes what happened on the far side of the gap.
	Result *ResultView `json:"result,omitempty"`
}

// ResultView is the receiver's report, as the API presents it.
type ResultView struct {
	Verified          bool    `json:"verified"`
	SHA256            string  `json:"sha256"`
	Size              uint64  `json:"size"`
	ChunksReceived    uint32  `json:"chunks_received"`
	ChunksRecovered   uint32  `json:"chunks_recovered"`
	FramesCaptured    uint64  `json:"frames_captured"`
	FramesFailed      uint64  `json:"frames_failed"`
	CallbackDelivered bool    `json:"callback_delivered"`
	CallbackStatus    int     `json:"callback_status,omitempty"`
	CallbackError     string  `json:"callback_error,omitempty"`
	Seconds           float64 `json:"seconds"`
	BytesPerSecond    float64 `json:"bytes_per_second"`
	Error             string  `json:"error,omitempty"`
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}

	tx, err := s.store.Transmissions.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "no such transfer", nil)
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the transfer", err)
		return
	}

	file, err := s.store.Files.Get(r.Context(), tx.FileID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusInternalServerError, "could not read the file", err)
		return
	}

	// Migration 004 added encryption_id; every row that predates it defaults to 0 ("none")
	// there regardless of what it actually carries. Such a row's Encrypted flag was set by
	// the single global-key scheme this feature replaced, which only ever meant one cipher,
	// AES-256-GCM — so a row with Encrypted true and EncryptionID still at its zero-value
	// default is reporting a gap in the migration, not an unencrypted transfer, and must say
	// so rather than repeat the default.
	encryptionName := protocol.EncryptionName(uint8(tx.EncryptionID))
	if tx.Encrypted && tx.EncryptionID == 0 {
		encryptionName = protocol.EncryptionName(protocol.EncryptionAES256GCM)
	}

	status := TransferStatus{
		TransmissionID: tx.ID,
		Filename:       file.Filename,
		SHA256:         hex.EncodeToString(file.SHA256),
		Status:         string(tx.Status),
		CallbackURL:    tx.CallbackURL,
		OriginalSize:   tx.OriginalSize,
		CompressedSize: tx.CompressedSize,
		ChunkCount:     tx.ChunkCount,
		ChunkSize:      tx.ChunkSize,
		FrameCount:     tx.FrameCount,
		AckedChunks:    tx.AckedChunks,
		Progress:       tx.Progress(),
		Retransmits:    tx.Retransmits,
		Encoder:        tx.Encoder,
		Compression:    tx.Compression,
		FECCodec:       tx.FECCodec,
		Encryption:     encryptionName,
		GridWidth:      tx.GridWidth,
		GridHeight:     tx.GridHeight,
		Error:          tx.Error,
	}
	if result, ok := s.acks.Result(id); ok {
		status.Result = resultView(result)
	}
	s.respond(w, http.StatusOK, status)
}

func resultView(result protocol.Result) *ResultView {
	return &ResultView{
		Verified:          result.Verified,
		SHA256:            result.SHA256,
		Size:              result.Size,
		ChunksReceived:    result.ChunksReceived,
		ChunksRecovered:   result.ChunksRecovered,
		FramesCaptured:    result.FramesCaptured,
		FramesFailed:      result.FramesFailed,
		CallbackDelivered: result.CallbackDelivered,
		CallbackStatus:    result.CallbackStatus,
		CallbackError:     result.CallbackError,
		Seconds:           result.Duration().Seconds(),
		BytesPerSecond:    result.ThroughputBytesPerSecond(),
		Error:             result.Error,
	}
}

// getResult returns the receiver's verdict, waiting for it if asked.
//
// The wait exists so a caller can hold one request open until the transfer finishes rather than
// polling. It is bounded by a deadline the caller supplies, because a transfer can legitimately take
// hours and an unbounded wait would be indistinguishable from a hang.
func (s *Server) getResult(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}

	if wait := r.URL.Query().Get("wait"); wait != "" {
		timeout, err := time.ParseDuration(wait)
		if err != nil || timeout <= 0 {
			s.fail(w, http.StatusBadRequest, "wait must be a positive duration such as 30s", err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		result, err := s.acks.WaitForResult(ctx, id)
		if err != nil {
			// A timeout is not an error about the transfer, so it is reported as "not yet" rather than
			// as a failure: the transfer is very likely still running.
			s.respond(w, http.StatusAccepted, map[string]any{
				"transmission_id": id,
				"pending":         true,
				"message":         "the receiver has not reported yet",
			})
			return
		}
		s.respond(w, http.StatusOK, resultView(result))
		return
	}

	result, ok := s.acks.Result(id)
	if !ok {
		s.respond(w, http.StatusAccepted, map[string]any{
			"transmission_id": id,
			"pending":         true,
			"message":         "the receiver has not reported yet",
		})
		return
	}
	s.respond(w, http.StatusOK, resultView(result))
}

func (s *Server) listTransfers(w http.ResponseWriter, r *http.Request) {
	var statuses []store.TransmissionStatus
	if raw := r.URL.Query().Get("status"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			statuses = append(statuses, store.TransmissionStatus(strings.TrimSpace(part)))
		}
	}
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)

	transfers, err := s.store.Transmissions.List(r.Context(), statuses, limit, offset)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list transfers", err)
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"transfers": transfers})
}

func (s *Server) listChunks(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}
	chunks, err := s.store.Chunks.List(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list chunks", err)
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"chunks": chunks})
}

func (s *Server) listFrames(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}
	frames, err := s.store.Frames.List(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list frames", err)
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"frames": frames})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}
	list, err := s.jobs.List(r.Context(), jobs.Filter{TransmissionID: &id, Limit: 100})
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list jobs", err)
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"jobs": list})
}

// listProfiles reports what this build can actually do.
//
// It is generated from the registries rather than written out, so it cannot fall out of step with the
// code: a client offering the operator a choice of encodings is offering the ones this binary has.
func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) {
	type encoderView struct {
		ID          uint8   `json:"id"`
		Name        string  `json:"name"`
		Description string  `json:"description"`
		BitDepths   []uint8 `json:"bit_depths"`
		Default     uint8   `json:"default_bit_depth"`
	}
	type codecView struct {
		ID          uint8  `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}

	encoders := make([]encoderView, 0, len(encoding.All()))
	for _, e := range encoding.All() {
		encoders = append(encoders, encoderView{
			ID: e.ID(), Name: e.Name(), Description: e.Description(),
			BitDepths: e.SupportedBitDepths(), Default: e.DefaultBitDepth(),
		})
	}
	compressors := make([]codecView, 0, len(compress.All()))
	for _, c := range compress.All() {
		compressors = append(compressors, codecView{c.ID(), c.Name(), c.Description()})
	}
	codes := make([]codecView, 0, len(fec.All()))
	for _, c := range fec.All() {
		codes = append(codes, codecView{c.ID(), c.Name(), c.Description()})
	}

	cfg := s.cfg.Current()
	s.respond(w, http.StatusOK, map[string]any{
		"protocol_version": protocol.Current,
		"encoders":         encoders,
		"compressors":      compressors,
		"fec_codecs":       codes,
		"defaults": map[string]any{
			"encoder":     cfg.Optical.Encoder,
			"bit_depth":   cfg.Optical.BitDepth,
			"compression": cfg.Optical.Compression,
			"level":       cfg.Optical.Level,
			"fec_codec":   cfg.Optical.FEC.Codec,
			"grid":        fmt.Sprintf("%dx%d", cfg.Optical.GridWidth, cfg.Optical.GridHeight),
			"cell_pixels": cfg.Optical.CellPixels,
			"fps":         cfg.Display.FPS,
			// encryption_configured tells the form whether leaving the encryption field
			// untouched means "plaintext" or "whatever this deployment did before this
			// feature": parseTransferRequest treats an omitted field as "encrypt under the
			// global key" exactly when one is configured. The key itself never appears here —
			// only whether one exists — so a form can match that behaviour without the browser
			// ever holding key material.
			"encryption_configured": cfg.Optical.EncryptionKeyHex != "",
		},
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.respond(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"protocol_version": protocol.Current,
	})
}

// live reports whether the process is running, and nothing else.
//
// It deliberately does not touch the database. A liveness probe that failed when the database was
// briefly unreachable would have the orchestrator restart a process that was working perfectly, which
// makes an outage worse rather than better. That is what readiness is for.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	s.respond(w, http.StatusOK, map[string]any{"status": "alive"})
}

// ready reports whether the process can serve requests, which does depend on the database.
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

// withLogging records every request with a correlation id.
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

		s.log.Info("request",
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

// fail writes an error response.
//
// The message is written for whoever made the request; the underlying error goes to the log instead.
// A caller does not need the database's opinion of what went wrong, and telling them would leak how
// the inside of this service is arranged.
func (s *Server) fail(w http.ResponseWriter, status int, message string, err error) {
	if err != nil {
		s.log.Warn("request failed", zap.Int("status", status), zap.String("message", message), zap.Error(err))
	}
	s.respond(w, status, map[string]any{"error": message})
}

// countingReader counts what passes through it, so an upload's size is known without trusting the
// Content-Length a client supplied.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func formInt(r *http.Request, field string, fallback int) int {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func formBool(r *http.Request, field string, fallback bool) bool {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		return fallback
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return b
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
