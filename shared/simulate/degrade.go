// Package simulate degrades rendered optical frames the way a real camera and
// lens would, so the decoder can be tested against realistic input rather than
// the pixel-perfect images the encoder produces.
//
// The geometry here is deliberately implemented from scratch rather than reusing
// the decoder's homography code. If both sides shared an implementation, a bug in
// it would cancel out and the tests would pass on frames no real camera could
// read.
package simulate

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"math/rand"
)

// Profile describes a simulated optical path. The zero value is a perfect
// channel: applying it returns the input unchanged.
type Profile struct {
	// BlurSigma is the gaussian blur radius in pixels, modelling defocus and
	// lens softness.
	BlurSigma float64

	// NoiseSigma is the standard deviation of additive sensor noise, in 8-bit
	// levels.
	NoiseSigma float64

	// Tilt rotates the frame in 3D, in 0..1, where 0.3 is a pronounced off-axis
	// view. It is the degradation the homography exists to undo.
	Tilt float64

	// TiltAxis selects the tilt direction in radians. 0 tilts about the vertical
	// axis, so the right edge recedes.
	TiltAxis float64

	// Rotation turns the image in the sensor plane, in degrees, modelling a
	// camera that is not mounted square to the display.
	Rotation float64

	// Scale resizes the capture. Below 1 models a camera further away or lower
	// resolution than the display. Zero means no rescale.
	Scale float64

	// Brightness shifts every level, in 8-bit units. Gamma applies a power curve;
	// zero means 1.0.
	Brightness float64
	Gamma      float64

	// Vignette darkens the corners, in 0..1, modelling lens falloff.
	Vignette float64

	// JPEGQuality round-trips the frame through JPEG at this quality, modelling a
	// camera that compresses before handing frames over. Zero disables it.
	JPEGQuality int

	// RollingShutterRows tears the frame every this many rows, modelling a
	// rolling-shutter sensor catching a display mid-refresh. Zero disables it.
	RollingShutterRows int

	// RollingShutterShift is how far, in pixels, each torn band slides.
	RollingShutterShift int

	// Pad surrounds the frame with dark margin before any geometry is applied,
	// as a fraction of the frame's larger side.
	//
	// Without it, rotating or tilting inside the original canvas pushes the
	// corner fiducials outside the image and silently clips them, which tests the
	// decoder against something no camera produces. A real sensor sees the display
	// occupying part of a larger field of view, and that headroom is what lets the
	// frame rotate without leaving the picture.
	Pad float64

	// Seed makes noise and tearing reproducible.
	Seed int64
}

// Named profiles spanning the range from a screen grab to a marginal capture.
var (
	// Pristine is a lossless channel, equivalent to reading the encoder's output
	// directly.
	Pristine = Profile{}

	// Clean models a well-focused camera squarely mounted in controlled light.
	Clean = Profile{BlurSigma: 0.6, NoiseSigma: 2, Gamma: 1.0, Pad: 0.05, Seed: 1}

	// Typical models a normal industrial installation: slight defocus, modest
	// noise, a few degrees off square.
	Typical = Profile{
		BlurSigma: 1.1, NoiseSigma: 6, Tilt: 0.06, Rotation: 1.5, Pad: 0.08,
		Brightness: -6, Gamma: 1.1, Vignette: 0.15, JPEGQuality: 92, Seed: 2,
	}

	// Harsh models a poorly sited camera at the edge of the protocol's operating
	// envelope: noticeably soft, off-axis, noisy, and compressing its output.
	//
	// Blur is held to roughly a fifth of a cell width. Beyond about a quarter, a
	// fiducial's one-cell separator ring closes up, its core merges with its
	// outer ring, and no amount of decoder cleverness recovers the structure —
	// see EnvelopeReport for where that limit actually falls.
	Harsh = Profile{
		BlurSigma: 1.5, NoiseSigma: 12, Tilt: 0.16, TiltAxis: 0.5, Rotation: -4,
		Scale: 0.9, Pad: 0.12, Brightness: -18, Gamma: 1.25, Vignette: 0.3,
		JPEGQuality: 80, Seed: 3,
	}

	// RollingShutter models a sensor that scans progressively and catches the
	// display mid-refresh.
	//
	// A tear crossing a fiducial rips its ring apart, and that frame is lost. That
	// is expected rather than a defect: the scheduler retransmits unacknowledged
	// chunks, so what matters is the fraction of frames that survive, not whether
	// every one does.
	RollingShutter = Profile{
		BlurSigma: 1.0, NoiseSigma: 6, Pad: 0.05,
		RollingShutterRows: 120, RollingShutterShift: 5, Seed: 4,
	}
)

