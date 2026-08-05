package fec

// GF(256) arithmetic, as RFC 6330 Section 5.7 defines it.
//
// The field is the one every byte-oriented erasure code uses: 256 elements,
// addition is exclusive-or, and multiplication is polynomial multiplication modulo
// the primitive polynomial x^8 + x^4 + x^3 + x^2 + 1. What makes it useful here is
// that every non-zero element has an inverse, so a system of linear equations over
// bytes can be solved by ordinary Gaussian elimination — which is exactly what
// decoding a fountain code comes down to.
//
// The RFC publishes its OCT_EXP and OCT_LOG tables as literal data. They are
// generated here instead, because a table that is computed cannot be mistyped, and
// TestGF256MatchesRFC checks the generated values against the published ones at the
// points that pin the whole table.
var (
	// octExp maps i to alpha^i. It runs to 510 entries rather than 255 so that
	// multiplying two logarithms never needs a modulo: the sum of two logs is at
	// most 508.
	octExp [510]uint8

	// octLog maps a non-zero element to its logarithm base alpha.
	octLog [256]uint8
)

func init() {
	x := 1
	for i := 0; i < 510; i++ {
		octExp[i] = uint8(x)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D // The primitive polynomial, as a bit pattern.
		}
	}
	for i := 0; i < 255; i++ {
		octLog[octExp[i]] = uint8(i)
	}
}

// gfMul multiplies two field elements.
func gfMul(a, b uint8) uint8 {
	if a == 0 || b == 0 {
		return 0
	}
	return octExp[int(octLog[a])+int(octLog[b])]
}

// gfDiv divides a by b. b must not be zero.
func gfDiv(a, b uint8) uint8 {
	if a == 0 {
		return 0
	}
	return octExp[int(octLog[a])-int(octLog[b])+255]
}

// gfPow returns alpha^i for any non-negative i.
func gfPow(i int) uint8 { return octExp[i%255] }

// xorInto adds src into dst, element by element.
func xorInto(dst, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}

// mulAddInto adds c*src into dst. It is the inner loop of every solve in this
// package, so the two cases that need no multiplication at all are taken first.
func mulAddInto(dst, src []byte, c uint8) {
	switch c {
	case 0:
		return
	case 1:
		xorInto(dst, src)
		return
	}
	lc := int(octLog[c])
	for i, v := range src {
		if v != 0 {
			dst[i] ^= octExp[lc+int(octLog[v])]
		}
	}
}

// mulInto scales dst by c.
func mulInto(dst []byte, c uint8) {
	switch c {
	case 0:
		for i := range dst {
			dst[i] = 0
		}
		return
	case 1:
		return
	}
	lc := int(octLog[c])
	for i, v := range dst {
		if v != 0 {
			dst[i] = octExp[lc+int(octLog[v])]
		}
	}
}
