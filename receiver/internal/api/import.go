package api

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/opticaltransport/otp/receiver/internal/pipeline"
)

// Import: the receiving half of the sender's frame archive. A zip of frame PNGs — or the single
// composite PNG a one-chunk transfer downloads as — is replayed into the running pipeline
// exactly as though a camera had seen each frame. Acknowledgements, merge, verification and
// delivery all fire as normal, which is what makes this a transport rather than a parser: the
// optical channel and the USB stick end at the same code.

const (
	// maxImportBytes bounds the whole upload: a frame archive, not a data lake.
	maxImportBytes = 512 << 20

	// maxImportEntryBytes bounds one frame inside a zip. It matches maxPostedFrameBytes, the
	// bound on a single frame posted by the browser path — a frame is a frame, wherever it came
	// from.
	maxImportEntryBytes = 16 << 20
)

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
// and a frame archive is small enough (maxImportBytes) that this is not a real cost.
func (s *Server) postImport(w http.ResponseWriter, r *http.Request) {
	if s.ingest == nil {
		s.fail(w, http.StatusConflict, "this receiver is not taking imports", nil)
		return
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
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
		s.fail(w, http.StatusRequestEntityTooLarge, "that is larger than any real frame archive", nil)
		return
	}

	var entries []importEntry
	switch {
	case bytes.HasPrefix(body, []byte("PK")):
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			s.fail(w, http.StatusBadRequest, "not a readable zip", err)
			return
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			if !strings.HasSuffix(strings.ToLower(f.Name), ".png") {
				entries = append(entries, importEntry{Name: f.Name, Skipped: "not a .png"})
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

			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				// A bad entry is reported in its own row rather than failing the whole request —
				// the operator wants to know which frame was bad, and the rest are still worth
				// ingesting.
				entries = append(entries, importEntry{Name: f.Name, Skipped: "not a decodable PNG"})
				continue
			}
			entries = append(entries, s.ingestOne(r.Context(), f.Name, img, data))
		}

	default:
		img, err := png.Decode(bytes.NewReader(body))
		if err != nil {
			s.fail(w, http.StatusUnsupportedMediaType, "the file is neither a zip nor a PNG", err)
			return
		}
		for i, part := range splitComposite(img, s.probe) {
			entries = append(entries, s.ingestOne(r.Context(), fmt.Sprintf("%s#%d", header.Filename, i), part, nil))
		}
	}

	// An empty list rather than null, the same convention the rest of the API follows: a client
	// that has to treat null and [] as the same thing will one day forget to.
	if entries == nil {
		entries = []importEntry{}
	}

	transmissions := map[uuid.UUID]bool{}
	ingested, skipped := 0, 0
	for _, e := range entries {
		if e.Skipped != "" {
			skipped++
			continue
		}
		ingested++
		if e.TransmissionID != nil {
			transmissions[*e.TransmissionID] = true
		}
	}

	s.respond(w, http.StatusOK, map[string]any{
		"entries":       entries,
		"ingested":      ingested,
		"skipped":       skipped,
		"transmissions": len(transmissions),
	})
}

// ingestOne runs one image through the pipeline and reports what happened, whether that is the
// ingest's own verdict or the reason it could not be attempted at all.
func (s *Server) ingestOne(ctx context.Context, name string, img image.Image, raw []byte) importEntry {
	result, err := s.ingest(ctx, img, raw)
	if err != nil {
		return importEntry{Name: name, Skipped: err.Error()}
	}
	return importEntry{Name: name, IngestResult: result}
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
