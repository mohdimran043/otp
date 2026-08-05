package encoding

import (
	"image/color"

	"github.com/opticaltransport/otp/shared/protocol"
)

// reference is the frame's own photometric calibration, fitted from its scaffold.
//
// Nearest-neighbour palette matching compares a sample against ideal colours, so
// it is only as good as the assumption that the capture *is* the ideal colours.
// Nothing about a real optical path honours that: the panel has its own white
// point, the room adds ambient light, the sensor applies exposure and gamma, and
// the lens darkens the corners. A grey ramp survives none of it — under a
// twenty-percent corner falloff, one level in an eight-level ramp lands squarely
// on the level below.
//
// The fix needs no calibration target, because every frame already carries one.
// The fiducials and the timing columns are the cells whose values are known before
// the payload is read, and between them they surround the payload region: four
// corner blocks, plus a column of alternating known cells down each side. Sampling
// them gives a few hundred measurements of what this optical path does to black
// and to white, spread across the frame.
//
// Those measurements are fitted to a surface rather than interpolated between,
// because lens falloff is radial and interpolation cannot represent a bowl. Four
// corner references say nothing about the middle of a frame except by averaging
// the edges, which is exactly wrong when the middle is the brightest part of it.
// The model is a plane plus a radial term — enough for falloff and for the
// brightness gradient of an off-axis panel, and few enough parameters to be
// determined comfortably by the references available.
//
// What this cannot undo is gamma: the model is linear in intensity, two reference
// levels fix a line, and a power curve needs a third point to be observable at
// all. The residual is a bounded mid-tone error, which the wider-margin encodings
// absorb and the three-bit grey ramp eventually does not.
// TestPhotometricEnvelope records where each of them gives out.
type reference struct {
	// black and white model the two reference levels, per RGB channel.
	black, white [3]surface

	// gridW and gridH normalise cell coordinates into ±0.5, which keeps the fit
	// well conditioned regardless of grid size.
	gridW, gridH float64
}

// surface is an illumination model over the grid: a plane plus a radial term.
type surface struct {
	a, b, c, d float64
}

func (s surface) at(x, y float64) float64 {
	return s.a + s.b*x + s.c*y + s.d*(x*x+y*y)
}

// sample is one measurement of a cell whose intended value is known.
type sample struct {
	x, y float64
	on   bool
	c    color.RGBA
}

// newReference measures the optical path from a located frame's scaffold.
func newReference(s *protocol.Sampler, l protocol.Layout) *reference {
	r := &reference{gridW: float64(l.GridWidth), gridH: float64(l.GridHeight)}

	samples := make([]sample, 0, 4*protocol.FinderPattern*protocol.FinderPattern+2*l.PayloadRows())

	bits := protocol.FinderBits()
	for _, o := range l.FinderOrigins() {
		for dy := 0; dy < protocol.FinderPattern; dy++ {
			for dx := 0; dx < protocol.FinderPattern; dx++ {
				cell := protocol.Cell{X: o.X + dx, Y: o.Y + dy}
				x, y := r.norm(cell)
				samples = append(samples, sample{x, y, bits[dy][dx], s.Color(cell)})
			}
		}
	}
	// The timing columns are what make the fit possible: they run the full height
	// of the payload region in alternating phase, so they measure both reference
	// levels at every row rather than only at the corners.
	for _, t := range l.TimingCells() {
		x, y := r.norm(t.Cell)
		samples = append(samples, sample{x, y, t.On, s.Color(t.Cell)})
	}

	for ch := 0; ch < 3; ch++ {
		r.white[ch] = fitSurface(samples, ch, true)
		r.black[ch] = fitSurface(samples, ch, false)
	}

	// A fit that claims less contrast than the palettes need anywhere in the frame
	// is not describing this optical path — a blown highlight, a geometry that
	// missed, or references too damaged to fit. Fall back to the nominal range and
	// let the payload checksums decide, rather than normalising by noise.
	for _, p := range [][2]float64{{-0.5, -0.5}, {0.5, -0.5}, {-0.5, 0.5}, {0.5, 0.5}, {0, 0}} {
		for ch := 0; ch < 3; ch++ {
			if r.white[ch].at(p[0], p[1])-r.black[ch].at(p[0], p[1]) < 8 {
				r.black[ch] = surface{}
				r.white[ch] = surface{a: 255}
			}
		}
	}
	return r
}

