package api

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"regexp"
	"strconv"
)

// Pulling the frames back out of a PDF.
//
// The sender can hand out a transfer's frames as a printable document, and an operator who prints it may
// well have the PDF itself to hand — or a scanner that produced one from the sheets. Asking them to convert
// it to images first is asking them to do by hand what this can do exactly once.
//
// The images are extracted rather than the pages rasterised, and that distinction is the whole design. A
// rasteriser means embedding a PDF renderer — a large dependency with a large attack surface, parsing
// untrusted files — and it would produce a *rendering* of the frame at whatever resolution was chosen, when
// the original pixels are sitting in the file already. Every frame in these documents is one image object;
// pulling it out gives the decoder the bytes the sender encoded, not a picture of them.
//
// What it does not handle is stated plainly rather than discovered: a PDF whose pages are vector art, or
// whose images use a filter beyond the two below, yields nothing and says so. That is the honest failure —
// the alternative is a renderer, and this endpoint is not worth one.

// pdfImageLimit bounds how many images are pulled from one document, matching the printable export's own
// page limit. A file claiming more is either not one of ours or is not something to unpack in memory.
const pdfImageLimit = 500

// ErrNoPDFImages means the document parsed but held nothing this can read.
var ErrNoPDFImages = errors.New("no frames could be read from this PDF")

// looksLikePDF reports whether a body begins as a PDF does.
func looksLikePDF(body []byte) bool {
	return bytes.HasPrefix(body, []byte("%PDF-"))
}

// pdfObject is one image object found in the file: its dictionary, and the raw stream beside it.
var pdfStreamRe = regexp.MustCompile(`(?s)<<(.*?)>>\s*stream\r?\n`)

// imagesFromPDF returns every embedded image the document carries, in file order.
//
// Deliberately forgiving about the parts of PDF it does not need. A conforming reader resolves the
// cross-reference table, follows object references, and honours object streams; this walks the file for
// image streams and reads the ones it understands. That is enough for a document whose every page is one
// picture, which is what both a printable export and a scanner produce, and it cannot be led astray by a
// cross-reference table that disagrees with the body — it never consults one.
func imagesFromPDF(body []byte) ([]image.Image, error) {
	var out []image.Image

	for offset := 0; offset < len(body) && len(out) < pdfImageLimit; {
		loc := pdfStreamRe.FindSubmatchIndex(body[offset:])
		if loc == nil {
			break
		}
		dict := string(body[offset+loc[2] : offset+loc[3]])
		streamStart := offset + loc[1]

		end := bytes.Index(body[streamStart:], []byte("endstream"))
		if end < 0 {
			break
		}
		payload := body[streamStart : streamStart+end]
		offset = streamStart + end

		// Only image objects. A page's content stream matches the same shape and is not a picture.
		if !bytes.Contains([]byte(dict), []byte("/Image")) {
			continue
		}

		img, err := decodePDFImage(dict, payload)
		if err != nil || img == nil {
			// One unreadable image does not spoil the document: a scanner may embed a thumbnail or a
			// logo in a filter this does not know, and the frames beside it are still worth having.
			continue
		}
		out = append(out, img)
	}

	if len(out) == 0 {
		return nil, ErrNoPDFImages
	}
	return out, nil
}

// decodePDFImage turns one image object into a picture, for the two filters worth supporting.
func decodePDFImage(dict string, payload []byte) (image.Image, error) {
	width := pdfInt(dict, "Width")
	height := pdfInt(dict, "Height")
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("image has no size")
	}
	if err := checkImageDimensions(width, height); err != nil {
		return nil, err
	}

	switch {
	// A JPEG, stored as-is. This is what a scanner writes, and what a phone's "save as PDF" produces —
	// the bytes are a complete JPEG file and the standard library reads them unchanged.
	case bytes.Contains([]byte(dict), []byte("/DCTDecode")):
		img, _, err := image.Decode(bytes.NewReader(payload))
		return img, err

	// Deflated raw samples, which is what this project's own printable export writes: 8-bit RGB, one
	// byte a channel, no predictor. Anything else — a palette, 16-bit samples, a PNG predictor — is left
	// to the caller's "could not read" rather than half-decoded into a picture that looks plausible.
	case bytes.Contains([]byte(dict), []byte("/FlateDecode")):
		if !bytes.Contains([]byte(dict), []byte("/DeviceRGB")) || pdfInt(dict, "BitsPerComponent") != 8 {
			return nil, fmt.Errorf("unsupported raw image format")
		}
		zr, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		defer zr.Close()

		raw, err := io.ReadAll(io.LimitReader(zr, int64(width)*int64(height)*3+1))
		if err != nil {
			return nil, err
		}
		if len(raw) < width*height*3 {
			return nil, fmt.Errorf("image is short: %d bytes for %dx%d", len(raw), width, height)
		}

		img := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := range height {
			for x := range width {
				i := (y*width + x) * 3
				img.SetRGBA(x, y, color.RGBA{R: raw[i], G: raw[i+1], B: raw[i+2], A: 255})
			}
		}
		return img, nil
	}
	return nil, fmt.Errorf("unsupported image filter")
}

// pdfInt reads an integer entry from an object dictionary.
func pdfInt(dict, key string) int {
	re := regexp.MustCompile(`/` + key + `\s+(\d+)`)
	m := re.FindStringSubmatch(dict)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}
