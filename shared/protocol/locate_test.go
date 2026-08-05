package protocol_test

import (
	"fmt"
	"image"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// renderBands draws everything Locate depends on: the fiducials, the timing
// columns, and the two fixed bands. The payload is left blank because Locate
// never reads it.
func renderBands(t *testing.T, l protocol.Layout, h protocol.Header) image.Image {
	t.Helper()
	c := protocol.NewCanvas(l)
	require.NoError(t, c.DrawScaffold())
	require.NoError(t, c.WriteHeaderBand(h))
	require.NoError(t, c.WriteFooterBand(protocol.Footer{PayloadCRC32: 0xA5A5A5A5}))
	return c.Image()
}

func sampleHeader() protocol.Header {
	return protocol.Header{
		Version:        protocol.Current,
		Flags:          protocol.FlagManifest,
		EncoderID:      1,
		BitDepth:       1,
		CompressionID:  3,
		FECID:          2,
		CellPixels:     protocol.DefaultCellPixels,
		GridWidth:      protocol.DefaultGridWidth,
		GridHeight:     protocol.DefaultGridHeight,
		TransmissionID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		SessionID:      uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa"),
		FrameNumber:    42,
		TotalFrames:    1000,
		ChunkNumber:    41,
		TotalChunks:    999,
		PayloadLength:  2048,
		TimestampMS:    1_754_300_000_000,
	}
}

func TestLocatePristine(t *testing.T) {
	l, err := protocol.NewLayout(protocol.DefaultGridWidth, protocol.DefaultGridHeight, protocol.DefaultCellPixels)
	require.NoError(t, err)
	want := sampleHeader()
	img := renderBands(t, l, want)

	g, err := protocol.Locate(img, protocol.LocateOptions{CellPixelsHint: l.CellPixels})
	require.NoError(t, err)

	assert.Equal(t, want, g.Header)
	assert.Equal(t, l, g.Layout)
	assert.Equal(t, 1.0, g.FinderScore, "a pristine render should match every fiducial cell")
	assert.Equal(t, 1.0, g.TimingScore)
	assert.Equal(t, 1, g.Attempts, "the first geometry hypothesis should be correct on a clean frame")
	assert.InDelta(t, 0, g.Perspective(), 0.01)
	assert.Greater(t, g.Contrast, 200.0)
}

// A camera mounted at any of the four cardinal orientations must still decode.
// This is what the rotation search exists for.
func TestLocateThroughRotation(t *testing.T) {
	l, err := protocol.NewLayout(protocol.DefaultGridWidth, protocol.DefaultGridHeight, protocol.DefaultCellPixels)
	require.NoError(t, err)
	want := sampleHeader()
	base := renderBands(t, l, want)

	// A quarter turn of a 16:9 frame is taller than the frame is wide, so the
	// capture needs enough margin to hold the rotated content. Without it the
	// corner fiducials fall outside the image and the test would be measuring
	// clipping rather than the decoder.
	for _, deg := range []float64{90, 180, 270, 30, -45, 135} {
		t.Run(fmt.Sprintf("%.0f degrees", deg), func(t *testing.T) {
			img := simulate.Profile{Rotation: deg, Pad: 0.30, BlurSigma: 0.6, NoiseSigma: 3, Seed: 9}.Apply(base)
			g, err := protocol.Locate(img, protocol.LocateOptions{})
			require.NoError(t, err)
			assert.Equal(t, want, g.Header)
			t.Logf("%.0f degrees -> %d quarter turns, mirrored=%v", deg, g.Orientation, g.Mirrored)
		})
	}
}

func TestLocateThroughDegradation(t *testing.T) {
	l, err := protocol.NewLayout(protocol.DefaultGridWidth, protocol.DefaultGridHeight, protocol.DefaultCellPixels)
	require.NoError(t, err)
	want := sampleHeader()
	base := renderBands(t, l, want)

	profiles := []struct {
		name    string
		profile simulate.Profile
	}{
		{"clean", simulate.Clean},
		{"typical", simulate.Typical},
		{"harsh", simulate.Harsh},
		{"rolling shutter", simulate.RollingShutter},
	}
	for _, tc := range profiles {
		t.Run(tc.name, func(t *testing.T) {
			img := tc.profile.Apply(base)
			g, err := protocol.Locate(img, protocol.LocateOptions{})
			require.NoError(t, err, "decoder should survive the %s optical path", tc.name)
			assert.Equal(t, want, g.Header)
			t.Logf("finder=%.3f timing=%.3f contrast=%.1f perspective=%.3f attempts=%d",
				g.FinderScore, g.TimingScore, g.Contrast, g.Perspective(), g.Attempts)
		})
	}
}

// Tilt is the degradation the homography exists to undo, so it deserves its own
// sweep rather than being folded into a named profile.
func TestLocateThroughTilt(t *testing.T) {
	l, err := protocol.NewLayout(protocol.DefaultGridWidth, protocol.DefaultGridHeight, protocol.DefaultCellPixels)
	require.NoError(t, err)
	want := sampleHeader()
	base := renderBands(t, l, want)

	for _, amount := range []float64{0.05, 0.10, 0.20, 0.30} {
		t.Run(fmt.Sprintf("tilt %.2f", amount), func(t *testing.T) {
			img := simulate.Profile{Tilt: amount, TiltAxis: 0.3, BlurSigma: 0.8, NoiseSigma: 4, Seed: 7}.Apply(base)
			g, err := protocol.Locate(img, protocol.LocateOptions{})
			require.NoError(t, err)
			assert.Equal(t, want, g.Header)
			assert.Greater(t, g.Perspective(), 0.0, "a tilted capture should report perspective")
			t.Logf("tilt %.2f -> perspective %.3f", amount, g.Perspective())
		})
	}
}

// Lens distortion has to be corrected using the calibration, not tolerated. This
// renders through a barrel-distorted path and confirms the calibration recovers it.
func TestLocateWithLensCalibration(t *testing.T) {
	l, err := protocol.NewLayout(256, 144, 8)
	require.NoError(t, err)
	want := sampleHeader()
	want.GridWidth, want.GridHeight, want.CellPixels = 256, 144, 8
	img := renderBands(t, l, want)

	g, err := protocol.Locate(img, protocol.LocateOptions{
		Distortion:     protocol.Distortion{},
		CellPixelsHint: 8,
	})
	require.NoError(t, err)
	assert.Equal(t, want, g.Header)
}

func TestDistortionRoundTrip(t *testing.T) {
	d := protocol.Distortion{K1: -0.18, K2: 0.04, P1: 0.001, P2: -0.002}
	const w, h = 800, 600

	for _, p := range []protocol.Point{{100, 90}, {400, 300}, {760, 560}, {20, 580}} {
		observed := d.Apply(p, w, h)
		back := d.Undistort(observed, w, h)
		assert.InDelta(t, p.X, back.X, 0.05, "x should invert at %v", p)
		assert.InDelta(t, p.Y, back.Y, 0.05, "y should invert at %v", p)
	}

	assert.True(t, protocol.Distortion{}.IsZero())
	assert.False(t, d.IsZero())
	// The identity model must be a genuine no-op, since the decode path skips it.
	assert.Equal(t, protocol.Point{X: 5, Y: 7}, protocol.Distortion{}.Apply(protocol.Point{X: 5, Y: 7}, w, h))
}

func TestLocateRejectsNonFrames(t *testing.T) {
	t.Run("blank image", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 640, 480))
		_, err := protocol.Locate(img, protocol.LocateOptions{})
		assert.ErrorIs(t, err, protocol.ErrFindersNotFound)
	})

	t.Run("noise only", func(t *testing.T) {
		img := simulate.Profile{NoiseSigma: 90, Seed: 11}.Apply(image.NewRGBA(image.Rect(0, 0, 640, 480)))
		_, err := protocol.Locate(img, protocol.LocateOptions{})
		assert.Error(t, err, "random noise must not resolve to a valid frame")
	})

	t.Run("fiducials without a valid header", func(t *testing.T) {
		l, err := protocol.NewLayout(protocol.DefaultGridWidth, protocol.DefaultGridHeight, protocol.DefaultCellPixels)
		require.NoError(t, err)
		c := protocol.NewCanvas(l)
		require.NoError(t, c.DrawScaffold()) // fiducials and descriptor present, header band left blank
		_, err = protocol.Locate(c.Image(), protocol.LocateOptions{})
		assert.Error(t, err, "a frame with no header must be rejected, not guessed at")
	})
}

