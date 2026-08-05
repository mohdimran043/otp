package encoding

import (
	"github.com/opticaltransport/otp/shared/internal/bitio"
	"github.com/opticaltransport/otp/shared/protocol"
)

// A rolling-shutter sensor exposes one row of pixels at a time rather than the
// whole sensor at once. Pointed at a display that is mid-refresh, it captures the
// old frame in the rows it has already read and the new one in the rows it has
// not, producing a horizontal tear: a band of the image belongs to a different
// frame entirely.
//
// Row-major payload placement is the worst possible arrangement for that. A tear
// twenty rows deep destroys one long contiguous run of the chunk, which is
// exactly the damage pattern erasure coding handles least well — a burst that
// long can exceed what any reasonable parity budget covers. Interleaving inverts
// the problem: consecutive payload bits are placed in *different* bands, so the
// same tear removes every Nth bit instead of a contiguous block, and the loss
// spreads thinly enough for the FEC layer to repair.
//
// Each band then carries its own checksum. It is not there to correct anything —
// the footer's CRC32 and SHA-256 already say whether the payload survived. It is
// there to say which band failed, so the receiver can report the tear rather than
// an anonymous bad frame, and an operator can see that the fault is sensor sync
// rather than focus or aim.
const (
	// bandChecksumBits is the width of a band's checksum, in bits.
	bandChecksumBits = 16

	// preferredBandRows is the band height in cells. It is deliberately smaller
	// than a typical tear so that a tear spans whole bands: a tear confined to one
	// band is one checksum failure, while a tear straddling two is two, and either
	// way the damaged region is named exactly.
	preferredBandRows = 8
)

// bandRanges splits the payload rows into half-open row ranges.
//
// A band has to be wide enough that its checksum is a small tax rather than the
// bulk of it, so on a narrow grid the bands are made taller until they are. The
// result depends only on the layout, which is what lets a decoder reconstruct the
// same bands without the frame telling it anything.
func bandRanges(rows, cols int) [][2]int {
	bandRows := preferredBandRows
	for bandRows < rows && bandRows*cols < 4*bandChecksumBits {
		bandRows++
	}

	var out [][2]int
	for start := 0; start < rows; start += bandRows {
		out = append(out, [2]int{start, min(start+bandRows, rows)})
	}

	// A short final band would spend most of itself on its own checksum, so it
	// joins the band before it instead.
	if n := len(out); n > 1 {
		last := out[n-1]
		if (last[1]-last[0])*cols < 4*bandChecksumBits {
			out[n-2][1] = last[1]
			out = out[:n-1]
		}
	}
	return out
}

// rollingPlan arranges the payload interleaved across bands, reserving the tail
// of each band for that band's checksum.
func rollingPlan(l protocol.Layout, bitsPerCell int) plan {
	r := l.PayloadRect()
	cols := l.PayloadCols()
	ranges := bandRanges(l.PayloadRows(), cols)

	payloadCells := make([][]protocol.Cell, len(ranges))
	checksumCells := make([][]protocol.Cell, len(ranges))
	for i, rg := range ranges {
		cells := make([]protocol.Cell, 0, (rg[1]-rg[0])*cols)
		for y := rg[0]; y < rg[1]; y++ {
			for x := 0; x < cols; x++ {
				cells = append(cells, protocol.Cell{X: r.Min.X + x, Y: r.Min.Y + y})
			}
		}
		// The checksum sits at the end of the band, where it is reached last in
		// reading order and so needs no separate exclusion rule.
		split := len(cells) - bandChecksumBits
		payloadCells[i], checksumCells[i] = cells[:split], cells[split:]
	}

	// Round-robin across the bands: symbol i goes to band i%bands. Within a band
	// the symbols stay in relative order, so a band's checksum covers a
	// well-defined, reproducible bit string.
	order := make([]protocol.Cell, 0, l.PayloadCellCount())
	bandOf := make([]int, 0, l.PayloadCellCount())
	for i := 0; ; i++ {
		placed := false
		for b := range payloadCells {
			if i < len(payloadCells[b]) {
				order = append(order, payloadCells[b][i])
				bandOf = append(bandOf, b)
				placed = true
			}
		}
		if !placed {
			break
		}
	}

	return plan{
		cells: order,
		finish: func(c *protocol.Canvas, symbols []uint32) error {
			sums := bandChecksums(symbols, bandOf, len(ranges), bitsPerCell)
			for b, cells := range checksumCells {
				for i, cell := range cells {
					c.SetBit(cell, sums[b]&(1<<uint(bandChecksumBits-1-i)) != 0)
				}
			}
			return nil
		},
		diagnose: func(s *protocol.Sampler, symbols []uint32) error {
			sums := bandChecksums(symbols, bandOf, len(ranges), bitsPerCell)
			var damaged []int
			for b, cells := range checksumCells {
				var got uint16
				for _, cell := range cells {
					got <<= 1
					if s.Bit(cell) {
						got |= 1
					}
				}
				if got != sums[b] {
					damaged = append(damaged, b)
				}
			}
			if len(damaged) == 0 {
				return nil
			}
			return &BandDamageError{Bands: damaged, TotalBands: len(ranges)}
		},
	}
}

// bandChecksums computes each band's checksum over the symbols that landed in it.
//
// The checksum covers the symbols as read, not the payload bytes they encode, so
// it can be computed by the decoder before the payload is reassembled — which is
// the whole point: a torn band's bits never make it into a payload to check.
func bandChecksums(symbols []uint32, bandOf []int, bands, bitsPerCell int) []uint16 {
	counts := make([]int, bands)
	for _, b := range bandOf {
		counts[b]++
	}

	writers := make([]*bitio.Writer, bands)
	for b := range writers {
		writers[b] = bitio.NewWriter(counts[b] * bitsPerCell)
	}
	for i, v := range symbols {
		// The writer was sized from the same band assignment, so it cannot overflow.
		_ = writers[bandOf[i]].WriteBits(v, bitsPerCell)
	}

	out := make([]uint16, bands)
	for b, w := range writers {
		out[b] = crc16(w.Bytes())
	}
	return out
}

// crc16 is CRC-16/CCITT-FALSE, the same polynomial and seed the grid descriptor
// uses, so the protocol has one sixteen-bit checksum rather than two subtly
// different ones. TestBandChecksumMatchesDescriptor holds the two in step.
func crc16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