// norm maps a cell centre into ±0.5 across the grid.
func (r *reference) norm(c protocol.Cell) (x, y float64) {
	return (float64(c.X)+0.5)/r.gridW - 0.5, (float64(c.Y)+0.5)/r.gridH - 0.5
}

// normalize maps a sample taken at a cell onto the ideal 0..255 range, using the
// black and white levels the model predicts at that position.
func (r *reference) normalize(c color.RGBA, cell protocol.Cell) color.RGBA {
	x, y := r.norm(cell)
	in := [3]float64{float64(c.R), float64(c.G), float64(c.B)}

	var out [3]uint8
	for ch := 0; ch < 3; ch++ {
		lo := r.black[ch].at(x, y)
		hi := r.white[ch].at(x, y)
		if hi-lo < 1 {
			hi = lo + 1
		}
		out[ch] = uint8(clampF((in[ch]-lo)/(hi-lo)*255, 0, 255) + 0.5)
	}
	return color.RGBA{R: out[0], G: out[1], B: out[2], A: 255}
}

// fitSurface least-squares fits one channel's response at one reference level.
//
// Four unknowns against a few hundred samples is a heavily overdetermined system,
// which is the point: the fiducial cells are only one cell wide in places, so blur
// bleeds their neighbours into them and every individual sample is slightly wrong.
// Averaging that many of them through a four-parameter model leaves a far better
// estimate than any single measurement.
func fitSurface(samples []sample, ch int, on bool) surface {
	// Normal equations for the basis [1, x, y, x²+y²].
	var ata [4][4]float64
	var atb [4]float64
	var n int

	for _, s := range samples {
		if s.on != on {
			continue
		}
		v := channel(s.c, ch)
		basis := [4]float64{1, s.x, s.y, s.x*s.x + s.y*s.y}
		for i := 0; i < 4; i++ {
			for j := 0; j < 4; j++ {
				ata[i][j] += basis[i] * basis[j]
			}
			atb[i] += basis[i] * v
		}
		n++
	}
	if n < 8 {
		return constantSurface(samples, ch, on)
	}

	sol, ok := solve4(ata, atb)
	if !ok {
		return constantSurface(samples, ch, on)
	}
	return surface{sol[0], sol[1], sol[2], sol[3]}
}

// constantSurface is the fallback when the system cannot be solved: the plain mean
// of the samples, which is the model the protocol's binary threshold already uses.
func constantSurface(samples []sample, ch int, on bool) surface {
	var sum float64
	var n int
	for _, s := range samples {
		if s.on != on {
			continue
		}
		sum += channel(s.c, ch)
		n++
	}
	if n == 0 {
		if on {
			return surface{a: 255}
		}
		return surface{}
	}
	return surface{a: sum / float64(n)}
}

func channel(c color.RGBA, ch int) float64 {
	switch ch {
	case 0:
		return float64(c.R)
	case 1:
		return float64(c.G)
	default:
		return float64(c.B)
	}
}

// solve4 solves a 4x4 system by Gaussian elimination with partial pivoting,
// reporting failure rather than returning a solution built on a pivot too small
// to trust.
func solve4(a [4][4]float64, b [4]float64) ([4]float64, bool) {
	for col := 0; col < 4; col++ {
		pivot := col
		for row := col + 1; row < 4; row++ {
			if abs(a[row][col]) > abs(a[pivot][col]) {
				pivot = row
			}
		}
		if abs(a[pivot][col]) < 1e-9 {
			return [4]float64{}, false
		}
		a[col], a[pivot] = a[pivot], a[col]
		b[col], b[pivot] = b[pivot], b[col]

		for row := col + 1; row < 4; row++ {
			f := a[row][col] / a[col][col]
			for k := col; k < 4; k++ {
				a[row][k] -= f * a[col][k]
			}
			b[row] -= f * b[col]
		}
	}

	var out [4]float64
	for row := 3; row >= 0; row-- {
		v := b[row]
		for k := row + 1; k < 4; k++ {
			v -= a[row][k] * out[k]
		}
		out[row] = v / a[row][row]
	}
	return out, true
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
