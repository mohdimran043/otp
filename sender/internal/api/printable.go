package api

import (
	"bytes"
	"errors"
	"image/png"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/pdf"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// getFramePDF serves a transfer's frames as one printable document, a frame to a page.
//
// The zip beside this is for a machine — the receiver's import endpoint replays it into the same pipeline a
// camera feeds. This is for a person: print the sheets, hold them up, point a camera. It is the cheapest way
// to exercise the optical path with no display at all, and the only way to make a capture reproducible, since
// a sheet of paper is the same frame every time where a panel's brightness, refresh and viewing angle are
// not.
//
// Each page carries its frame number and what it holds, because a stack of printed QR codes is otherwise
// indistinguishable and the order matters for reading them back.
func (s *Server) getFramePDF(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "the transfer id is not a UUID", err)
		return
	}
	tx, err := s.store.Transmissions.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "no such transfer", err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read the transfer", err)
		return
	}
	frames, err := s.store.Frames.List(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list the frames", err)
		return
	}
	if tx.FrameCount == 0 || len(frames) < tx.FrameCount {
		s.fail(w, http.StatusConflict,
			"the frames are still being rendered; try again when the transfer is ready", nil)
		return
	}

	// One manifest, as the zip does. The re-emissions exist so a camera joining mid-stream can catch one;
	// a person holding a stack of paper reads them in order and needs it once.
	selected := make([]store.Frame, 0, len(frames))
	manifestTaken := false
	for _, f := range frames {
		if f.IsManifest {
			if manifestTaken {
				continue
			}
			manifestTaken = true
		}
		selected = append(selected, f)
	}

	// Bounded, because a PDF is built whole in memory before any of it can be written — the cross-reference
	// table at the end records where every object began, so nothing can be streamed. A large transfer would
	// otherwise be a request that allocates a gigabyte and a print job nobody wants. The zip streams and has
	// no such limit, which is the honest thing to point at when this refuses.
	const maxPrintablePages = 500
	if len(selected) > maxPrintablePages {
		s.fail(w, http.StatusRequestEntityTooLarge,
			"this transfer is "+strconv.Itoa(len(selected))+" frames, past the "+
				strconv.Itoa(maxPrintablePages)+" a printable document is built for — "+
				"use the frame archive instead, or send a smaller file to test with", nil)
		return
	}

	file, err := s.store.Files.Get(r.Context(), tx.FileID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the file record", err)
		return
	}

	// Built entirely before the first byte is written, so a failure part way through is still a real status
	// rather than a truncated download that opens as a corrupt file.
	doc := pdf.A4()
	for i, f := range selected {
		body, err := objectstore.GetBytes(r.Context(), s.objects, f.StoredPath, maxFrameImageBytes)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "could not read a frame image", err)
			return
		}
		img, err := png.Decode(bytes.NewReader(body))
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "a stored frame will not decode", err)
			return
		}
		doc.AddImage(img, printCaption(file.Filename, f, i+1, len(selected)))
	}

	out := doc.Bytes()

	h := w.Header()
	h.Set("Content-Type", "application/pdf")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(out)))
	h.Set("Content-Disposition",
		`attachment; filename="`+escapeHeaderFilename(file.Filename)+`-frames.pdf"`)
	if _, err := w.Write(out); err != nil {
		s.log.Warn("could not write the printable frames", zap.Error(err))
	}
}

// printCaption labels one sheet.
//
// The sheet number as well as the frame number, because they differ: the manifest re-emissions are dropped,
// so frame 37 can be sheet 34, and an operator counting pages against a frame list would otherwise conclude
// something was missing.
func printCaption(filename string, f store.Frame, sheet, sheets int) string {
	kind := "chunk"
	switch {
	case f.IsManifest:
		kind = "manifest"
	case protocol.Flags(f.Flags).Has(protocol.FlagParity):
		kind = "parity"
	}
	return filename + " — sheet " + strconv.Itoa(sheet) + " of " + strconv.Itoa(sheets) +
		", frame " + strconv.Itoa(f.FrameNumber) + " (" + kind + ")"
}
