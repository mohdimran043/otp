// Package pdf writes a printable document of frame images, one to a page.
//
// It exists so an operator can put the frames on paper and read them with a camera by hand, which is the
// cheapest way to test the optical path without standing up a display: print, hold up, point. It is also
// what makes a failure reproducible — a sheet of paper is the same frame every time, where a panel's
// brightness, refresh and viewing angle are not.
//
// Written here rather than taken as a dependency. A PDF carrying nothing but raster images and a caption is
// a few hundred lines of well-specified format, and the things that matter for this particular document are
// exactly the things a general-purpose library makes hard to be sure of: that the image is not interpolated
// when scaled, that a cell lands on a whole number of device pixels where it can, and that nothing is
// resampled between here and the printer. A blurred QR code prints as an unreadable one.
package pdf

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image"
	"strings"
)

// Page sizes in PostScript points, which is what a PDF measures in: 72 to the inch.
const (
	// A4Width and A4Height are 210 x 297 mm.
	A4Width, A4Height = 595.28, 841.89

	// LetterWidth and LetterHeight are 8.5 x 11 inches.
	LetterWidth, LetterHeight = 612.0, 792.0
)

// margin is the space left around a frame, in points. Half an inch, which is inside what every desktop
// printer can reach — a frame clipped by an unprintable margin is a frame missing a fiducial.
const margin = 36.0

// captionHeight is the strip reserved under each frame for its label.
const captionHeight = 24.0

// Document accumulates pages.
type Document struct {
	width, height float64
	pages         []page
}

type page struct {
	// rgb is the image's pixels, already flattened to 8-bit RGB, and w/h its size.
	rgb  []byte
	w, h int

	caption string
}

// New returns an empty document at the given page size.
func New(width, height float64) *Document {
	return &Document{width: width, height: height}
}

// A4 returns an empty A4 document.
func A4() *Document { return New(A4Width, A4Height) }

// AddImage appends a page carrying one image, captioned.
//
// The image is flattened to 8-bit RGB here rather than at write time so that a caller handing over a decoded
// PNG does not have to keep it alive until the document is written — a transfer can run to thousands of
// frames, and holding every decoded bitmap would be a bitmap-sized leak per page.
func (d *Document) AddImage(img image.Image, caption string) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return
	}

	rgb := make([]byte, 0, w*h*3)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			// The 16-bit values image.Color reports are scaled down rather than truncated, so a frame's
			// blacks and whites stay at 0 and 255 instead of drifting a level and costing contrast the
			// decoder is measuring against.
			r, g, bl, _ := img.At(x, y).RGBA()
			rgb = append(rgb, byte(r>>8), byte(g>>8), byte(bl>>8))
		}
	}
	d.pages = append(d.pages, page{rgb: rgb, w: w, h: h, caption: caption})
}

// Pages is how many pages the document holds.
func (d *Document) Pages() int { return len(d.pages) }

// Bytes renders the document.
//
// A PDF is a set of numbered objects followed by a table of where each one starts, so the offsets are
// recorded as the body is written rather than computed afterwards: a cross-reference table that disagrees
// with the body by one byte produces a file every reader rejects, and it is not visible by inspection.
func (d *Document) Bytes() []byte {
	var out bytes.Buffer
	offsets := []int{0} // object 0 is the free-list head and is never written

	// object appends one numbered object and remembers where it began.
	object := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets)-1, body)
	}
	stream := func(dict, payload string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n<<%s/Length %d>>\nstream\n%s\nendstream\nendobj\n",
			len(offsets)-1, dict, len(payload), payload)
	}

	out.WriteString("%PDF-1.4\n")
	// A comment of high bytes, which is what tells a transfer protocol the file is binary rather than text.
	out.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})

	// 1: catalog, 2: page tree, 3: the caption font. The page objects follow, and their numbers are known
	// in advance because each page contributes exactly three: the page, its content stream, and its image.
	const firstPage = 4
	kids := make([]string, 0, len(d.pages))
	for i := range d.pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", firstPage+i*3))
	}

	object(fmt.Sprintf("<</Type/Catalog/Pages 2 0 R>>"))
	object(fmt.Sprintf("<</Type/Pages/Kids[%s]/Count %d>>", strings.Join(kids, " "), len(d.pages)))
	// Helvetica is one of the fourteen faces every reader carries, so the caption needs no embedded font.
	object("<</Type/Font/Subtype/Type1/BaseFont/Helvetica/Encoding/WinAnsiEncoding>>")

	for i, p := range d.pages {
		pageNum := firstPage + i*3
		contentNum := pageNum + 1
		imageNum := pageNum + 2

		x, y, w, h := d.fit(p.w, p.h)

		var content bytes.Buffer
		// q/Q save and restore, so the image's transform does not leak into the caption. The cm matrix
		// places and scales the unit image square: width, 0, 0, height, x, y.
		fmt.Fprintf(&content, "q\n%.2f 0 0 %.2f %.2f %.2f cm\n/Im0 Do\nQ\n", w, h, x, y)
		if p.caption != "" {
			fmt.Fprintf(&content, "BT\n/F1 9 Tf\n%.2f %.2f Td\n(%s) Tj\nET\n",
				margin, margin-12, escapeText(p.caption))
		}

		object(fmt.Sprintf(
			"<</Type/Page/Parent 2 0 R/MediaBox[0 0 %.2f %.2f]"+
				"/Resources<</XObject<</Im0 %d 0 R>>/Font<</F1 3 0 R>>>>/Contents %d 0 R>>",
			d.width, d.height, imageNum, contentNum))
		stream("", content.String())

		// /Interpolate is deliberately absent, which means false.
		//
		// It is the single most important flag in this file. Turned on, a reader smooths the image as it
		// scales — which is exactly right for a photograph and ruins a QR code, because a cell's edge is
		// the thing being measured and smoothing spreads each cell into its neighbours. A printed frame
		// with interpolation on is soft at every boundary and reads far worse than the same frame printed
		// hard, at any size.
		stream(fmt.Sprintf(
			"/Type/XObject/Subtype/Image/Width %d/Height %d"+
				"/ColorSpace/DeviceRGB/BitsPerComponent 8/Filter/FlateDecode",
			p.w, p.h), deflate(p.rgb))
	}

	// The cross-reference table, then the trailer pointing at it.
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(offsets))
	out.WriteString("0000000000 65535 f \n")
	for _, at := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", at)
	}
	fmt.Fprintf(&out, "trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)

	return out.Bytes()
}

