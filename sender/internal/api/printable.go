package api

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"strconv"
	"strings"

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

	// One to a page unless asked otherwise, so every document produced before this existed is still the
	// document this produces.
	perPage := 1
	if raw := strings.TrimSpace(r.URL.Query().Get("per_page")); raw != "" {
		perPage, err = strconv.Atoi(raw)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "per_page is not a number", err)
			return
		}
		if _, _, err := sheetArrangement(perPage); err != nil {
			s.fail(w, http.StatusBadRequest, err.Error(), err)
			return
		}
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
	images := make([]image.Image, 0, len(selected))
	for _, f := range selected {
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
		images = append(images, img)
	}

	// The geometry the frames were rendered at, which is what the tiling has to agree with: it places
	// each frame by computing where a lane of this size begins, so a layout that disagreed with the
	// stored images by a pixel would overlap them.
	lane, err := protocol.NewLayoutQuiet(tx.GridWidth, tx.GridHeight, tx.CellPixels, tx.QuietZone)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "this transfer's geometry will not resolve", err)
		return
	}
	sheets, err := composeSheets(images, lane, perPage)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not lay the frames out on sheets", err)
		return
	}

	doc := pdf.A4()
	for i, sheet := range sheets {
		doc.AddImage(sheet, sheetCaption(file.Filename, selected, i, perPage, len(sheets)))
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

// sheetArrangement is how a sheet carrying perPage frames is divided, in columns and rows.
//
// Portrait, which is where this differs from the display's own tiling and why that code could not simply
// be called. A display is wider than it is tall, so protocol.NewLaneLayout puts two lanes side by side.
// A4 is the other way round: two frames side by side on a portrait page are each bounded by half the
// page's *width*, 3.6 inches, where stacking them gives 5.2. Same paper, same two frames, half again the
// cell size, decided entirely by which way round they go.
//
// The counts are enumerated rather than computed because each one is a claim about paper. Three-up has no
// arrangement on a portrait page that is not either mostly margin or wider than it is tall, and a count
// this does not list is refused rather than approximated — a sheet whose geometry nothing has looked at
// is not something to produce at request time.
func sheetArrangement(perPage int) (columns, rows int, err error) {
	switch perPage {
	case 1:
		return 1, 1, nil
	case 2:
		return 1, 2, nil
	case 4:
		return 2, 2, nil
	case 6:
		return 2, 3, nil
	}
	return 0, 0, fmt.Errorf("no sheet arrangement for %d frames to a page; use 1, 2, 4 or 6", perPage)
}

// composeSheets tiles rendered frames onto sheets, perPage to a sheet.
//
// The tiling is the protocol's own, not a layout invented for paper, and that is the point rather than a
// convenience. protocol.LaneLayout is what a tiled display composes with, so a photograph of a sheet
// presents the receiver exactly the picture a photograph of a tiled display does — several frames, each
// with its own fiducials, separated by DefaultLaneGapCells of background — and LocateAll reads it with
// nothing added on either side. Inventing a paper-specific arrangement would have meant a second geometry
// to keep in step with the decoder, for no gain.
//
// The gap is load-bearing and comes from the same place. LocateAll crops around a frame's fiducials and
// that crop reaches ten cells past each centre, so flush frames put each neighbour's fiducials inside the
// other's crop: measured, they read 2/2 from the encoder's own pixels and 0/2 through any camera.
//
// A short final sheet keeps its full size, empty lanes and all. Compose leaves them as background, which
// a receiver reads as an absence rather than as a corrupt frame — and cropping the sheet instead would
// print its frames at a different scale from every other sheet, since the document fits each page's
// image to the page.
func composeSheets(frames []image.Image, lane protocol.Layout, perPage int) ([]image.Image, error) {
	columns, rows, err := sheetArrangement(perPage)
	if err != nil {
		return nil, err
	}

	// One-up is the frame itself. Composing it would wrap it in a single-lane layout of identical size,
	// which is the same picture at the cost of a copy — and this is the path every printed sheet has
	// taken until now, so it stays the one that touches the image least.
	if perPage == 1 {
		return append([]image.Image(nil), frames...), nil
	}

	tiling := protocol.LaneLayout{
		Lane:    lane,
		Columns: columns,
		Rows:    rows,
		Gap:     protocol.DefaultLaneGapCells,
	}

	sheets := make([]image.Image, 0, (len(frames)+perPage-1)/perPage)
	for start := 0; start < len(frames); start += perPage {
		end := min(start+perPage, len(frames))
		sheet, err := tiling.Compose(frames[start:end])
		if err != nil {
			return nil, fmt.Errorf("composing sheet %d: %w", len(sheets)+1, err)
		}
		sheets = append(sheets, sheet)
	}
	return sheets, nil
}

// sheetCaption labels one sheet, however many frames it carries.
//
// A one-up sheet keeps exactly the caption it has always had. Above that the frames are listed rather than
// given as a range: the manifest re-emissions are dropped from the printed set, so the numbers on a sheet
// can skip, and "frames 12–17" would be a claim about six frames when only five are there.
//
// Numbered in the order they are tiled, which is reading order — left to right, then down — so the list
// tells an operator which frame is which square without a diagram beside it.
func sheetCaption(filename string, selected []store.Frame, sheetIndex, perPage, sheets int) string {
	start := sheetIndex * perPage
	end := min(start+perPage, len(selected))
	if start >= end {
		return filename
	}
	on := selected[start:end]

	if perPage == 1 {
		return printCaption(filename, on[0], sheetIndex+1, sheets)
	}

	numbers := make([]string, 0, len(on))
	manifest := false
	for _, f := range on {
		numbers = append(numbers, strconv.Itoa(f.FrameNumber))
		if f.IsManifest {
			manifest = true
		}
	}

	caption := filename + " — sheet " + strconv.Itoa(sheetIndex+1) + " of " + strconv.Itoa(sheets) +
		", frames " + strings.Join(numbers, ", ") + " (reading order)"
	if manifest {
		// Called out because it is the frame the whole transfer waits on: without it the receiver has
		// the bytes and does not know the filename, the size, or the hash to check them against.
		caption += " — includes the manifest"
	}
	return caption
}

// printCaption labels a one-up sheet.
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
