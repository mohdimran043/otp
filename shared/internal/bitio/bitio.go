// Package bitio provides most-significant-bit-first readers and writers over
// byte slices. The optical protocol packs sub-byte symbols into grid cells, so
// every codec in the platform needs a shared, well-tested bit cursor.
package bitio

import "errors"

var (
	// ErrOverflow is returned when a write would exceed the destination buffer.
	ErrOverflow = errors.New("bitio: write exceeds buffer capacity")
	// ErrUnderflow is returned when a read would exceed the available bits.
	ErrUnderflow = errors.New("bitio: read exceeds available bits")
	// ErrWidth is returned when a symbol width is outside 1..32.
	ErrWidth = errors.New("bitio: symbol width must be between 1 and 32")
)

// Writer packs bits into a byte slice, most significant bit first.
type Writer struct {
	buf []byte
	pos int // bit cursor
}

// NewWriter returns a Writer over a freshly allocated buffer holding n bits.
func NewWriter(bits int) *Writer {
	return &Writer{buf: make([]byte, (bits+7)/8)}
}

// NewWriterBytes returns a Writer that appends into an existing buffer.
func NewWriterBytes(buf []byte) *Writer { return &Writer{buf: buf} }

// WriteBit appends a single bit.
func (w *Writer) WriteBit(b bool) error {
	if w.pos >= len(w.buf)*8 {
		return ErrOverflow
	}
	if b {
		w.buf[w.pos/8] |= 1 << (7 - uint(w.pos%8))
	}
	w.pos++
	return nil
}

// WriteBits appends the low `width` bits of v, most significant bit first.
func (w *Writer) WriteBits(v uint32, width int) error {
	if width < 1 || width > 32 {
		return ErrWidth
	}
	if w.pos+width > len(w.buf)*8 {
		return ErrOverflow
	}
	for i := width - 1; i >= 0; i-- {
		if err := w.WriteBit(v&(1<<uint(i)) != 0); err != nil {
			return err
		}
	}
	return nil
}

// Bits returns the number of bits written so far.
func (w *Writer) Bits() int { return w.pos }

// Bytes returns the underlying buffer. Trailing bits of the final byte are zero.
func (w *Writer) Bytes() []byte { return w.buf }

// Reader unpacks bits from a byte slice, most significant bit first.
type Reader struct {
	buf   []byte
	pos   int
	limit int
}

// NewReader returns a Reader over the whole of buf.
func NewReader(buf []byte) *Reader {
	return &Reader{buf: buf, limit: len(buf) * 8}
}

// NewReaderLimit returns a Reader that will not read beyond `bits` bits.
func NewReaderLimit(buf []byte, bits int) *Reader {
	if bits > len(buf)*8 {
		bits = len(buf) * 8
	}
	return &Reader{buf: buf, limit: bits}
}

// ReadBit consumes one bit.
func (r *Reader) ReadBit() (bool, error) {
	if r.pos >= r.limit {
		return false, ErrUnderflow
	}
	b := r.buf[r.pos/8]&(1<<(7-uint(r.pos%8))) != 0
	r.pos++
	return b, nil
}

// ReadBits consumes `width` bits and returns them right-aligned.
func (r *Reader) ReadBits(width int) (uint32, error) {
	if width < 1 || width > 32 {
		return 0, ErrWidth
	}
	if r.pos+width > r.limit {
		return 0, ErrUnderflow
	}
	var v uint32
	for i := 0; i < width; i++ {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		v <<= 1
		if b {
			v |= 1
		}
	}
	return v, nil
}

// Bits returns the number of bits consumed so far.
func (r *Reader) Bits() int { return r.pos }

// Remaining reports how many bits are still available.
func (r *Reader) Remaining() int { return r.limit - r.pos }

// MajorityVote reduces `repeat` consecutive copies of a `size`-byte record to a
// single record by voting on each bit. It is how the protocol recovers header
// and footer bands that were damaged in transit: any bit position corrupted in
// a minority of copies is repaired.
//
// src must hold at least repeat*size bytes. repeat must be odd so that no bit
// position can tie.
func MajorityVote(src []byte, size, repeat int) ([]byte, error) {
	if size <= 0 || repeat <= 0 {
		return nil, errors.New("bitio: size and repeat must be positive")
	}
	if repeat%2 == 0 {
		return nil, errors.New("bitio: repeat must be odd to avoid ties")
	}
	if len(src) < size*repeat {
		return nil, ErrUnderflow
	}
	if repeat == 1 {
		out := make([]byte, size)
		copy(out, src)
		return out, nil
	}
	out := make([]byte, size)
	threshold := repeat / 2
	for i := 0; i < size; i++ {
		for bit := 0; bit < 8; bit++ {
			mask := byte(1 << uint(7-bit))
			set := 0
			for c := 0; c < repeat; c++ {
				if src[c*size+i]&mask != 0 {
					set++
				}
			}
			if set > threshold {
				out[i] |= mask
			}
		}
	}
	return out, nil
}