// Apply runs the whole simulated optical path in the order a real one occurs:
// geometry first, because the lens projects before the sensor integrates; then
// photometry; then sensor noise; then whatever compression the camera applies on
// the way out.
func (p Profile) Apply(src image.Image) image.Image {
	out := toRGBA(src)

	if p.Pad > 0 {
		out = pad(out, p.Pad)
	}
	if p.Tilt != 0 {
		out = tilt(out, p.Tilt, p.TiltAxis)
	}
	if p.Rotation != 0 {
		out = rotate(out, p.Rotation)
	}
	if p.Scale > 0 && p.Scale != 1 {
		out = scale(out, p.Scale)
	}
	if p.BlurSigma > 0 {
		out = blur(out, p.BlurSigma)
	}
	if p.Vignette > 0 {
		out = vignette(out, p.Vignette)
	}
	if p.Brightness != 0 || (p.Gamma != 0 && p.Gamma != 1) {
		out = photometry(out, p.Brightness, p.Gamma)
	}
	if p.RollingShutterRows > 0 {
		out = rollingShutter(out, p.RollingShutterRows, p.RollingShutterShift, p.Seed)
	}
	if p.NoiseSigma > 0 {
		out = noise(out, p.NoiseSigma, p.Seed)
	}
	if p.JPEGQuality > 0 {
		out = jpegRoundTrip(out, p.JPEGQuality)
	}
	return out
}

func toRGBA(src image.Image) *image.RGBA {
	if r, ok := src.(*image.RGBA); ok {
		out := image.NewRGBA(r.Bounds())
		copy(out.Pix, r.Pix)
		return out
	}
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	return out
}

// pad centres the frame on a larger dark canvas, modelling a display that fills
// only part of the camera's field of view.
func pad(src *image.RGBA, fraction float64) *image.RGBA {
	b := src.Bounds()
	margin := int(math.Round(float64(max(b.Dx(), b.Dy())) * fraction))
	if margin <= 0 {
		return src
	}
	out := image.NewRGBA(image.Rect(0, 0, b.Dx()+2*margin, b.Dy()+2*margin))
	draw.Draw(out, out.Bounds(), &image.Uniform{color.RGBA{A: 255}}, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(margin, margin, margin+b.Dx(), margin+b.Dy()), src, b.Min, draw.Src)
	return out
}

// bilinear samples src at fractional coordinates, returning the background
// colour outside the image so a warped frame gets a clean border rather than
// smeared edge pixels.
func bilinear(src *image.RGBA, x, y float64) color.RGBA {
	b := src.Bounds()
	if x < float64(b.Min.X)-1 || y < float64(b.Min.Y)-1 ||
		x > float64(b.Max.X) || y > float64(b.Max.Y) {
		return color.RGBA{A: 255}
	}
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(x0), y-float64(y0)

	at := func(px, py int) (float64, float64, float64) {
		if px < b.Min.X || py < b.Min.Y || px >= b.Max.X || py >= b.Max.Y {
			return 0, 0, 0
		}
		i := src.PixOffset(px, py)
		return float64(src.Pix[i]), float64(src.Pix[i+1]), float64(src.Pix[i+2])
	}
	r00, g00, b00 := at(x0, y0)
	r10, g10, b10 := at(x0+1, y0)
	r01, g01, b01 := at(x0, y0+1)
	r11, g11, b11 := at(x0+1, y0+1)

	mix := func(v00, v10, v01, v11 float64) uint8 {
		top := v00*(1-fx) + v10*fx
		bot := v01*(1-fx) + v11*fx
		v := top*(1-fy) + bot*fy
		return uint8(math.Max(0, math.Min(255, v)))
	}
	return color.RGBA{
		R: mix(r00, r10, r01, r11),
		G: mix(g00, g10, g01, g11),
		B: mix(b00, b10, b01, b11),
		A: 255,
	}
}

