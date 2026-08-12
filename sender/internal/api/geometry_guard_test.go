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

// TestBinaryIsOfferedWhenItWouldWork covers the trade an operator wanting a dense grid has to make: the same
// grid reads at one bit a cell, because a thresholded cell needs less than half the pixels a measured one does.
func TestBinaryIsOfferedWhenItWouldWork(t *testing.T) {
	req := TransferRequest{GridWidth: 192, GridHeight: 192, CellPixels: 4, Encoder: "color8", BitDepth: 3}
	err := validateGeometryForCamera(req, guardConfig(), uint8(req.BitDepth))
	require.Error(t, err)
	require.Contains(t, err.Error(), "one bit a cell")

	req.Encoder, req.BitDepth = "binary", 1
	err = validateGeometryForCamera(req, guardConfig(), uint8(req.BitDepth))
	require.NoError(t, err, "192 cells is readable at one bit a cell on a 1080 capture")
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