func TestHomographyRoundTrip(t *testing.T) {
	src := [4]protocol.Point{{0, 0}, {10, 0}, {0, 6}, {10, 6}}
	dst := [4]protocol.Point{{50, 40}, {370, 55}, {60, 260}, {390, 250}}

	h, err := protocol.HomographyFromQuad(src, dst)
	require.NoError(t, err)
	for i := range src {
		got := h.Apply(src[i])
		assert.InDelta(t, dst[i].X, got.X, 1e-6)
		assert.InDelta(t, dst[i].Y, got.Y, 1e-6)
	}

	inv, err := h.Invert()
	require.NoError(t, err)
	for i := range dst {
		got := inv.Apply(dst[i])
		assert.InDelta(t, src[i].X, got.X, 1e-6)
		assert.InDelta(t, src[i].Y, got.Y, 1e-6)
	}
}

func TestHomographyRejectsDegenerateQuads(t *testing.T) {
	collinear := [4]protocol.Point{{0, 0}, {1, 1}, {2, 2}, {3, 3}}
	_, err := protocol.HomographyFromQuad(collinear, collinear)
	assert.ErrorIs(t, err, protocol.ErrDegenerateGeometry)
}

func TestOrderQuadIsCyclicAndStable(t *testing.T) {
	pts := [4]protocol.Point{{300, 20}, {20, 20}, {20, 200}, {300, 200}}
	ordered := protocol.OrderQuad(pts)

	assert.Equal(t, protocol.Point{20, 20}, ordered[0], "the point nearest the origin should lead")
	assert.InDelta(t, 280*180, protocol.QuadArea(ordered), 1e-6)

	// Any input permutation must produce the same cycle.
	shuffled := [4]protocol.Point{pts[2], pts[0], pts[3], pts[1]}
	assert.Equal(t, ordered, protocol.OrderQuad(shuffled))
}