// fit places an image on the page: as large as the margins allow, aspect preserved, centred.
//
// The caption's strip is taken off the height before fitting rather than drawn over the image, because a
// label across the bottom of a frame covers cells the decoder needs.
func (d *Document) fit(imgW, imgH int) (x, y, w, h float64) {
	availW := d.width - 2*margin
	availH := d.height - 2*margin - captionHeight

	scale := min(availW/float64(imgW), availH/float64(imgH))
	w, h = float64(imgW)*scale, float64(imgH)*scale

	x = (d.width - w) / 2
	// Sitting above the caption strip rather than centred in the whole page, so every sheet places its
	// frame identically — a stack of prints that shifts down the page is harder to photograph in sequence.
	y = margin + captionHeight + (availH-h)/2
	return x, y, w, h
}

// deflate compresses a stream, which is what FlateDecode expects.
func deflate(b []byte) string {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	// A write to a bytes.Buffer cannot fail, and neither can closing the writer over one.
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.String()
}

// escapeText makes a string safe inside a PDF literal and encodable by the font.
//
// Two jobs, and the second is the one that is easy to miss. Parentheses delimit a literal and a backslash
// escapes, so a filename like "report (final).pdf" would close the string early and leave the rest as
// syntax — a document that fails to open, from an entirely ordinary filename.
//
// The encoding is the other half. The caption is set in Helvetica with WinAnsiEncoding, which is a
// single-byte encoding, so a UTF-8 string written through verbatim renders as its individual bytes: an em
// dash arrives as three characters of mojibake, which is exactly what the first version of this did. Runes
// that WinAnsi has are written as their byte; runes it does not — and a filename may be in any script — are
// written as a question mark, because a caption is a label for a human holding a sheet of paper and a
// missing character is better than a corrupt document or an embedded font.
func escapeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '(', ')':
			b.WriteByte('\\')
			b.WriteByte(byte(r))
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		default:
			b.WriteByte(winAnsi(r))
		}
	}
	return b.String()
}

// winAnsi maps a rune to its WinAnsiEncoding byte, or '?' when it has none.
func winAnsi(r rune) byte {
	// The printable ASCII range is identical in WinAnsi, which covers almost every filename in practice.
	if r >= 0x20 && r <= 0x7E {
		return byte(r)
	}
	// The handful of punctuation marks that appear in text this program writes itself, or that a filename
	// picks up from a word processor. WinAnsi puts them in the 0x80..0x9F range that Latin-1 leaves empty.
	switch r {
	case '\u2014': // em dash
		return 0x97
	case '\u2013': // en dash
		return 0x96
	case '\u2018':
		return 0x91
	case '\u2019':
		return 0x92
	case '\u201c':
		return 0x93
	case '\u201d':
		return 0x94
	case '\u2026': // ellipsis
		return 0x85
	case '\u2022': // bullet
		return 0x95
	case '\u20ac': // euro
		return 0x80
	}
	// The rest of Latin-1 maps one to one.
	if r >= 0xA0 && r <= 0xFF {
		return byte(r)
	}
	return '?'
}
