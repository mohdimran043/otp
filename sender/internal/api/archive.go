package api

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// getFrameArchive serves every frame of a transfer as one downloadable artifact.
//
// This is the sneakernet path: the frames that would have crossed the optical channel
// cross on a USB stick instead, and the receiver's import endpoint replays them into
// the same pipeline a camera feeds. The unit is frames rather than chunks because a
// receiver holding every chunk and no manifest still has nothing it can verify or name.
//
// Two shapes. Several images make a zip. Exactly two — one manifest, one data frame,
// the smallest complete transfer — make a single composite PNG, the manifest stacked
// above the data frame, because one image is the artifact an operator expects from a
// one-chunk file and the pair is only decodable together.
func (s *Server) getFrameArchive(w http.ResponseWriter, r *http.Request) {
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
	// tx.FrameCount == 0 covers both "nothing rendered yet" and "not even prepared"; a
	// transmission whose frame count is set always has at least one frame row by the time
	// len(frames) reaches it, so a separate len(frames) == 0 clause would never fire on its
	// own.
	if tx.FrameCount == 0 || len(frames) < tx.FrameCount {
		s.fail(w, http.StatusConflict, "the frames are still being rendered; try again when the transfer is ready", nil)
		return
	}

	// The interleaved manifest re-emissions are byte-identical payloads for a receiver
	// joining mid-stream; a file needs the manifest once.
	var selected []store.Frame
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

	file, err := s.store.Files.Get(r.Context(), tx.FileID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the file record", err)
		return
	}

	if len(selected) == 2 {
		s.serveCompositeFrames(w, r, selected, file.Filename)
		return
	}

	// The first frame is read before any header is written, on purpose: WriteHeader has not
	// been called yet, so a failure here can still answer with a real status instead of the
	// 200 net/http would send by default the moment anything is written to w. Once the zip
	// writer has produced its first byte, that option is gone — a failure on frame two onward
	// really is the "short zip that fails to open" case the loop below logs and truncates on.
	first, err := objectstore.GetBytes(r.Context(), s.objects, selected[0].StoredPath, maxFrameImageBytes)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read a frame image", err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", `attachment; filename="`+escapeHeaderFilename(file.Filename)+`-frames.zip"`)
	zw := zip.NewWriter(w)
	if !writeZipEntry(zw, selected[0], first) {
		return
	}
	for _, f := range selected[1:] {
		body, err := objectstore.GetBytes(r.Context(), s.objects, f.StoredPath, maxFrameImageBytes)
		if err != nil {
			// The response has already begun streaming, so an error here can only be logged
			// and the archive truncated; the client sees a short zip that fails to open.
			s.log.Error("frame archive read failed", zap.String("path", f.StoredPath), zap.Error(err))
			return
		}
		if !writeZipEntry(zw, f, body) {
			return
		}
	}
	if err := zw.Close(); err != nil {
		s.log.Warn("frame archive close failed", zap.Error(err))
	}
}

// writeZipEntry adds one frame to the archive, named the way the receiver's importer expects
// it. It reports whether the write succeeded; a failure here is a write to the response body
// that has already started, so the caller can only stop rather than report an error.
func writeZipEntry(zw *zip.Writer, f store.Frame, body []byte) bool {
	name := fmt.Sprintf("frame-%08d.png", f.FrameNumber)
	if f.IsManifest {
		name = fmt.Sprintf("frame-%08d-manifest.png", f.FrameNumber)
	}
	entry, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err != nil {
		return false
	}
	_, err = entry.Write(body)
	return err == nil
}

// serveCompositeFrames stacks the manifest frame above the single data frame in one PNG.
func (s *Server) serveCompositeFrames(w http.ResponseWriter, r *http.Request, frames []store.Frame, filename string) {
	images := make([]image.Image, 0, 2)
	// The manifest goes on top whatever order the rows came in, so the receiver reads
	// the transmission's identity before its data — and so the artifact is deterministic.
	if !frames[0].IsManifest {
		frames[0], frames[1] = frames[1], frames[0]
	}
	for _, f := range frames {
		body, err := objectstore.GetBytes(r.Context(), s.objects, f.StoredPath, maxFrameImageBytes)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "could not read a frame image", err)
			return
		}
		img, err := png.Decode(bytes.NewReader(body))
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "a stored frame is not a PNG", err)
			return
		}
		images = append(images, img)
	}

	top, bottom := images[0].Bounds(), images[1].Bounds()
	if top.Dy() != bottom.Dy() {
		// The receiver splits the composite into two equal-height bands rather than reading
		// a boundary out of the image, so a mismatch here would hand it a file it decodes
		// wrongly rather than one it refuses. Refusing here is the same failure, but honest
		// about where it happened.
		s.fail(w, http.StatusInternalServerError, "the manifest and data frames are not the same size", nil)
		return
	}
	width := max(top.Dx(), bottom.Dx())
	composite := image.NewRGBA(image.Rect(0, 0, width, top.Dy()+bottom.Dy()))
	draw.Draw(composite, image.Rect(0, 0, top.Dx(), top.Dy()), images[0], top.Min, draw.Src)
	draw.Draw(composite, image.Rect(0, top.Dy(), bottom.Dx(), top.Dy()+bottom.Dy()), images[1], bottom.Min, draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, composite); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not encode the composite", err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+escapeHeaderFilename(filename)+`-frames.png"`)
	s.writePNG(w, buf.Bytes(), map[string]string{"Cache-Control": "no-store"})
}
