package pdf_test

import (
	"bytes"
	"image"
	"image/color"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/pdf"
)

// A PDF is a set of numbered objects followed by a table of where each one begins, and a table that
// disagrees with the body by a single byte produces a file every reader rejects. Nothing about that is
// visible by inspection, so it is checked here rather than trusted.

func checkerboard(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{0, 0, 0, 255}
			if (x+y)%2 == 0 {
				c = color.RGBA{255, 255, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// The cross-reference table must point at the object it claims to.
//
// This is the failure that produces a file which looks fine, downloads fine, and will not open. Every
// offset is followed back into the body and required to land on that object's own header.
func TestEveryCrossReferenceOffsetLandsOnItsObject(t *testing.T) {
	doc := pdf.A4()
	doc.AddImage(checkerboard(16, 16), "sheet 1")
	doc.AddImage(checkerboard(32, 24), "sheet 2")
	out := doc.Bytes()

	require.True(t, bytes.HasPrefix(out, []byte("%PDF-1.4")), "a reader identifies the format by this")
	require.True(t, bytes.Contains(out, []byte("%%EOF")), "and refuses a file without this")

	// The table: "xref\n0 N\n" then N fixed-width entries.
	at := bytes.Index(out, []byte("\nxref\n"))
	require.Positive(t, at, "the cross-reference table must be present")

	header := regexp.MustCompile(`xref\n0 (\d+)\n`).FindSubmatch(out[at:])
	require.NotNil(t, header)
	count, err := strconv.Atoi(string(header[1]))
	require.NoError(t, err)
	require.Greater(t, count, 1)

	entries := regexp.MustCompile(`(\d{10}) \d{5} [nf] `).FindAllSubmatch(out[at:], -1)
	require.Len(t, entries, count, "one entry per object, including the free-list head")

	// Entry 0 is the free head; the rest must each land on "<n> 0 obj".
	for i := 1; i < count; i++ {
		offset, err := strconv.Atoi(string(entries[i][1]))
		require.NoError(t, err)
		require.Less(t, offset, len(out), "object %d is claimed past the end of the file", i)

		want := []byte(strconv.Itoa(i) + " 0 obj")
		assert.True(t, bytes.HasPrefix(out[offset:], want),
			"object %d should begin at %d; found %q", i, offset, string(out[offset:min(offset+24, len(out))]))
	}
}

// startxref must point at the table, or a reader cannot find it.
func TestStartxrefPointsAtTheTable(t *testing.T) {
	doc := pdf.A4()
	doc.AddImage(checkerboard(8, 8), "only sheet")
	out := doc.Bytes()

	m := regexp.MustCompile(`startxref\n(\d+)\n`).FindSubmatch(out)
	require.NotNil(t, m)
	offset, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)

	assert.True(t, bytes.HasPrefix(out[offset:], []byte("xref")),
		"startxref should land on the table; found %q", string(out[offset:min(offset+16, len(out))]))
}

// One page per frame, and the page tree must agree with how many were added.
func TestOnePagePerFrame(t *testing.T) {
	doc := pdf.A4()
	for i := range 5 {
		doc.AddImage(checkerboard(12, 12), "sheet "+strconv.Itoa(i+1))
	}
	require.Equal(t, 5, doc.Pages())

	out := doc.Bytes()
	assert.Equal(t, 5, bytes.Count(out, []byte("/Type/Page/")), "five page objects")
	assert.Contains(t, string(out), "/Count 5", "and a page tree that says so")
}

// Interpolation must stay off, which is the one flag that decides whether a printed frame is readable.
//
// Turned on, a reader smooths the image as it scales. That is right for a photograph and ruins a QR code:
// the cell edge is the thing being measured, and smoothing spreads every cell into its neighbours.
func TestImagesAreNotInterpolated(t *testing.T) {
	doc := pdf.A4()
	doc.AddImage(checkerboard(16, 16), "sheet")
	out := doc.Bytes()

	assert.NotContains(t, string(out), "/Interpolate true",
		"a smoothed QR code prints as an unreadable one")
}

// The caption is escaped, because a filename is not a safe PDF literal.
//
// Parentheses delimit a string in PDF, so a file called "report (final).pdf" would close the literal early
// and leave the rest of the caption as syntax — producing a document that fails to open, from a filename
// that is entirely ordinary.
func TestCaptionsAreEscaped(t *testing.T) {
	doc := pdf.A4()
	doc.AddImage(checkerboard(8, 8), `report (final) \ draft.bin`)
	out := string(doc.Bytes())

	assert.Contains(t, out, `report \(final\) \\ draft.bin`)
}

// An image is fitted inside the margins rather than run to the paper's edge.
//
// Desktop printers cannot reach the edge, and a frame clipped by an unprintable margin is a frame missing a
// fiducial — which reads as "the camera cannot find the grid" rather than as a printing problem.
func TestImagesStayInsideThePrintableArea(t *testing.T) {
	doc := pdf.New(pdf.A4Width, pdf.A4Height)
	// Deliberately wider than tall, to catch a fit that assumed square frames.
	doc.AddImage(checkerboard(400, 100), "wide")
	out := string(doc.Bytes())

	m := regexp.MustCompile(`q\n([\d.]+) 0 0 ([\d.]+) ([\d.]+) ([\d.]+) cm`).FindStringSubmatch(out)
	require.NotNil(t, m, "the content stream should place the image with a cm matrix")

	w, _ := strconv.ParseFloat(m[1], 64)
	h, _ := strconv.ParseFloat(m[2], 64)
	x, _ := strconv.ParseFloat(m[3], 64)
	y, _ := strconv.ParseFloat(m[4], 64)

	assert.GreaterOrEqual(t, x, 30.0, "left margin")
	assert.GreaterOrEqual(t, y, 30.0, "bottom margin, above the caption strip")
	assert.LessOrEqual(t, x+w, pdf.A4Width-30, "right margin")
	assert.LessOrEqual(t, y+h, pdf.A4Height-30, "top margin")

	// Aspect preserved: a stretched QR code is a broken one.
	assert.InDelta(t, 4.0, w/h, 0.01, "400x100 is four to one and must stay so")
}

// An empty document is still a valid PDF rather than a truncated one.
func TestAnEmptyDocumentIsStillValid(t *testing.T) {
	out := pdf.A4().Bytes()

	assert.True(t, bytes.HasPrefix(out, []byte("%PDF-1.4")))
	assert.Contains(t, string(out), "/Count 0")
	assert.Contains(t, string(out), "%%EOF")
}

// A caption is encoded to the font's own encoding, not written through as UTF-8.
//
// The font is Helvetica with WinAnsiEncoding, which is single-byte. A UTF-8 string written verbatim renders
// as its individual bytes: the first version of this put an em dash on every sheet and printed three
// characters of mojibake in its place.
func TestCaptionsAreEncodedForTheFont(t *testing.T) {
	doc := pdf.A4()
	doc.AddImage(checkerboard(8, 8), "report.bin — sheet 1")
	out := doc.Bytes()

	// The em dash is one byte in WinAnsi, and its UTF-8 form must not survive into the file.
	assert.NotContains(t, string(out), "—", "a UTF-8 em dash renders as mojibake in WinAnsi")
	assert.Contains(t, string(out), "report.bin \x97 sheet 1")
}

// A filename in a script the font cannot set still produces a valid document.
func TestAnUnrenderableFilenameDoesNotBreakTheDocument(t *testing.T) {
	doc := pdf.A4()
	doc.AddImage(checkerboard(8, 8), "文件.bin sheet 1")
	out := doc.Bytes()

	assert.True(t, bytes.HasPrefix(out, []byte("%PDF-1.4")))
	// Replaced rather than dropped or written raw: a missing character beats a corrupt file.
	assert.Contains(t, string(out), "??.bin sheet 1")
}
