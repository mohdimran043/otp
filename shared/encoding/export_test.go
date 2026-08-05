package encoding

import (
	"fmt"
	"image"

	"github.com/opticaltransport/otp/shared/protocol"
)

// DecodePayloadAt reads a frame's payload without verifying it, so a test can
// measure how badly a damaged frame was damaged.
//
// It is deliberately test-only. Production code has no legitimate use for an
// unverified payload — every consumer downstream of the decoder is entitled to
// assume a frame that reached it passed CRC32 and SHA-256 — and exporting it
// would make the unsafe path as easy to reach as the safe one.
func DecodePayloadAt(e Encoder, g *protocol.Geometry, img image.Image) ([]byte, error) {
	c, ok := e.(*codec)
	if !ok {
		return nil, fmt.Errorf("encoding: %s is not a codec", e.Name())
	}
	r, err := c.read(g, img)
	if err != nil {
		return nil, err
	}
	return r.payload, nil
}

// BandRangesForTest exposes the rolling encoding's band split so a test can
// damage a specific band without duplicating the arithmetic that chose it.
func BandRangesForTest(rows, cols int) [][2]int { return bandRanges(rows, cols) }

// CRC16ForTest exposes the band checksum function so it can be held in step with
// the grid descriptor's.
func CRC16ForTest(data []byte) uint16 { return crc16(data) }
