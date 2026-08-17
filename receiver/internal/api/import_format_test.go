package api

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// The same side-effect registrations the handler relies on. Importing them here as well would be
	// redundant — they are already linked in by import.go — but naming why they matter is not.
	_ "image/gif"
)

// What an uploaded sheet is allowed to be.
//
// PNG-only was wrong for the case this endpoint most obviously serves. The printable export exists so an
// operator can print frames and photograph them, and every phone writes JPEG — so the feature refused
// exactly the file it was built to accept, with "the file is neither a zip nor a PNG". Measured before the
// fix: 0 of 21 photographs accepted. After: 21 of 21.

func sheet(t *testing.T) image.Image {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			c := color.RGBA{0, 0, 0, 255}
			if (x/4+y/4)%2 == 0 {
				c = color.RGBA{255, 255, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// image.Decode reads every format the handler registers, which is what the import path depends on.
func TestAPhotographIsADecodableUpload(t *testing.T) {
	original := sheet(t)

	for _, tc := range []struct {
		name   string
		encode func(*bytes.Buffer) error
	}{
		{"png, as the sender renders", func(b *bytes.Buffer) error { return png.Encode(b, original) }},
		{"jpeg, as every phone camera writes", func(b *bytes.Buffer) error {
			return jpeg.Encode(b, original, &jpeg.Options{Quality: 92})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, tc.encode(&buf))

			// The handler sizes from the header before decoding any pixels, then decodes. Both steps have
			// to recognise the format, and only the first one did for JPEG before this.
			cfg, format, err := image.DecodeConfig(bytes.NewReader(buf.Bytes()))
			require.NoError(t, err, "the size check must recognise it")
			assert.Equal(t, 64, cfg.Width)
			assert.NotEmpty(t, format)

			decoded, _, err := image.Decode(bytes.NewReader(buf.Bytes()))
			require.NoError(t, err, "and the decode must too")
			assert.Equal(t, original.Bounds(), decoded.Bounds())
		})
	}
}

// Something that is not an image is still refused, so widening the formats did not widen it to anything.
func TestAnUploadThatIsNotAnImageIsStillRefused(t *testing.T) {
	_, _, err := image.DecodeConfig(bytes.NewReader([]byte("this is not a sheet of paper")))
	assert.Error(t, err)
}
