package protocol

import (
	"image"
	"math"

	"github.com/opticaltransport/otp/shared/internal/bitio"
)

// LocateOptions tunes the geometry search.
type LocateOptions struct {
	// Distortion is the active camera calibration. Leave zero for an undistorted
	// source such as a file replay or a screen capture.
	Distortion Distortion

	// ExpectedLayout, when set, is tried before the descriptor search. The
	// receiver supplies it from the decoder profile or from the previous frame,
	// which turns steady-state decoding into a single header read.
	ExpectedLayout *Layout

	// CellPixelsHint is the sender's configured cell size, used only to size the
	// sampling window when the descriptor is unreadable.
	CellPixelsHint int
}

// Geometry is a resolved frame location: where the grid sits in the image, and
// the header read from it.
type Geometry struct {
	Layout     Layout
	Homography Homography
	Distortion Distortion

	// Corners are the fiducial centres in image space, ordered top-left,
	// top-right, bottom-left, bottom-right.
	Corners [4]Point

	// ModuleSize is the apparent width of one cell, in pixels.
	ModuleSize float64

	// FinderScore and TimingScore are match fractions in 0..1, surfaced by the
	// receiver as decode quality.
	FinderScore float64
	TimingScore float64

	// Contrast is the luminance gap between set and clear fiducial cells.
	Contrast float64

	// BandErrorRate is the fraction of header-band bits that had to be repaired by majority vote
	// across the band's repeated copies.
	//
	// It is the closest thing to a bit error rate this protocol can measure honestly. The payload
	// gives no such figure — a payload either matches its checksum or does not, with nothing in
	// between — but the header is written several times over, so disagreement between the copies
	// is directly observable. A rising rate across frames that still decode is the earliest
	// warning that a camera is drifting out of focus, which is exactly when an operator wants to
	// hear about it rather than after frames start failing.
	BandErrorRate float64

	// Header is the header read at this geometry.
	Header Header

	// Descriptor is the grid descriptor that resolved the geometry.
	Descriptor Descriptor

	// Orientation is how far the capture was rotated, in quarter turns, and
	// Mirrored reports whether the fiducial cycle ran the opposite way — which
	// happens with a mirror in the optical path. Both are diagnostics for the
	// receiver's camera-alignment display.
	Orientation int
	Mirrored    bool

	// Attempts counts candidate orientations tried before this one matched.
	Attempts int
}

// Perspective reports how far from rectangular the captured quad is, in 0..1,
// where 0 is a perfect rectangle. The receiver UI shows it as perspective
// detection, since it directly measures how far off-axis the camera sits.
func (g Geometry) Perspective() float64 {
	top := g.Corners[0].Dist(g.Corners[1])
	bottom := g.Corners[2].Dist(g.Corners[3])
	left := g.Corners[0].Dist(g.Corners[2])
	right := g.Corners[1].Dist(g.Corners[3])
	if top+bottom == 0 || left+right == 0 {
		return 1
	}
	hSkew := math.Abs(top-bottom) / (top + bottom)
	vSkew := math.Abs(left-right) / (left + right)
	return clamp01(math.Max(hSkew, vSkew) * 2)
}

// orientation is one hypothesis about which detected fiducial is which corner.
type orientation struct {
	tl, tr, bl, br FinderCandidate
	quarterTurns   int
	mirrored       bool
}

