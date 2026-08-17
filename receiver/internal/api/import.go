package api

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"

	// Registered for their side effects, so image.Decode recognises what a camera actually writes. A
	// photograph of a printed frame is JPEG on every phone made, and PNG-only was refusing exactly the
	// case this endpoint exists to serve.
	_ "image/gif"
	_ "image/jpeg"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/opticaltransport/otp/receiver/internal/pipeline"
)

// Import: the receiving half of the sender's frame archive. A zip of frame PNGs — or the single
// composite PNG a one-chunk transfer downloads as — is replayed into the running pipeline
// exactly as though a camera had seen each frame. Acknowledgements, merge, verification and
// delivery all fire as normal, which is what makes this a transport rather than a parser: the
// optical channel and the USB stick end at the same code.
//
// Every entry in a zip goes through the pipeline's single-threaded applier synchronously, at
// roughly the cost of one captured frame — hundreds of milliseconds at a dense geometry. That
// makes an import request itself a thing worth bounding carefully: an unbounded or oversized
// one would tie up live capture, and this handler's own WriteTimeout, for minutes. maxImportBytes,
// maxImportEntries, the per-entry ctx check, and the systemic-failure abort below are all in
// service of that: fail fast and cheaply, rather than grinding through an archive nobody is
// waiting on any more.

const (
	// maxImportBytes bounds the whole upload: a frame archive, not a data lake.
	maxImportBytes = 512 << 20

	// maxImportEntryBytes bounds one frame inside a zip. It matches maxPostedFrameBytes, the
	// bound on a single frame posted by the browser path — a frame is a frame, wherever it came
	// from.
	maxImportEntryBytes = 16 << 20

	// maxImportEntries bounds how many frames one request may carry. Each one is ingested
	// synchronously through the single applier, so an archive with no cap on entry count could
	// keep that applier — and this handler's own response — busy for minutes even while staying
	// under maxImportBytes (thousands of tiny PNGs are still small in aggregate). 4096 is far
	// more than any transfer this platform is sized for produces in one archive; a real
	// transfer that large is already something an operator would split into more than one
	// import, not something this handler should try to swallow whole.
	maxImportEntries = 4096
)

// looksLikeImage reports whether a zip entry is worth handing to the decoder, by extension.
//
// The extension rather than the bytes, because the alternative is to decode every entry in the
// archive to find out — and an archive is allowed to carry a README, a checksum file, or a
// directory of notes beside its frames. Guessing wrong here is cheap in one direction only: an
// entry that passes this and then fails to decode is reported as its own skipped row, where one
// rejected on its name is never looked at again.
//
// The set matches what the single-image branch below accepts, and for the same reason: a
// photographed sheet is a JPEG. An operator who scanned a stack of prints has a folder of JPEGs,
// and zipping that folder is the obvious way to import the lot — refusing them by extension would
// leave the bulk path PNG-only while the single-file path had already moved on.
func looksLikeImage(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return true
	}
	return false
}

// importEntry is one image the importer looked at, whether it came from a zip entry or a half
// of a split composite.
type importEntry struct {
	Name string `json:"name"`
	pipeline.IngestResult
	Skipped string `json:"skipped,omitempty"`
}