// quadWarp resamples src so that its four corners land on the given destination
// corners, ordered top-left, top-right, bottom-right, bottom-left.
func quadWarp(src *image.RGBA, dst [4][2]float64) *image.RGBA {
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	srcCorners := [4][2]float64{{0, 0}, {w, 0}, {w, h}, {0, h}}

	// Solve destination to source directly: the resampler iterates over output
	// pixels and needs to know where each came from.
	m, ok := solveProjective(dst, srcCorners)
	if !ok {
		return src
	}

	out := image.NewRGBA(b)
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			den := m[6]*fx + m[7]*fy + 1
			if den == 0 {
				continue
			}
			sx := (m[0]*fx + m[1]*fy + m[2]) / den
			sy := (m[3]*fx + m[4]*fy + m[5]) / den
			c := bilinear(src, sx, sy)
			i := out.PixOffset(x, y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = c.R, c.G, c.B, 255
		}
	}
	return out
}

// solveProjective finds the eight coefficients carrying from onto to.
func solveProjective(from, to [4][2]float64) ([8]float64, bool) {
	var a [8][9]float64
	for i := 0; i < 4; i++ {
		x, y := from[i][0], from[i][1]
		u, v := to[i][0], to[i][1]
		a[2*i] = [9]float64{x, y, 1, 0, 0, 0, -u * x, -u * y, u}
		a[2*i+1] = [9]float64{0, 0, 0, x, y, 1, -v * x, -v * y, v}
	}
	for col := 0; col < 8; col++ {
		pivot := col
		for r := col + 1; r < 8; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[pivot][col]) {
				pivot = r
			}
		}
		if math.Abs(a[pivot][col]) < 1e-12 {
			return [8]float64{}, false
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
	return out, true
}

// tilt foreshortens one pair of edges, the way viewing a display off-axis does.
func tilt(src *image.RGBA, amount, axis float64) *image.RGBA {
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	a := math.Max(0, math.Min(0.45, amount))

	// Shrink the receding edge and pull it inward, distributing the effect
	// between horizontal and vertical according to the axis angle.
	hx := a * math.Abs(math.Cos(axis))
	vy := a * math.Abs(math.Sin(axis))

	dst := [4][2]float64{
		{w * hx * 0.5, h * vy * 0.5},
		{w * (1 - hx*0.5), h * vy * 0.5 * 0.4},
		{w * (1 - hx*0.5*0.4), h * (1 - vy*0.5)},
		{w * hx * 0.5 * 0.4, h * (1 - vy*0.5*0.4)},
	}
	return quadWarp(src, dst)
}

// rotate turns the image about its centre, keeping the same canvas size.
func rotate(src *image.RGBA, degrees float64) *image.RGBA {
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	cx, cy := w/2, h/2
	rad := degrees * math.Pi / 180
	cos, sin := math.Cos(rad), math.Sin(rad)

	corners := [4][2]float64{{0, 0}, {w, 0}, {w, h}, {0, h}}
	var dst [4][2]float64
	for i, c := range corners {
		dx, dy := c[0]-cx, c[1]-cy
		dst[i] = [2]float64{cx + dx*cos - dy*sin, cy + dx*sin + dy*cos}
	}
	return quadWarp(src, dst)
}

// scale resamples the frame, then re-centres it on a canvas of the original size
// so the caller's dimensions stay stable across a pipeline.
func scale(src *image.RGBA, factor float64) *image.RGBA {
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	sw, sh := w*factor, h*factor
	ox, oy := (w-sw)/2, (h-sh)/2

	dst := [4][2]float64{
		{ox, oy}, {ox + sw, oy}, {ox + sw, oy + sh}, {ox, oy + sh},
	}
	return quadWarp(src, dst)
}