// Locate finds the grid in a captured image and reads its header.
//
// A receiver holds a photograph, not a file: it does not know the grid
// dimensions, the rotation, or the scale, and it cannot read the header until it
// knows all three, because the header band's own position depends on the grid
// width. The descriptor block breaks that circle. Detection fixes the four
// fiducial centres precisely, each fiducial's local frame is enough to read the
// small descriptor block beside it, and the descriptor states the geometry
// outright — so nothing has to be estimated from fiducial size, which measurement
// showed cannot be done reliably: extrapolating a seven-cell fiducial across a
// grid hundreds of cells wide turns a two percent scale error into dozens of
// cells.
//
// What remains unknown after detection is only which fiducial is top-left, and
// that has just eight possibilities — four rotations, each in two handednesses.
// Each is tested by reading the descriptor and checking its CRC16, so the answer
// is verified rather than guessed.
func Locate(img image.Image, opts LocateOptions) (*Geometry, error) {
	gray := Grayscale(img)

	quad, gray, err := detectQuad(gray)
	if err != nil {
		return nil, err
	}

	centers := [4]Point{quad[0].Center, quad[1].Center, quad[2].Center, quad[3].Center}
	cycle := OrderQuad(centers)

	// Recover each cycle position's full candidate, so per-corner module sizes
	// travel with the ordering.
	byCenter := make(map[Point]FinderCandidate, 4)
	for _, c := range quad {
		byCenter[c.Center] = c
	}
	ordered := [4]FinderCandidate{}
	for i, p := range cycle {
		ordered[i] = byCenter[p]
	}

	module := MeanModuleSize(quad)
	if module <= 0 {
		return nil, ErrDegenerateGeometry
	}

	attempts := 0
	for _, dir := range []int{1, -1} {
		for r := 0; r < 4; r++ {
			at := func(k int) FinderCandidate { return ordered[((r+dir*k)%4+4)%4] }
			o := orientation{
				tl: at(0), tr: at(1), br: at(2), bl: at(3),
				quarterTurns: r,
				mirrored:     dir == -1,
			}
			attempts++

			// A caller-supplied layout skips the descriptor read entirely, which
			// is the steady-state path once a transmission is under way.
			if opts.ExpectedLayout != nil {
				if g, ok := tryLayout(*opts.ExpectedLayout, o, img, gray, opts, module, attempts); ok {
					g.Descriptor = DescriptorFor(*opts.ExpectedLayout)
					return g, nil
				}
			}

			d, ok := readDescriptor(o, img, gray, opts, module)
			if !ok {
				continue
			}
			layout, err := d.Layout()
			if err != nil {
				continue
			}
			if g, ok := tryLayout(layout, o, img, gray, opts, module, attempts); ok {
				g.Descriptor = d
				return g, nil
			}
		}
	}
	return nil, ErrDescriptorCRC
}

// detectQuad finds the four fiducials, falling back to a denoised image when the
// raw capture yields too few.
//
// The fallback is not free — a median pass over a megapixel frame costs real time
// — so it only runs when the first attempt has already failed. It returns the
// luminance buffer that produced the result, so later sampling reads the same
// pixels detection did.
func detectQuad(gray *GrayMap) ([4]FinderCandidate, *GrayMap, error) {
	quad, err := SelectFinderQuad(FindFinderCandidates(Binarize(gray)))
	if err == nil {
		return quad, gray, nil
	}

	denoised := Median3(gray)
	quad, err2 := SelectFinderQuad(FindFinderCandidates(Binarize(denoised)))
	if err2 == nil {
		return quad, denoised, nil
	}
	return [4]FinderCandidate{}, gray, err
}

// cornerFrame is an affine frame anchored at one fiducial.
//
// The two edge directions are exact rather than approximate: a projective map
// carries straight lines to straight lines, so the image of the grid row through
// the top two fiducial centres really is the straight line joining them. Only the
// scale varies along that line, and over the thirteen cells to the descriptor
// block that variation is a fraction of one cell.
type cornerFrame struct {
	origin Point
	ux, uy Point // unit vectors along the two edges leaving this corner
	module float64
}

func newCornerFrame(corner, alongX, alongY FinderCandidate) cornerFrame {
	dx := alongX.Center.Sub(corner.Center)
	dy := alongY.Center.Sub(corner.Center)
	lx := math.Hypot(dx.X, dx.Y)
	ly := math.Hypot(dy.X, dy.Y)
	if lx == 0 || ly == 0 {
		return cornerFrame{}
	}
	m := corner.ModuleArea
	if m <= 0 {
		m = corner.ModuleSize
	}
	return cornerFrame{
		origin: corner.Center,
		ux:     Point{dx.X / lx, dx.Y / lx},
		uy:     Point{dy.X / ly, dy.Y / ly},
		module: m,
	}
}