// postImport replays an uploaded frame archive into the live pipeline.
//
// The body is read into memory rather than streamed, because a zip's central directory is at
// the end of the file — there is no way to validate or iterate one without seeing all of it —
// and a frame archive is small enough (maxImportBytes) that this is not a real cost, provided
// that bound is enforced before anything spools the body rather than after.
func (s *Server) postImport(w http.ResponseWriter, r *http.Request) {
	if s.ingest == nil {
		s.fail(w, http.StatusConflict, "this receiver is not taking imports", nil)
		return
	}

	// This endpoint is unauthenticated and reads up to maxImportBytes into memory before it does
	// anything else, so it is cheap for one caller to launch several requests at once and multiply
	// that cost. TryLock refuses a second import outright rather than making it wait: the single
	// applier every entry goes through serializes the real work anyway, so queuing here would only
	// hold a whole extra body in memory for the time it takes the first import to finish.
	if !s.importMu.TryLock() {
		s.fail(w, http.StatusConflict, "an import is already running; wait for it to finish and retry", nil)
		return
	}
	defer s.importMu.Unlock()

	// Wrapped before ParseMultipartForm reads a single byte. Without this, ParseMultipartForm
	// spools whatever the request carries — past its declared in-memory threshold, straight to a
	// temporary file, with no bound of its own — before this handler ever gets to look at the
	// "file" field's size. MaxBytesReader makes the read itself fail once the request has
	// carried more than maxImportBytes, so an oversized upload is refused while it is arriving,
	// not after it has been fully written to disk.
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.fail(w, http.StatusRequestEntityTooLarge, "that is larger than any real frame archive", err)
			return
		}
		s.fail(w, http.StatusBadRequest, "could not read the upload", err)
		return
	}
	upload, header, err := r.FormFile("file")
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the request must carry the archive in a \"file\" field", err)
		return
	}
	defer upload.Close()

	body, err := io.ReadAll(io.LimitReader(upload, maxImportBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "could not read the upload", err)
		return
	}
	if len(body) == 0 {
		s.fail(w, http.StatusBadRequest, "the upload carried no bytes", nil)
		return
	}
	if len(body) > maxImportBytes {
		// Belt and braces alongside the MaxBytesReader above, which bounds the request as a
		// whole rather than this one field specifically.
		s.fail(w, http.StatusRequestEntityTooLarge, "that is larger than any real frame archive", nil)
		return
	}

	var entries []importEntry
	truncated := false

	switch {
	case bytes.HasPrefix(body, []byte("PK")):
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			s.fail(w, http.StatusBadRequest, "not a readable zip", err)
			return
		}
		if len(zr.File) > maxImportEntries {
			s.fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"the archive carries %d entries, more than the %d this endpoint will import in one request",
				len(zr.File), maxImportEntries), nil)
			return
		}

		for _, f := range zr.File {
			if r.Context().Err() != nil {
				// The caller is gone, or this request's own deadline has passed. Every entry
				// ingested from here on is work nobody is waiting for, and it is work on the
				// single applier every captured frame also depends on — so this stops rather
				// than finishing the archive on principle.
				truncated = true
				break
			}
			if f.FileInfo().IsDir() {
				continue
			}
			if !looksLikeImage(f.Name) {
				entries = append(entries, importEntry{Name: f.Name, Skipped: "not an image"})
				continue
			}

			rc, err := f.Open()
			if err != nil {
				entries = append(entries, importEntry{Name: f.Name, Skipped: "could not open: " + err.Error()})
				continue
			}
			data, err := io.ReadAll(io.LimitReader(rc, maxImportEntryBytes+1))
			rc.Close()
			switch {
			case err != nil:
				entries = append(entries, importEntry{Name: f.Name, Skipped: "could not read: " + err.Error()})
				continue
			case len(data) > maxImportEntryBytes:
				entries = append(entries, importEntry{Name: f.Name, Skipped: "larger than a frame can be"})
				continue
			}

			// Checked from the header alone, before any pixel data is read: see maxDecodedPixels.
			// A zip entry can declare whatever dimensions it likes regardless of how few bytes
			// actually follow.
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				entries = append(entries, importEntry{Name: f.Name, Skipped: "not a decodable image"})
				continue
			}
			if err := checkImageDimensions(cfg.Width, cfg.Height); err != nil {
				entries = append(entries, importEntry{Name: f.Name, Skipped: err.Error()})
				continue
			}

			img, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				// A bad entry is reported in its own row rather than failing the whole request —
				// the operator wants to know which frame was bad, and the rest are still worth
				// ingesting.
				entries = append(entries, importEntry{Name: f.Name, Skipped: "not a decodable image"})
				continue
			}

			entry, ingestErr := s.ingestOne(r.Context(), f.Name, img, data)
			entries = append(entries, entry)
			if errors.Is(ingestErr, pipeline.ErrNotRunning) {
				// Not a property of this one frame: every remaining entry would fail the same
				// way, for the same reason, so grinding through the rest would only turn one
				// outage into hundreds of identical, misleading skips. Abort outright rather than
				// answer 200 having "skipped" an archive that never had a chance.
				s.fail(w, http.StatusServiceUnavailable,
					"this receiver stopped accepting frames partway through the import", ingestErr)
				return
			}
		}

	default:
		// Any still image the standard library can read, not only PNG.
		//
		// PNG-only was wrong for the case this endpoint most obviously serves: photographing a printed
		// frame. Every phone writes JPEG, so an operator holding a sheet in front of a camera got "the
		// file is neither a zip nor a PNG" for doing exactly what the feature is for. The rendered frames
		// this receiver hands out are PNG, and a picture *of* one is not.
		//
		// The size is checked from the header alone, before any pixel data is read: see maxDecodedPixels.
		// image.DecodeConfig dispatches on the registered formats, so this covers whatever the imports at
		// the top of this file bring in.
		cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
		if err != nil {
			s.fail(w, http.StatusUnsupportedMediaType,
				"the file is not a zip or an image this receiver can read (PNG, JPEG or GIF)", err)
			return
		}
		if err := checkImageDimensions(cfg.Width, cfg.Height); err != nil {
			s.fail(w, http.StatusRequestEntityTooLarge, err.Error(), err)
			return
		}
		img, _, err := image.Decode(bytes.NewReader(body))
		if err != nil {
			s.fail(w, http.StatusUnsupportedMediaType,
				"the file is not a zip or an image this receiver can read (PNG, JPEG or GIF)", err)
			return
		}
		for i, part := range splitComposite(img, s.probe) {
			entry, ingestErr := s.ingestOne(r.Context(), fmt.Sprintf("%s#%d", header.Filename, i), part, nil)
			entries = append(entries, entry)
			if errors.Is(ingestErr, pipeline.ErrNotRunning) {
				s.fail(w, http.StatusServiceUnavailable,
					"this receiver stopped accepting frames partway through the import", ingestErr)
				return
			}
		}
	}

	// An empty list rather than null, the same convention the rest of the API follows: a client
	// that has to treat null and [] as the same thing will one day forget to.
	if entries == nil {
		entries = []importEntry{}
	}

	transmissionSet := map[uuid.UUID]bool{}
	ingested, skipped := 0, 0
	for _, e := range entries {
		if e.Skipped != "" {
			skipped++
			continue
		}
		ingested++
		if e.TransmissionID != nil {
			transmissionSet[*e.TransmissionID] = true
		}
	}

	// The transmission ids themselves, not merely a count: Task 9's UI navigates straight to
	// each one once an import finishes, and a count alone would give it nothing to link to.
	// Sorted for a stable, diffable response.
	transmissions := make([]string, 0, len(transmissionSet))
	for id := range transmissionSet {
		transmissions = append(transmissions, id.String())
	}
	sort.Strings(transmissions)

	response := map[string]any{
		"entries":       entries,
		"ingested":      ingested,
		"skipped":       skipped,
		"transmissions": transmissions,
	}
	if truncated {
		// Present only when it happened, rather than always false, so a client can treat its
		// absence as "no" without a schema that grows a field nobody sets.
		response["truncated"] = true
	}
	s.respond(w, http.StatusOK, response)
}

