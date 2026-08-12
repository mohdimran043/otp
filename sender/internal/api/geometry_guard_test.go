package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// A geometry the receiving camera cannot resolve has to be refused at upload, not discovered by a transfer
// that retransmits chunk 0 eleven times and then fails. That happened twice on the same afternoon with the
// same file, so these cases are the observed ones rather than invented ones.

func guardConfig() config.Config {
	cfg := config.Default()
	cfg.Optical.CameraShortSidePixels = 1080
	cfg.Optical.CameraLongSidePixels = 1920
	return cfg
}

// TestTheGridThatFailedTwiceIsRefused is the exact request that failed: 128 cells at 6 px, colour8, against
// a 1080-wide capture. 132 cells across 1080 pixels is 8.2 a cell and colour8 needs 10.
func TestTheGridThatFailedTwiceIsRefused(t *testing.T) {
	req := TransferRequest{GridWidth: 128, GridHeight: 128, CellPixels: 6, Encoder: "color8", BitDepth: 3}

	err := validateGeometryForCamera(req, guardConfig(), uint8(req.BitDepth))
	require.Error(t, err)

	// "marginal", not "cannot be read". This geometry decoded 10 frames of 463 on the first pass with 6 more
	// recovered by the engine, and acknowledged 24 chunks of 74 — so it is refused because it will not
	// finish, not because nothing gets through. Claiming the latter while an operator watches chunks arrive
	// is what made the earlier wording untrustworthy, and the assertion is here so it cannot come back.
	require.Contains(t, err.Error(), "marginal")
	require.NotContains(t, err.Error(), "cannot be read")
	require.Contains(t, err.Error(), "sideways", "rotation is the cheapest fix and must be offered")
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