// at maps a cell offset from the fiducial's centre to an image point.
func (f cornerFrame) at(dx, dy float64) Point {
	return Point{
		X: f.origin.X + f.module*(dx*f.ux.X+dy*f.uy.X),
		Y: f.origin.Y + f.module*(dx*f.ux.Y+dy*f.uy.Y),
	}
}

func (f cornerFrame) valid() bool { return f.module > 0 }

// readDescriptor tries every corner's descriptor block under one orientation
// hypothesis, returning the first that passes its checksum.
func readDescriptor(o orientation, img image.Image, gray *GrayMap, opts LocateOptions, module float64) (Descriptor, bool) {
	// Each corner's frame points inward: its first axis runs away from the nearest
	// vertical edge and its second away from the nearest horizontal edge. That
	// matches the order Layout.DescriptorCellsAt writes, which is defined in the
	// same corner-local terms for exactly this reason.
	frames := []cornerFrame{
		newCornerFrame(o.tl, o.tr, o.bl),
		newCornerFrame(o.tr, o.tl, o.br),
		newCornerFrame(o.bl, o.br, o.tl),
		newCornerFrame(o.br, o.bl, o.tr),
	}

	radius := scoreRadius(module)
	for _, frame := range frames {
		if !frame.valid() {
			continue
		}
		thr, ok := calibrateCorner(frame, gray, radius)
		if !ok {
			continue
		}
		for _, k := range scaleTrials {
			scaled := frame
			scaled.module = frame.module * k
			var d Descriptor
			if err := d.UnmarshalBits(sampleDescriptorBlock(scaled, gray, opts, thr, radius)); err == nil {
				return d, true
			}
		}
	}
	return Descriptor{}, false
}

// scaleTrials are the local-scale corrections tried when reading a descriptor
// block, in decreasing order of likelihood.
//
// The block's far edge sits about twelve cells from the fiducial centre, so a
// scale estimate off by four percent lands sampling half a cell out and the read
// fails. Estimating the scale from a fiducial only seven cells across cannot do
// better than a few percent once blur and thresholding have eaten into its area,
// so rather than chase a more precise estimator the decoder simply tries a few
// corrections and lets the checksum say which was right. Each trial costs 52
// samples, so the whole sweep is cheaper than one header read.
var scaleTrials = []float64{1.00, 0.98, 1.02, 0.96, 1.04, 0.94, 1.06, 0.91, 1.09}

// sampleDescriptorBlock reads one corner's descriptor block through a local frame.
func sampleDescriptorBlock(frame cornerFrame, gray *GrayMap, opts LocateOptions, thr uint8, radius int) []byte {
	// Offsets are measured from the fiducial centre, which sits at cell
	// (3.5, 3.5) of its own pattern. The block begins FinderBox cells inward and
	// runs DescriptorCols by DescriptorRows in the corner's own frame.
	const half = FinderPattern / 2.0
	raw := make([]byte, (DescriptorBits+7)/8)
	w := bitio.NewWriterBytes(raw)
	for row := 0; row < DescriptorRows; row++ {
		for col := 0; col < DescriptorCols; col++ {
			if w.Bits() >= DescriptorBits {
				return raw
			}
			dx := (FinderBox - half) + float64(col) + 0.5
			dy := -half + float64(row) + 0.5
			p := opts.Distortion.Apply(frame.at(dx, dy), gray.W, gray.H)
			_ = w.WriteBit(SampleLuma(gray, p, radius) > thr)
		}
	}
	return raw
}