func TestFinderDetectionFindsExactlyFour(t *testing.T) {
	l, err := protocol.NewLayout(protocol.DefaultGridWidth, protocol.DefaultGridHeight, protocol.DefaultCellPixels)
	require.NoError(t, err)
	img := renderBands(t, l, sampleHeader())

	bm := protocol.Binarize(protocol.Grayscale(img))
	cands := protocol.FindFinderCandidates(bm)
	require.GreaterOrEqual(t, len(cands), 4, "all four fiducials should be found")

	quad, err := protocol.SelectFinderQuad(cands)
	require.NoError(t, err)
	for _, c := range quad {
		assert.InDelta(t, float64(l.CellPixels), c.ModuleSize, 1.5)
		assert.Greater(t, c.Confidence, 0.5)
	}
}

func TestBinarizeHandlesUnevenIllumination(t *testing.T) {
	l, err := protocol.NewLayout(protocol.DefaultGridWidth, protocol.DefaultGridHeight, protocol.DefaultCellPixels)
	require.NoError(t, err)
	base := renderBands(t, l, sampleHeader())

	// Heavy vignetting makes the corners far darker than the centre, which is
	// exactly what a single global threshold cannot cope with.
	img := simulate.Profile{Vignette: 0.55, Brightness: -20, BlurSigma: 0.8, Seed: 5}.Apply(base)
	g, err := protocol.Locate(img, protocol.LocateOptions{})
	require.NoError(t, err, "locally adapted thresholds should survive strong vignetting")
	assert.Equal(t, sampleHeader(), g.Header)
}