// blur applies a separable gaussian kernel.
func blur(src *image.RGBA, sigma float64) *image.RGBA {
	radius := int(math.Ceil(sigma * 3))
	if radius < 1 {
		return src
	}
	kernel := make([]float64, 2*radius+1)
	var sum float64
	for i := range kernel {
		d := float64(i - radius)
		kernel[i] = math.Exp(-d * d / (2 * sigma * sigma))
		sum += kernel[i]
	}
	for i := range kernel {
		kernel[i] /= sum
	}

	b := src.Bounds()
	tmp := image.NewRGBA(b)
	out := image.NewRGBA(b)

	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			var r, g, bb float64
			for k, wt := range kernel {
				sx := clampInt(x+k-radius, 0, b.Dx()-1)
				i := src.PixOffset(sx, y)
				r += float64(src.Pix[i]) * wt
				g += float64(src.Pix[i+1]) * wt
				bb += float64(src.Pix[i+2]) * wt
			}
			i := tmp.PixOffset(x, y)
			tmp.Pix[i], tmp.Pix[i+1], tmp.Pix[i+2], tmp.Pix[i+3] = u8(r), u8(g), u8(bb), 255
		}
	}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			var r, g, bb float64
			for k, wt := range kernel {
				sy := clampInt(y+k-radius, 0, b.Dy()-1)
				i := tmp.PixOffset(x, sy)
				r += float64(tmp.Pix[i]) * wt
				g += float64(tmp.Pix[i+1]) * wt
				bb += float64(tmp.Pix[i+2]) * wt
			}
			i := out.PixOffset(x, y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = u8(r), u8(g), u8(bb), 255
		}
	}
	return out
}

// noise adds zero-mean gaussian sensor noise.
func noise(src *image.RGBA, sigma float64, seed int64) *image.RGBA {
	rng := rand.New(rand.NewSource(seed))
	b := src.Bounds()
	out := image.NewRGBA(b)
	copy(out.Pix, src.Pix)
	for i := 0; i < len(out.Pix); i += 4 {
		for c := 0; c < 3; c++ {
			out.Pix[i+c] = u8(float64(out.Pix[i+c]) + rng.NormFloat64()*sigma)
		}
	}
	return out
}

// photometry applies a brightness offset and a gamma curve.
func photometry(src *image.RGBA, brightness, gamma float64) *image.RGBA {
	if gamma <= 0 {
		gamma = 1
	}
	var lut [256]uint8
	for i := range lut {
		v := math.Pow(float64(i)/255, gamma)*255 + brightness
		lut[i] = u8(v)
	}
	b := src.Bounds()
	out := image.NewRGBA(b)
	for i := 0; i < len(src.Pix); i += 4 {
		out.Pix[i] = lut[src.Pix[i]]
		out.Pix[i+1] = lut[src.Pix[i+1]]
		out.Pix[i+2] = lut[src.Pix[i+2]]
		out.Pix[i+3] = 255
	}
	return out
}

// vignette darkens toward the corners.
func vignette(src *image.RGBA, amount float64) *image.RGBA {
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	cx, cy := w/2, h/2
	maxR := math.Hypot(cx, cy)

	out := image.NewRGBA(b)
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r := math.Hypot(float64(x)-cx, float64(y)-cy) / maxR
			f := 1 - amount*r*r
			i := src.PixOffset(x, y)
			o := out.PixOffset(x, y)
			out.Pix[o] = u8(float64(src.Pix[i]) * f)
			out.Pix[o+1] = u8(float64(src.Pix[i+1]) * f)
			out.Pix[o+2] = u8(float64(src.Pix[i+2]) * f)
			out.Pix[o+3] = 255
		}
	}
	return out
}

// rollingShutter slides horizontal bands sideways, the artefact a progressive
// sensor produces when the display refreshes while the sensor is still scanning.
func rollingShutter(src *image.RGBA, bandRows, shift int, seed int64) *image.RGBA {
	if bandRows <= 0 || shift == 0 {
		return src
	}
	rng := rand.New(rand.NewSource(seed + 977))
	b := src.Bounds()
	out := image.NewRGBA(b)

	for y := 0; y < b.Dy(); y++ {
		band := y / bandRows
		// Each band is displaced by a different amount, and the displacement
		// grows down the frame the way an accumulating scan offset does.
		off := int(float64(shift) * (float64(band%3) - 1) * (1 + float64(band)*0.1))
		if rng.Intn(7) == 0 {
			off += rng.Intn(3) - 1
		}
		for x := 0; x < b.Dx(); x++ {
			sx := clampInt(x+off, 0, b.Dx()-1)
			i := src.PixOffset(sx, y)
			o := out.PixOffset(x, y)
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] =
				src.Pix[i], src.Pix[i+1], src.Pix[i+2], 255
		}
	}
	return out
}

// jpegRoundTrip encodes and decodes the frame, introducing block artefacts.
func jpegRoundTrip(src *image.RGBA, quality int) *image.RGBA {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: quality}); err != nil {
		return src
	}
	dec, err := jpeg.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return src
	}
	return toRGBA(dec)
}

func u8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
