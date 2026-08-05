package protocol

import (
	"fmt"
	"math"
	"sort"
)

// Point is a coordinate in either grid-cell or image-pixel space, depending on
// context. Decoding is largely the business of mapping between the two.
type Point struct {
	X, Y float64
}

// Sub returns p - q.
func (p Point) Sub(q Point) Point { return Point{p.X - q.X, p.Y - q.Y} }

// Dist returns the Euclidean distance between p and q.
func (p Point) Dist(q Point) float64 { return math.Hypot(p.X-q.X, p.Y-q.Y) }

// Homography is a row-major 3x3 projective transform. It is what lets the
// receiver read a frame photographed off-axis: the camera sees the display as an
// arbitrary quadrilateral, and the homography undoes that projection exactly
// rather than approximately.
type Homography [9]float64

// HomographyFromQuad solves for the transform carrying src onto dst.
//
// Four point correspondences determine a homography up to scale, so fixing
// h[8] = 1 leaves eight unknowns in eight equations. Each correspondence
// (x,y) -> (u,v) contributes two rows:
//
//	x*h0 + y*h1 + h2 - u*x*h6 - u*y*h7 = u
//	x*h3 + y*h4 + h5 - v*x*h6 - v*y*h7 = v
func HomographyFromQuad(src, dst [4]Point) (Homography, error) {
	var a [8][9]float64
	for i := 0; i < 4; i++ {
		x, y := src[i].X, src[i].Y
		u, v := dst[i].X, dst[i].Y

		r := 2 * i
		a[r] = [9]float64{x, y, 1, 0, 0, 0, -u * x, -u * y, u}
		a[r+1] = [9]float64{0, 0, 0, x, y, 1, -v * x, -v * y, v}
	}

	sol, err := solve8(&a)
	if err != nil {
		return Homography{}, err
	}
	h := Homography{sol[0], sol[1], sol[2], sol[3], sol[4], sol[5], sol[6], sol[7], 1}
	return h, nil
}

// solve8 solves an 8x9 augmented system by Gauss-Jordan elimination with partial
// pivoting. A vanishing pivot means the four points were collinear or coincident,
// which happens when finder detection latches onto noise.
func solve8(a *[8][9]float64) ([8]float64, error) {
	const eps = 1e-12
	for col := 0; col < 8; col++ {
		pivot := col
		for r := col + 1; r < 8; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[pivot][col]) {
				pivot = r
			}
		}
		if math.Abs(a[pivot][col]) < eps {
			return [8]float64{}, fmt.Errorf("%w: singular at column %d", ErrDegenerateGeometry, col)
		}
		a[col], a[pivot] = a[pivot], a[col]

		inv := 1 / a[col][col]
		for c := col; c < 9; c++ {
			a[col][c] *= inv
		}
		for r := 0; r < 8; r++ {
			if r == col || a[r][col] == 0 {
				continue
			}
			f := a[r][col]
			for c := col; c < 9; c++ {
				a[r][c] -= f * a[col][c]
			}
		}
	}
	var out [8]float64
	for i := 0; i < 8; i++ {
		out[i] = a[i][8]
	}
	return out, nil
}

// Apply projects a point through the homography.
func (h Homography) Apply(p Point) Point {
	w := h[6]*p.X + h[7]*p.Y + h[8]
	if w == 0 {
		w = 1e-12
	}
	return Point{
		X: (h[0]*p.X + h[1]*p.Y + h[2]) / w,
		Y: (h[3]*p.X + h[4]*p.Y + h[5]) / w,
	}
}

// Invert returns the inverse transform, mapping image space back to grid space.
func (h Homography) Invert() (Homography, error) {
	m := [3][3]float64{
		{h[0], h[1], h[2]},
		{h[3], h[4], h[5]},
		{h[6], h[7], h[8]},
	}
	c00 := m[1][1]*m[2][2] - m[1][2]*m[2][1]
	c01 := m[1][2]*m[2][0] - m[1][0]*m[2][2]
	c02 := m[1][0]*m[2][1] - m[1][1]*m[2][0]
	det := m[0][0]*c00 + m[0][1]*c01 + m[0][2]*c02
	if math.Abs(det) < 1e-15 {
		return Homography{}, fmt.Errorf("%w: determinant %g", ErrDegenerateGeometry, det)
	}
	inv := 1 / det
	return Homography{
		c00 * inv,
		(m[0][2]*m[2][1] - m[0][1]*m[2][2]) * inv,
		(m[0][1]*m[1][2] - m[0][2]*m[1][1]) * inv,
		c01 * inv,
		(m[0][0]*m[2][2] - m[0][2]*m[2][0]) * inv,
		(m[0][2]*m[1][0] - m[0][0]*m[1][2]) * inv,
		c02 * inv,
		(m[0][1]*m[2][0] - m[0][0]*m[2][1]) * inv,
		(m[0][0]*m[1][1] - m[0][1]*m[1][0]) * inv,
	}, nil
}

