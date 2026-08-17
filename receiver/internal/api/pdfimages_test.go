package api

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reading frames back out of a PDF.
//
// The images are extracted rather than the pages rasterised, so what the decoder receives is the bytes the
// sender encoded rather than a picture of them. These hold the two shapes that matter: the deflated raw RGB
// this project's own printable export writes, and the JPEG a scanner embeds.

// rawImageObject builds the PDF image object shape the printable export produces.
func rawImageObject(w, h int, fill byte) []byte {
	raw := bytes.Repeat([]byte{fill, fill, fill}, w*h)

	var deflated bytes.Buffer
	zw := zlib.NewWriter(&deflated)
	_, _ = zw.Write(raw)
	_ = zw.Close()

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	fmt.Fprintf(&out,
		"4 0 obj\n<</Type/XObject/Subtype/Image/Width %d/Height %d"+
			"/ColorSpace/DeviceRGB/BitsPerComponent 8/Filter/FlateDecode/Length %d>>\nstream\n",
		w, h, deflated.Len())
	out.Write(deflated.Bytes())
	out.WriteString("\nendstream\nendobj\n%%EOF\n")
	return out.Bytes()
}

func TestAPDFIsRecognisedByItsHeader(t *testing.T) {
	assert.True(t, looksLikePDF([]byte("%PDF-1.4\nrest of it")))
	assert.False(t, looksLikePDF([]byte("\x89PNG\r\n\x1a\n")))
	assert.False(t, looksLikePDF([]byte{0xFF, 0xD8, 0xFF}), "a JPEG is not a PDF")
	assert.False(t, looksLikePDF(nil))
}

// The export's own image shape round-trips, pixels intact.
func TestImagesComeOutOfADeflatedRGBDocument(t *testing.T) {
	doc := rawImageObject(8, 6, 0x7F)

	images, err := imagesFromPDF(doc)
	require.NoError(t, err)
	require.Len(t, images, 1)

	b := images[0].Bounds()
	assert.Equal(t, 8, b.Dx())
	assert.Equal(t, 6, b.Dy())

	// The pixels are the pixels, not a rendering of them: that is the point of extracting rather than
	// rasterising, and a value that drifted would mean the decoder is reading a picture of a frame.
	r, g, bl, _ := images[0].At(3, 3).RGBA()
	assert.Equal(t, uint32(0x7F), r>>8)
	assert.Equal(t, uint32(0x7F), g>>8)
	assert.Equal(t, uint32(0x7F), bl>>8)
}

// Several pages come out in file order, which is the order the sheets were printed in.
func TestEveryPageComesOutInOrder(t *testing.T) {
	var doc bytes.Buffer
	doc.WriteString("%PDF-1.4\n")
	for _, fill := range []byte{0x10, 0x20, 0x30} {
		one := rawImageObject(4, 4, fill)
		// Strip the header and trailer this helper adds, keeping the object.
		doc.Write(one[len("%PDF-1.4\n") : len(one)-len("%%EOF\n")])
	}
	doc.WriteString("%%EOF\n")

	images, err := imagesFromPDF(doc.Bytes())
	require.NoError(t, err)
	require.Len(t, images, 3)

	for i, want := range []uint32{0x10, 0x20, 0x30} {
		r, _, _, _ := images[i].At(1, 1).RGBA()
		assert.Equal(t, want, r>>8, "page %d is out of order", i+1)
	}
}

// A document with nothing readable says so rather than returning an empty success.
//
// The difference matters to an operator: "this PDF holds no frames" sends them to export images instead,
// where a silent zero looks like the receiver ignoring them.
func TestADocumentWithNoImagesIsRefused(t *testing.T) {
	_, err := imagesFromPDF([]byte("%PDF-1.4\n1 0 obj\n<</Type/Catalog>>\nendobj\n%%EOF\n"))
	assert.ErrorIs(t, err, ErrNoPDFImages)
}

// An image in a filter this does not read is skipped, and its neighbours still come out.
//
// A scanner embeds thumbnails and logos beside the page images, and one it cannot decode must not cost the
// frames next to it.
func TestAnUnreadableImageDoesNotSpoilTheRest(t *testing.T) {
	var doc bytes.Buffer
	doc.WriteString("%PDF-1.4\n")
	doc.WriteString("5 0 obj\n<</Type/XObject/Subtype/Image/Width 4/Height 4" +
		"/ColorSpace/DeviceCMYK/BitsPerComponent 8/Filter/JPXDecode/Length 4>>\nstream\nxxxx\nendstream\nendobj\n")
	good := rawImageObject(4, 4, 0x44)
	doc.Write(good[len("%PDF-1.4\n") : len(good)-len("%%EOF\n")])
	doc.WriteString("%%EOF\n")

	images, err := imagesFromPDF(doc.Bytes())
	require.NoError(t, err)
	require.Len(t, images, 1, "the readable one should survive its unreadable neighbour")
}

// An absurd size is refused before any pixels are allocated.
func TestAnAbsurdlyLargeImageIsRefused(t *testing.T) {
	doc := []byte("%PDF-1.4\n6 0 obj\n<</Type/XObject/Subtype/Image/Width 999999/Height 999999" +
		"/ColorSpace/DeviceRGB/BitsPerComponent 8/Filter/FlateDecode/Length 4>>\nstream\nxxxx\nendstream\nendobj\n%%EOF\n")
	_, err := imagesFromPDF(doc)
	assert.ErrorIs(t, err, ErrNoPDFImages, "it is skipped, so the document yields nothing")
}
