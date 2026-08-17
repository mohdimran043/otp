package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/readable"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// A geometry no camera could read has to be refused at upload rather than discovered by a transfer that
// retransmits chunk 0 eleven times and then fails. That happened twice on the same afternoon with the same
// file, so these cases are observed rather than invented.
//
// Only the hopeless band is refused. A marginal geometry is allowed through with its numbers stated, because
// the first version refused those too and that turned a slow channel into a blocked upload — which is the
// sender deciding on the operator's behalf about a transfer they may have good reason to attempt.

func guardConfig() config.Config {
	cfg := config.Default()
	cfg.Optical.CameraShortSidePixels = 1080
	cfg.Optical.CameraLongSidePixels = 1920
	return cfg
}

// TestAMarginalGridIsAllowedThroughWithItsNumbers is the request that failed twice: 128 cells at 6 px,
// colour8, against a 1080-wide capture. 132 cells across 1080 pixels is 8.2 a cell where colour8 wants 10.
func TestAMarginalGridIsAllowedThroughWithItsNumbers(t *testing.T) {
	req := TransferRequest{GridWidth: 128, GridHeight: 128, CellPixels: 6, Encoder: "color8", BitDepth: 3}

	// Allowed, not refused. Refusing it blocked an upload outright, and that is worse than a slow transfer
	// the operator chose knowingly: this geometry decoded 10 frames of 463 on the first pass with 6 more
	// recovered, and acknowledged 24 chunks of 74. Deciding it is not worth trying is not the sender's call.
	require.NoError(t, validateGeometryForCamera(req, guardConfig(), uint8(req.BitDepth)))

	// The wording it would report, though, must be honest about which band it is in.
	a := readable.Assess(req.GridWidth, guardConfig().Optical.QuietZone, uint8(req.BitDepth), 1080, 1920)
	require.True(t, a.Marginal)
	msg := a.Explain(req.GridWidth, uint8(req.BitDepth))
	require.Contains(t, msg, "marginal")
	require.NotContains(t, msg, "cannot be read")

	// And it must not suggest turning the camera, which does nothing for a square frame: a square inscribed
	// in a 1920x1080 picture is 1080 across, and so is one in a 1080x1920 picture.
	require.NotContains(t, msg, "sideways")
}

// TestTheGridThatWorkedIsAccepted is the one that carried a 175-chunk PDF to a verified hash.
func TestTheGridThatWorkedIsAccepted(t *testing.T) {
	req := TransferRequest{GridWidth: 96, GridHeight: 96, CellPixels: 8, Encoder: "color8", BitDepth: 3}
	err := validateGeometryForCamera(req, guardConfig(), uint8(req.BitDepth))
	require.NoError(t, err)
}

// TestBinaryIsOfferedWhenItWouldWork covers the trade an operator wanting a denser grid has to make: binary
// needs 6 pixels a cell against colour8's 10, so it reaches 176 cells on a 1080 capture where colour reaches
// 104.
//
// 160 cells rather than 192, and the difference is an observation rather than a tidy-up: 192 in binary located
// its geometry on every frame and read no payloads at all, which is what moved the binary floor from 4 to 6.
func TestBinaryIsOfferedWhenItWouldWork(t *testing.T) {
	req := TransferRequest{GridWidth: 160, GridHeight: 160, CellPixels: 4, Encoder: "color8", BitDepth: 3}
	err := validateGeometryForCamera(req, guardConfig(), uint8(req.BitDepth))
	require.Error(t, err, "6.6 px/cell is below the colour band")
	require.Contains(t, err.Error(), "one bit a cell")

	req.Encoder, req.BitDepth = "binary", 1
	require.NoError(t, validateGeometryForCamera(req, guardConfig(), uint8(req.BitDepth)),
		"160 cells clears the binary floor on a 1080 capture")
}

// TestTheCheckIsSkippedWithoutACamera keeps the file-loopback channel usable. There is no camera there, so
// there is no resolution to fail against, and refusing a dense grid would break a working development path.
func TestTheCheckIsSkippedWithoutACamera(t *testing.T) {
	cfg := guardConfig()
	cfg.Optical.CameraShortSidePixels = 0

	req := TransferRequest{GridWidth: 512, GridHeight: 512, CellPixels: 2, Encoder: "color8", BitDepth: 3}
	err := validateGeometryForCamera(req, cfg, uint8(req.BitDepth))
	require.NoError(t, err)
}

// TestASingleOptionDepthIsResolvedRatherThanRefused covers the upload that was blocked by a field the caller
// had no say in.
//
// Switching the encoder on the send form leaves the bit depth beside it holding the previous encoder's value,
// so "binary at depth 3" arrives — which is not a request for something unavailable, it is a request for
// binary carrying a leftover 3 from colour8. Binary offers only depth 1, so there is nothing to preserve.
func TestASingleOptionDepthIsResolvedRatherThanRefused(t *testing.T) {
	cfg := guardConfig()
	// 192 cells in binary is readable on a 1080 capture — 5.5 px/cell against the 4 binary needs — so this
	// case tests the depth resolution and not the geometry guard.
	req := TransferRequest{
		Filename: "x.bin", GridWidth: 192, GridHeight: 192, CellPixels: 4,
		Encoder: "binary", BitDepth: 3, Compression: "none", FECCodec: "none",
	}

	out, err := resolveEncoderDepth(req)
	require.NoError(t, err, "binary offers only depth 1, so a leftover 3 must be resolved, not refused")
	require.Equal(t, 1, out.BitDepth)

	// And the geometry is genuinely fine at that depth, which is the point of choosing binary for a dense grid.
	require.NoError(t, validateGeometryForCamera(out, cfg, uint8(out.BitDepth)))
}

