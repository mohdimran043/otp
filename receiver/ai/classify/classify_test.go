package classify_test

import (
	"fmt"
	"image"
	"image/color"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/shared/protocol"
)

func TestOfMapsEachStage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want classify.Bucket
	}{
		{"decoded", nil, classify.BucketDecoded},
		{"no quad", protocol.ErrFindersNotFound, classify.BucketNoQuad},
		{"degenerate", protocol.ErrDegenerateGeometry, classify.BucketDegenerate},
		{"descriptor", protocol.ErrDescriptorCRC, classify.BucketDescriptorCRC},
		{"header", protocol.ErrHeaderCRC, classify.BucketHeaderCRC},
		{"footer", protocol.ErrFooterCRC, classify.BucketFooterCRC},
		{"payload crc", protocol.ErrPayloadCRC, classify.BucketPayloadCRC},
		{"payload hash", protocol.ErrPayloadHash, classify.BucketPayloadCRC},
		{"unsupported", protocol.ErrUnsupportedVersion, classify.BucketUnsupported},
		{"floors", classify.ErrBelowFloors, classify.BucketBelowFloors},
		{"unknown", fmt.Errorf("something else"), classify.BucketOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, classify.Of(c.err))
		})
	}
}

// TestOfSeesThroughWrapping matters because the decoder wraps its sentinels with context, and a
// classifier that only matched bare values would put almost every real failure in "other".
func TestOfSeesThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("reading the payload at 80x80: %w", protocol.ErrPayloadCRC)
	require.Equal(t, classify.BucketPayloadCRC, classify.Of(wrapped))

	floors := fmt.Errorf("%w: fiducial match 0.61 is below the 0.75 floor", classify.ErrBelowFloors)
	require.Equal(t, classify.BucketBelowFloors, classify.Of(floors))
}

func TestRecoverable(t *testing.T) {
	require.True(t, classify.Recoverable(classify.BucketPayloadCRC))
	require.True(t, classify.Recoverable(classify.BucketDescriptorCRC))
	require.True(t, classify.Recoverable(classify.BucketHeaderCRC))
	require.True(t, classify.Recoverable(classify.BucketBelowFloors))
	require.False(t, classify.Recoverable(classify.BucketDecoded))
	require.False(t, classify.Recoverable(classify.BucketUnsupported))
	require.False(t, classify.Recoverable(classify.BucketNoQuad))
	require.False(t, classify.Recoverable(classify.BucketFooterCRC))
}

func TestClipped(t *testing.T) {
	require.InDelta(t, 1.0, classify.Clipped(flat(color.RGBA{R: 255, G: 255, B: 255, A: 255})), 0.01)
	require.InDelta(t, 0.0, classify.Clipped(flat(color.RGBA{R: 128, G: 128, B: 128, A: 255})), 0.01)

	// A fully saturated red is NOT clipping, and getting this wrong cost a real measurement. Colour8
	// puts every symbol at a corner of the RGB cube, so seven symbols in eight saturate a channel by
	// design. Counting those made a capture that decoded all 31 of its frames read as 0.628 clipped —
	// and the sidecar, which refuses above 0.5, would have declined every colour frame ever captured.
	require.InDelta(t, 0.0, classify.Clipped(flat(color.RGBA{R: 255, G: 0, B: 0, A: 255})), 0.01)
	require.InDelta(t, 0.0, classify.Clipped(flat(color.RGBA{R: 0, G: 255, B: 255, A: 255})), 0.01)
}

func TestClippedHandlesNil(t *testing.T) {
	require.Equal(t, 0.0, classify.Clipped(nil))
}

func flat(c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}