// ingestOne runs one image through the pipeline and reports what happened, whether that is the
// ingest's own verdict or the reason it could not be attempted at all. The error is returned
// alongside the entry — not folded away into the entry's Skipped string — so the caller can
// tell a systemic failure (errors.Is(err, pipeline.ErrNotRunning)) from an ordinary per-frame
// one and decide whether to keep going.
func (s *Server) ingestOne(ctx context.Context, name string, img image.Image, raw []byte) (importEntry, error) {
	result, err := s.ingest(ctx, img, raw)
	if err != nil {
		return importEntry{Name: name, Skipped: err.Error()}, err
	}
	return importEntry{Name: name, IngestResult: result}, nil
}

// splitComposite returns the frames inside one uploaded image.
//
// The sender's single-chunk download stacks two equal-height frames vertically. Halving is
// tried first and kept only if BOTH halves decode as frames — a lone frame cut in half decodes
// as neither, so it falls through to being taken whole. Decode-as-probe is affordable here:
// imports are rare operator actions, not the capture path.
//
// probe is injected rather than reaching for a decoder directly, so this function has no
// dependency on how decoding works and is unit-testable with a fake.
func splitComposite(img image.Image, probe func(image.Image) bool) []image.Image {
	whole := []image.Image{img}
	if probe == nil {
		return whole
	}

	bounds := img.Bounds()
	height := bounds.Dy()
	if height < 2 || height%2 != 0 {
		return whole
	}

	half := height / 2
	top := subImage(img, image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, bounds.Min.Y+half))
	bottom := subImage(img, image.Rect(bounds.Min.X, bounds.Min.Y+half, bounds.Max.X, bounds.Max.Y))
	if probe(top) && probe(bottom) {
		return []image.Image{top, bottom}
	}
	return whole
}

// subImage crops an image, preferring the concrete type's own SubImage when the result would
// still start at the origin, and falling back to a pixel copy otherwise.
//
// The fallback is not only for types that lack SubImage. protocol.Locate resolves a grid's
// geometry against a zero-based sample of the image and then reads header, payload and footer
// bands back out of the original image at those same coordinates — a path that assumes
// Bounds().Min is (0,0) and does not hold up against an image that starts elsewhere. A
// SubImage of the bottom half of a stacked composite has exactly that shape: it shares the
// parent's pixels but its Bounds().Min.Y is the split point, not zero. Decoding it directly
// through that path silently samples the wrong rows and fails — verified by hand while this
// endpoint was built, since it is exactly the composite import is for. Copying into a fresh
// image at (0,0) costs a pass over a few hundred kilobytes for an operator-driven import and
// sidesteps the whole class of bug; the zero-copy path is kept for the one crop that is always
// safe, the one already anchored at the origin.
//
// pipeline.Decodable normalises the same way internally, as a second line of defence for any
// caller that reaches it directly — but the images built here also go on to real Ingest calls,
// which do not route through Decodable, so this copy is load-bearing on its own, not merely
// belt-and-braces.
func subImage(img image.Image, rect image.Rectangle) image.Image {
	if rect.Min.X == 0 && rect.Min.Y == 0 {
		if si, ok := img.(interface {
			SubImage(image.Rectangle) image.Image
		}); ok {
			return si.SubImage(rect)
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), img, rect.Min, draw.Src)
	return dst
}