// TestAmbiguousDepthIsStillRefused is the other half, and the reason the resolution above is narrow.
// Grayscale offers two depths and three, so a caller asking for four has made a real mistake between real
// options — picking one for them would be deciding, not helping.
func TestAmbiguousDepthIsStillRefused(t *testing.T) {
	req := TransferRequest{Encoder: "grayscale", BitDepth: 4}
	_, err := resolveEncoderDepth(req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only")
}

// The printed-sheet channel: a transfer that will be read off paper, not off a display.
//
// The guard's arithmetic is right and its input is wrong for anything printed. It measures against the
// camera the deployment is configured for — a phone pointed at a monitor — while a printed sheet is read
// by whatever photographs or scans it, which resolves several times as much. The cases below are the two
// halves that matter: a geometry the configured camera cannot read but a scanner can must get through, and
// one that even the scanner cannot read must still be refused.

// TestAPrintedGeometryIsJudgedAgainstTheCaptureThatWillReadIt is the case that was blocked.
//
// 384 cells in binary against the configured 1080 camera is 2.8 px/cell — hopeless, correctly refused, and
// about a camera nobody printing a sheet is going to use. A 600dpi scan of the same A4 sheet gives 4360
// pixels across the frame, which is 11.2 a cell: comfortably above the binary floor.
func TestAPrintedGeometryIsJudgedAgainstTheCaptureThatWillReadIt(t *testing.T) {
	req := TransferRequest{GridWidth: 384, GridHeight: 384, CellPixels: 8, Encoder: "binary", BitDepth: 1}

	require.Error(t, validateGeometryForCamera(req, guardConfig(), 1),
		"against the configured 1080 camera this is genuinely hopeless")

	req.CaptureShortSidePixels = 4360
	require.NoError(t, validateGeometryForCamera(req, guardConfig(), 1),
		"a 600dpi scan reads 384 cells at 11.2 px/cell and must not be refused")
}

// TestAPrintedGeometryTheScannerCannotReadIsStillRefused is why this is a capture resolution rather than
// another way of spelling send_anyway.
//
// Declaring the real capture keeps the guard doing its job against that capture, in both directions. That
// matters more on paper than on a display: printing a stack of sheets nothing can decode costs paper,
// toner and an afternoon, where displaying frames nothing can decode costs a retry.
//
// 1024 cells against a 600dpi scan's 4360 pixels is 4.2 a cell, below even the marginal band, so it is
// refused despite the capture being declared honestly.
func TestAPrintedGeometryTheScannerCannotReadIsStillRefused(t *testing.T) {
	req := TransferRequest{
		GridWidth: 1024, GridHeight: 1024, CellPixels: 8,
		Encoder: "binary", BitDepth: 1, CaptureShortSidePixels: 4360,
	}
	require.Error(t, validateGeometryForCamera(req, guardConfig(), 1),
		"1024 cells reaches 4.2 px/cell on a 600dpi scan, below the band where anything decodes")
}

// The band between those two is allowed through with its numbers, exactly as it is for a camera: 768 cells
// on the same scan is 5.6 px/cell, under the 6 binary wants but inside the marginal band. Refusing it would
// be the sender deciding on the operator's behalf about a geometry that decodes some of the time — the same
// judgement TestAMarginalGridIsAllowedThroughWithItsNumbers records for the camera channel, and it must not
// change just because the capture was declared rather than configured.
func TestAMarginalPrintedGeometryIsAllowedThrough(t *testing.T) {
	req := TransferRequest{
		GridWidth: 768, GridHeight: 768, CellPixels: 8,
		Encoder: "binary", BitDepth: 1, CaptureShortSidePixels: 4360,
	}
	require.NoError(t, validateGeometryForCamera(req, guardConfig(), 1))

	a := readable.Assess(req.GridWidth, guardConfig().Optical.QuietZone, 1, 4360, 4360)
	require.True(t, a.Marginal, "5.6 px/cell is the marginal band, not the hopeless one")
}

// And the ordinary transfer is untouched: no declared capture means the configured camera, exactly as
// before. The phone-photo geometry from the recommendation — 256 cells one-up — is checked against the
// deployment's own camera and refused there, which is correct: on a 1080 display capture it is 4.2 px/cell.
func TestAnUndeclaredCaptureStillMeansTheConfiguredCamera(t *testing.T) {
	req := TransferRequest{GridWidth: 256, GridHeight: 256, CellPixels: 8, Encoder: "binary", BitDepth: 1}
	require.Error(t, validateGeometryForCamera(req, guardConfig(), 1))

	// The same sheet photographed by a 12MP phone gives 2506 px across the frame — 9.6 a cell.
	req.CaptureShortSidePixels = 2506
	require.NoError(t, validateGeometryForCamera(req, guardConfig(), 1))
}

// A declared capture that is nonsense is ignored rather than trusted. Zero already means "not declared";
// a negative one is a caller error, and taking it literally would compute a negative px/cell and refuse
// every geometry with an explanation that made no sense.
func TestANonsensicalDeclaredCaptureFallsBackToTheCamera(t *testing.T) {
	req := TransferRequest{
		GridWidth: 96, GridHeight: 96, CellPixels: 8,
		Encoder: "color8", BitDepth: 3, CaptureShortSidePixels: -1,
	}
	require.NoError(t, validateGeometryForCamera(req, guardConfig(), 3),
		"96 cells colour is fine on the configured camera, which is what a negative declaration falls back to")
}