// calibrateCorner derives a binary threshold from one fiducial's known cells.
//
// The descriptor has to be read before any layout exists, so the sampler's usual
// whole-frame calibration is unavailable. One fiducial is enough: its 49 cell
// values are fixed by the format, so averaging the bright ones against the dark
// ones gives a threshold local to that corner — which is better than a global one
// anyway, since exposure varies across a captured frame.
func calibrateCorner(f cornerFrame, gray *GrayMap, radius int) (uint8, bool) {
	bits := FinderBits()
	const half = FinderPattern / 2.0
	var onSum, offSum, onN, offN float64
	for row := 0; row < FinderPattern; row++ {
		for col := 0; col < FinderPattern; col++ {
			dx := -half + float64(col) + 0.5
			dy := -half + float64(row) + 0.5
			v := float64(SampleLuma(gray, f.at(dx, dy), radius))
			if bits[row][col] {
				onSum += v
				onN++
			} else {
				offSum += v
				offN++
			}
		}
	}
	if onN == 0 || offN == 0 {
		return 0, false
	}
	onMean, offMean := onSum/onN, offSum/offN
	if onMean-offMean < 8 {
		// No usable contrast at this corner: glare, or the frame is not there.
		return 0, false
	}
	return uint8(clampF((onMean+offMean)/2, 1, 254)), true
}

// tryLayout builds the homography for a resolved layout and orientation, then
// reads and validates the header.
func tryLayout(l Layout, o orientation, img image.Image, gray *GrayMap, opts LocateOptions, module float64, attempts int) (*Geometry, bool) {
	src := l.FinderCenters()
	srcPts := [4]Point{
		{src[0][0], src[0][1]},
		{src[1][0], src[1][1]},
		{src[2][0], src[2][1]},
		{src[3][0], src[3][1]},
	}
	dst := [4]Point{o.tl.Center, o.tr.Center, o.bl.Center, o.br.Center}

	hm, err := HomographyFromQuad(srcPts, dst)
	if err != nil {
		return nil, false
	}

	s := NewSampler(l, hm, opts.Distortion, img)
	s.Gray = gray
	s.Radius = sampleRadius(module, l.CellPixels)

	h, err := s.ReadHeader()
	if err != nil {
		return nil, false
	}
	// The header restates the geometry, so a mismatch means the descriptor and the
	// header disagree and neither should be trusted.
	if int(h.GridWidth) != l.GridWidth || int(h.GridHeight) != l.GridHeight {
		return nil, false
	}

	return &Geometry{
		Layout:        l,
		Homography:    hm,
		Distortion:    opts.Distortion,
		Corners:       dst,
		ModuleSize:    module,
		FinderScore:   s.FinderScore(),
		TimingScore:   s.TimingScore(),
		Contrast:      s.Contrast(),
		BandErrorRate: s.BandErrorRate(),
		Header:        h,
		Orientation:   o.quarterTurns,
		Mirrored:      o.mirrored,
		Attempts:      attempts,
	}, true
}

// scoreRadius picks a small averaging window for the pre-layout passes, where the
// only scale estimate available is the fiducial's own.
func scoreRadius(module float64) int {
	r := int(module / 4)
	if r < 1 {
		return 0
	}
	if r > 2 {
		return 2
	}
	return r
}

// sampleRadius sizes the payload sampling window from the apparent cell size,
// falling back to the layout's own cell size when the capture is unscaled.
func sampleRadius(module float64, cellPx int) int {
	m := module
	if m <= 0 {
		m = float64(cellPx)
	}
	r := int(m / 4)
	if r < 1 {
		return 0
	}
	if r > 4 {
		return 4
	}
	return r
}

// NewSamplerAt rebuilds a sampler for an already-resolved geometry. The decode
// pipeline uses it to read the payload after Locate has settled the geometry.
func NewSamplerAt(g *Geometry, img image.Image) *Sampler {
	s := NewSampler(g.Layout, g.Homography, g.Distortion, img)
	s.Radius = sampleRadius(g.ModuleSize, g.Layout.CellPixels)
	return s
}
