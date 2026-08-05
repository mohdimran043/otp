package protocol

import (
	"fmt"

	"github.com/opticaltransport/otp/shared/internal/bitio"
)

// Descriptor geometry. Each corner carries an identical copy of the grid
// descriptor in a small block just inward of its fiducial.
const (
	// DescriptorCols and DescriptorRows size the block, in cells.
	DescriptorCols = 8
	DescriptorRows = 7

	// DescriptorCells is the block's capacity.
	DescriptorCells = DescriptorCols * DescriptorRows

	// DescriptorBits is what a serialised descriptor occupies: twelve bits of
	// width, twelve of height, eight of cell size, four of flags, and a sixteen-bit
	// checksum.
	DescriptorBits = 12 + 12 + 8 + 4 + 16

	// descriptorDataBits is the payload the checksum covers.
	descriptorDataBits = 12 + 12 + 8 + 4
)

// ErrDescriptorCRC means no corner yielded a descriptor that passed its checksum.
var ErrDescriptorCRC = fmt.Errorf("protocol: grid descriptor CRC mismatch")

// Descriptor is the minimum a decoder needs before it can address anything else
// in the frame.
//
// It exists to break a circular dependency. Every other field lives in the header
// band, but the header band's own position depends on the grid width, so a
// decoder cannot read the header until it already knows the geometry. The
// descriptor is the one record placed where it can be read *without* that
// knowledge: in a fixed-size block at a fixed cell offset from a fiducial, which
// detection has already located precisely. Reading it needs only that fiducial's
// centre, its apparent cell size, and the direction of the two edges leaving it.
type Descriptor struct {
	GridWidth  uint16
	GridHeight uint16
	CellPixels uint8

	// Flags is reserved for future geometry variants — an alternative band repeat
	// count, say — so a later protocol version can change layout rules without
	// moving the descriptor itself.
	Flags uint8
}

// MarshalBits serialises the descriptor and appends its CRC16.
func (d Descriptor) MarshalBits() ([]byte, error) {
	if d.GridWidth > 0x0FFF || d.GridHeight > 0x0FFF {
		return nil, fmt.Errorf("%w: %dx%d exceeds the descriptor's 12-bit fields",
			ErrGridBounds, d.GridWidth, d.GridHeight)
	}
	if d.Flags > 0x0F {
		return nil, fmt.Errorf("%w: flags %d exceed four bits", ErrGridBounds, d.Flags)
	}

	data := bitio.NewWriter(descriptorDataBits)
	if err := data.WriteBits(uint32(d.GridWidth), 12); err != nil {
		return nil, err
	}
	if err := data.WriteBits(uint32(d.GridHeight), 12); err != nil {
		return nil, err
	}
	if err := data.WriteBits(uint32(d.CellPixels), 8); err != nil {
		return nil, err
	}
	if err := data.WriteBits(uint32(d.Flags), 4); err != nil {
		return nil, err
	}

	out := bitio.NewWriter(DescriptorBits)
	r := bitio.NewReaderLimit(data.Bytes(), descriptorDataBits)
	for i := 0; i < descriptorDataBits; i++ {
		b, err := r.ReadBit()
		if err != nil {
			return nil, err
		}
		if err := out.WriteBit(b); err != nil {
			return nil, err
		}
	}
	if err := out.WriteBits(uint32(crc16(data.Bytes())), 16); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// UnmarshalBits parses a descriptor and verifies its checksum.
func (d *Descriptor) UnmarshalBits(b []byte) error {
	r := bitio.NewReaderLimit(b, DescriptorBits)
	w, err := r.ReadBits(12)
	if err != nil {
		return err
	}
	h, err := r.ReadBits(12)
	if err != nil {
		return err
	}
	cell, err := r.ReadBits(8)
	if err != nil {
		return err
	}
	flags, err := r.ReadBits(4)
	if err != nil {
		return err
	}
	want, err := r.ReadBits(16)
	if err != nil {
		return err
	}

	// Re-serialise the data half so the checksum is computed over exactly the
	// same bytes the encoder used.
	data := bitio.NewWriter(descriptorDataBits)
	_ = data.WriteBits(w, 12)
	_ = data.WriteBits(h, 12)
	_ = data.WriteBits(cell, 8)
	_ = data.WriteBits(flags, 4)
	if got := uint32(crc16(data.Bytes())); got != want {
		return fmt.Errorf("%w: computed %04x, descriptor declares %04x", ErrDescriptorCRC, got, want)
	}

	d.GridWidth = uint16(w)
	d.GridHeight = uint16(h)
	d.CellPixels = uint8(cell)
	d.Flags = uint8(flags)
	return nil
}

// Layout resolves the geometry the descriptor describes.
func (d Descriptor) Layout() (Layout, error) {
	return NewLayout(int(d.GridWidth), int(d.GridHeight), int(d.CellPixels))
}

// DescriptorFor builds the descriptor matching a layout.
func DescriptorFor(l Layout) Descriptor {
	return Descriptor{
		GridWidth:  uint16(l.GridWidth),
		GridHeight: uint16(l.GridHeight),
		CellPixels: uint8(min(l.CellPixels, 255)),
	}
}

// crc16 is CRC-16/CCITT-FALSE. Sixteen bits over a thirty-six bit record makes a
// false accept a one-in-sixty-five-thousand event, which matters because the
// decoder tries eight candidate orientations and takes the first that validates.
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