// Distortion is the Brown-Conrady lens model: two radial terms and two
// tangential terms about a principal point.
//
// The parameter names and semantics match OpenCV's calibrateCamera output, so a
// calibration produced by the GoCV camera path drops straight into the pure-Go
// decoder without conversion.
type Distortion struct {
	K1 float64 `json:"k1" yaml:"k1"`
	K2 float64 `json:"k2" yaml:"k2"`
	P1 float64 `json:"p1" yaml:"p1"`
	P2 float64 `json:"p2" yaml:"p2"`

	// CX and CY are the principal point in pixels. Zero means image centre.
	CX float64 `json:"cx" yaml:"cx"`
	CY float64 `json:"cy" yaml:"cy"`

	// Scale normalises pixel offsets before the radial terms are applied. Zero
	// means half the image diagonal, which keeps K1 and K2 near unity magnitude.
	Scale float64 `json:"scale" yaml:"scale"`
}

// IsZero reports whether the model is the identity, letting callers skip the
// per-cell arithmetic entirely on an undistorted source such as a file replay.
func (d Distortion) IsZero() bool {
	return d.K1 == 0 && d.K2 == 0 && d.P1 == 0 && d.P2 == 0
}

// resolve fills in defaults for the principal point and normalisation scale.
func (d Distortion) resolve(imgW, imgH int) Distortion {
	if d.CX == 0 {
		d.CX = float64(imgW) / 2
	}
	if d.CY == 0 {
		d.CY = float64(imgH) / 2
	}
	if d.Scale == 0 {
		d.Scale = math.Hypot(float64(imgW), float64(imgH)) / 2
	}
	return d
}

// Apply maps an ideal pinhole point to the pixel a real lens would put it on.
//
// The direction matters: calibration expresses distortion as ideal to observed,
// so the decoder projects a grid cell through the homography and then distorts
// the result to find where to sample. Correcting the whole image the other way
// would cost a full resample per frame for no gain in accuracy.
func (d Distortion) Apply(p Point, imgW, imgH int) Point {
	if d.IsZero() {
		return p
	}
	r := d.resolve(imgW, imgH)
	x := (p.X - r.CX) / r.Scale
	y := (p.Y - r.CY) / r.Scale
	r2 := x*x + y*y
	radial := 1 + r.K1*r2 + r.K2*r2*r2
	dx := 2*r.P1*x*y + r.P2*(r2+2*x*x)
	dy := r.P1*(r2+2*y*y) + 2*r.P2*x*y
	return Point{
		X: (x*radial+dx)*r.Scale + r.CX,
		Y: (y*radial+dy)*r.Scale + r.CY,
	}
}

// Undistort inverts Apply by fixed-point iteration. It exists for calibration
// diagnostics and for the UI's perspective overlay; the decode path uses Apply.
func (d Distortion) Undistort(p Point, imgW, imgH int) Point {
	if d.IsZero() {
		return p
	}
	guess := p
	for i := 0; i < 12; i++ {
		err := d.Apply(guess, imgW, imgH).Sub(p)
		if math.Hypot(err.X, err.Y) < 1e-6 {
			break
		}
		guess = Point{guess.X - err.X, guess.Y - err.Y}
	}
	return guess
}

// OrderQuad sorts four points into a consistent counter-clockwise cycle in
// image coordinates, starting from the point nearest the image origin.
//
// It fixes the *cycle*, not which corner is top-left: a display photographed
// upside down produces the same cycle with a different starting point. Resolving
// that last ambiguity is the decoder's job, and it does so by trying each of the
// four rotations and letting the header checksum decide.
func OrderQuad(pts [4]Point) [4]Point {
	var cx, cy float64
	for _, p := range pts {
		cx += p.X / 4
		cy += p.Y / 4
	}

	idx := []int{0, 1, 2, 3}
	angle := func(i int) float64 {
		return math.Atan2(pts[i].Y-cy, pts[i].X-cx)
	}
	sort.Slice(idx, func(a, b int) bool { return angle(idx[a]) < angle(idx[b]) })

	// Rotate so the point closest to the origin leads. Because image y grows
	// downward, ascending atan2 traverses the quad counter-clockwise on screen.
	start := 0
	best := math.Inf(1)
	for k, i := range idx {
		if d := math.Hypot(pts[i].X, pts[i].Y); d < best {
			best, start = d, k
		}
	}

	var out [4]Point
	for k := 0; k < 4; k++ {
		out[k] = pts[idx[(start+k)%4]]
	}
	return out
}

// QuadArea returns the absolute area of a quadrilateral by the shoelace formula.
// Detection uses it to discard candidate quads too small to be a real frame.
func QuadArea(q [4]Point) float64 {
	var s float64
	for i := 0; i < 4; i++ {
		j := (i + 1) % 4
		s += q[i].X*q[j].Y - q[j].X*q[i].Y
	}
	return math.Abs(s) / 2
}
