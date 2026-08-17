package simulate

// The paper channel: render, print, scan, decode.
//
// The profiles in degrade.go model a lens pointed at a lit panel. Paper is a different channel and the
// differences are the ones that decide what geometry survives it, so the path is modelled explicitly here
// rather than by picking a camera profile that looks about right.
//
// Four things matter, and three of them make paper *worse* than a screen at the same pixels per cell:
//
//  1. The printable document places its image with /Interpolate absent, so the reader resamples with
//     NEAREST NEIGHBOUR — deliberately, because smoothing spreads a cell into its neighbours and a soft
//     QR code prints as an unreadable one. It also means a cell's edge lands on a whole printer dot or it
//     does not, and the jitter that produces is the real cost of a cell that is not a whole number of dots.
//  2. A printer is a bilevel device. Continuous tone is halftoned, and colour is halftoned as separate
//     inks that register against each other imperfectly.
//  3. Paper does not lie flat. The decoder fits one homography per frame and a homography describes a
//     plane; a sheet with any waviness departs from that plane by a fraction of a cell, which at a dense
//     geometry is the whole margin.
//
// And one that makes it better:
//
//  4. A scanner integrates over its aperture rather than point-sampling, and it does so under controlled
//     illumination with no exposure, white balance, or motion to get wrong. That is why the pixels-per-cell
//     floors in shared/readable — measured against handheld captures of panels — do not transfer directly,
//     and why this file exists instead of an argument from those numbers.

import (
	"image"
	"image/color"
	"math"
)

// A4 printable area, in points, matching what sender/internal/pdf computes: the page less a 36pt margin on
// each side, less the 24pt caption strip under the frame.
const (
	A4WidthPt, A4HeightPt = 595.28, 841.89
	pdfMarginPt           = 36.0
	pdfCaptionPt          = 24.0

	availWpt = A4WidthPt - 2*pdfMarginPt
	availHpt = A4HeightPt - 2*pdfMarginPt - pdfCaptionPt
)

// PrinterDPI is the device resolution a sheet is rasterised at. 600 is an ordinary office laser.
const PrinterDPI = 600

// PrintedSizeInches is how large a sheet image ends up on paper: as large as the margins allow, aspect
// preserved. It mirrors the fit the printable document performs, so a caller can ask what a given frame
// geometry will physically measure before printing several hundred of them.
func PrintedSizeInches(imgW, imgH int) (w, h float64) {
	scale := math.Min(availWpt/float64(imgW), availHpt/float64(imgH))
	return float64(imgW) * scale / 72, float64(imgH) * scale / 72
}

// PixelsPerCell is the figure shared/readable is built around, computed for a printed sheet: how many
// scan pixels fall across one cell, given how many cells span the sheet's width.
func PixelsPerCell(sheetW, sheetH, cellsAcross int, s Scanner) float64 {
	if cellsAcross <= 0 {
		return 0
	}
	widthIn, _ := PrintedSizeInches(sheetW, sheetH)
	return widthIn * s.DPI / float64(cellsAcross)
}

// Scanner is one capture device reading a printed sheet.
type Scanner struct {
	Name string

	// DPI is the sampling resolution over the paper. A flatbed is set to this directly; a photograph is
	// expressed as the effective DPI its sensor achieves across the sheet.
	DPI float64

	// BlurInches is the optical softness of the whole path — printer dot gain plus scanner optics —
	// expressed physically rather than in pixels, because that is how it behaves: the same print scanned
	// at twice the resolution is twice as soft measured in pixels and exactly as soft measured in inches.
	//
	// This is the least certain number in the model. It is set from the one thing reliably true about
	// scanners, which is that rated resolution is not resolved resolution: a unit sold as 600dpi typically
	// holds usable contrast to 60-70% of that, so the optical spot is meaningfully wider than one sample.
	BlurInches float64

	// NoiseSigma is sensor noise and paper texture in 8-bit levels, and RotationDeg a sheet laid down
	// slightly crooked on the platen.
	NoiseSigma  float64
	RotationDeg float64

	// JPEGQuality models a device that compresses on the way out, as every phone does. Zero disables it.
	JPEGQuality int
}

// The capture devices worth answering for.
//
// Chosen to be pessimistic rather than flattering: the failure worth avoiding is a table that says a
// geometry works and then four hundred printed sheets say otherwise.
var (
	// Flatbed600 is an ordinary office flatbed at its common high setting.
	Flatbed600 = Scanner{Name: "flatbed-600", DPI: 600, BlurInches: 1.0 / 250, NoiseSigma: 6, RotationDeg: 0.4}

	// Flatbed300 is the same unit at the setting most people leave it on.
	Flatbed300 = Scanner{Name: "flatbed-300", DPI: 300, BlurInches: 1.0 / 250, NoiseSigma: 6, RotationDeg: 0.4}

	// Phone12MP is a handheld photograph of the whole sheet.
	//
	// The sensor is 4032x3024 and A4 is 8.27 by 11.69 inches, so framing the page in portrait the binding
	// dimension is height: 4032 over 11.69 is 345 dpi, against 366 across the width. Discounted to 320 for
	// the framing slack nobody avoids — a photograph with the sheet exactly edge to edge is one nobody
	// takes, and a clipped corner is a lost fiducial.
	Phone12MP = Scanner{
		Name: "phone-12mp", DPI: 320, BlurInches: 1.0 / 220,
		NoiseSigma: 7, RotationDeg: 1.2, JPEGQuality: 92,
	}
)

// Scanners is the set the paper sweep reports on.
var Scanners = []Scanner{Flatbed600, Flatbed300, Phone12MP}

// PrintAndScan runs a composed sheet through the whole paper path and returns what the scanner would hand
// the receiver.
//
// stress scales every degradation together: 1.0 is the modelled device, 1.4 a worse print on worse paper
// read by a tireder unit. Sweeping it is how margin gets measured, and margin is the useful output — a
// geometry that passes at 1.0 and fails at 1.1 is not one to print a stack of, and a single pass/fail at
// one operating point cannot tell that apart from a geometry that passes comfortably.
func PrintAndScan(sheet image.Image, s Scanner, stress float64) image.Image {
	if stress <= 0 {
		stress = 1
	}
	src := toRGBA(sheet)
	b := src.Bounds()

	widthIn, heightIn := PrintedSizeInches(b.Dx(), b.Dy())

	// Rasterised at the printer's resolution, nearest neighbour, as the reader will.
	page := nearestUpscale(src,
		int(math.Round(widthIn*PrinterDPI)),
		int(math.Round(heightIn*PrinterDPI)))

	// Bilevel device: anything that is not already ink or paper gets screened, with the colour
	// separations landing a dot or two apart from each other.
	page = halftone(page, int(math.Round(1.5*stress)))

	// Ink spreads into the fibre before anything reads it.
	page = blur(page, s.BlurInches*PrinterDPI*0.6*stress)

	// The scanner's aperture integrates over its sampling pitch.
	scanW := int(math.Round(widthIn * s.DPI))
	scanH := int(math.Round(heightIn * s.DPI))
	if scanW < 1 || scanH < 1 {
		return page
	}
	scan := boxDownsample(page, scanW, scanH)

	// Whatever optical softness is left, expressed in scan pixels.
	scan = blur(scan, s.BlurInches*s.DPI*0.5*stress)

	// The sheet does not lie flat. Amplitude in scan pixels, so a finer scan resolves the same physical
	// waviness as more pixels of it — which is what actually happens, and why a denser scan does not buy
	// as much as its resolution suggests.
	scan = cockle(scan, 0.0015*s.DPI*stress, 2.5)

	// The platen is larger than the print, and the sheet is never laid down perfectly square.
	scan = pad(scan, 0.04)
	if s.RotationDeg != 0 {
		scan = rotate(scan, s.RotationDeg*stress)
	}
	if s.NoiseSigma > 0 {
		scan = noise(scan, s.NoiseSigma*stress, 7)
	}
	if s.JPEGQuality > 0 {
		scan = jpegRoundTrip(scan, clampInt(s.JPEGQuality-int(10*(stress-1)), 40, 100))
	}
	return scan
}

// nearestUpscale resamples with nearest neighbour, which is what a PDF reader does to an image whose
// /Interpolate flag is absent.
func nearestUpscale(src *image.RGBA, dstW, dstH int) *image.RGBA {
	if dstW < 1 || dstH < 1 {
		return src
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := range dstH {
		sy := b.Min.Y + y*b.Dy()/dstH
		for x := range dstW {
			dst.SetRGBA(x, y, src.RGBAAt(b.Min.X+x*b.Dx()/dstW, sy))
		}
	}
	return dst
}

// bayer8 is an 8x8 ordered dither matrix. Ordered rather than error-diffused because that is what a
// laser's screening actually is — a fixed cell pattern, not a serial algorithm. Error diffusion would
// model an inkjet's better-behaved output and flatter the result.
var bayer8 = [8][8]float64{
	{0, 32, 8, 40, 2, 34, 10, 42},
	{48, 16, 56, 24, 50, 18, 58, 26},
	{12, 44, 4, 36, 14, 46, 6, 38},
	{60, 28, 52, 20, 62, 30, 54, 22},
	{3, 35, 11, 43, 1, 33, 9, 41},
	{51, 19, 59, 27, 49, 17, 57, 25},
	{15, 47, 7, 39, 13, 45, 5, 37},
	{63, 31, 55, 23, 61, 29, 53, 21},
}

// halftone reduces the page to ink or no ink, the way a bilevel printer must.
//
// Modelled as separations rather than as three independent RGB channels, because the difference decides
// the colour verdict entirely. A printer has no red ink: pure red is magenta and yellow laid down by two
// passes, screened at different angles and registered to each other only as well as the mechanism
// manages, so a colour cell's edges carry fringes of its component inks and the scanner averages those
// fringes back into the cell. Treating the printer as an RGB bilevel device makes color8 print perfectly,
// because its palette is the eight corners of the RGB cube and every corner is already "ink or no ink" in
// that fiction. On paper it is nothing of the sort.
//
// Neutral pixels are the exception, and a real one: grey component replacement means a printer renders
// R==G==B with the black separation alone — one pass, no registration error between separations. That is
// why binary pays almost nothing here and colour pays a great deal. Binary's two symbols are exactly the
// two things a printer can natively put on paper.
//
// misregisterDots is how far the colour separations sit from each other, in printer dots.
func halftone(src *image.RGBA, misregisterDots int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(b)

	// Each separation gets its own screen origin, standing in for the different screen angles a real RIP
	// uses to stop the separations moiring against each other.
	offsets := [3][2]int{{0, 0}, {misregisterDots, 1}, {1, misregisterDots}}

	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := src.RGBAAt(x, y)

			if c.R == c.G && c.G == c.B {
				// Neutral: one separation, no registration error.
				v := uint8(0)
				if float64(c.R)/255 > (bayer8[y&7][x&7]+0.5)/64 {
					v = 255
				}
				dst.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
				continue
			}

			var out [3]uint8
			for i := range 3 {
				sx := clampInt(x+offsets[i][0], b.Min.X, b.Max.X-1)
				sy := clampInt(y+offsets[i][1], b.Min.Y, b.Max.Y-1)
				p := src.RGBAAt(sx, sy)
				v := [3]uint8{p.R, p.G, p.B}[i]
				t := (bayer8[(y+offsets[i][1])&7][(x+offsets[i][0])&7] + 0.5) / 64
				if float64(v)/255 > t {
					out[i] = 255
				}
			}
			dst.SetRGBA(x, y, color.RGBA{R: out[0], G: out[1], B: out[2], A: 255})
		}
	}
	return dst
}

// boxDownsample averages every source pixel falling under a destination pixel.
//
// This is the scanner's aperture, and using it rather than a point-sampling resize is what makes the
// halftone modelling mean anything: a block of dithered dots integrates back to the grey it was dithered
// from, exactly as it does under real glass. A nearest-neighbour downsample would instead pick one dot and
// report pure ink or pure paper, which is not what a scanner sees at any resolution.
func boxDownsample(src *image.RGBA, dstW, dstH int) *image.RGBA {
	if dstW < 1 || dstH < 1 {
		return src
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := range dstH {
		y0, y1 := b.Min.Y+y*b.Dy()/dstH, b.Min.Y+(y+1)*b.Dy()/dstH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := range dstW {
			x0, x1 := b.Min.X+x*b.Dx()/dstW, b.Min.X+(x+1)*b.Dx()/dstW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, n float64
			for sy := y0; sy < y1 && sy < b.Max.Y; sy++ {
				for sx := x0; sx < x1 && sx < b.Max.X; sx++ {
					c := src.RGBAAt(sx, sy)
					r, g, bl, n = r+float64(c.R), g+float64(c.G), bl+float64(c.B), n+1
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{R: u8(r / n), G: u8(g / n), B: u8(bl / n), A: 255})
		}
	}
	return dst
}

// cockle warps the page slightly, modelling paper that is not perfectly flat under the glass.
//
// Small, smooth and non-uniform, which is the point: the decoder fits one homography per frame and a
// homography describes a plane. Without this the simulated page is geometrically perfect in a way no sheet
// on a platen ever is, and dense grids pass that nothing physical would.
func cockle(src *image.RGBA, amplitudePx, periods float64) *image.RGBA {
	if amplitudePx <= 0 {
		return src
	}
	b := src.Bounds()
	out := image.NewRGBA(b)
	w, h := float64(b.Dx()), float64(b.Dy())
	for y := range b.Dy() {
		for x := range b.Dx() {
			dx := amplitudePx * math.Sin(2*math.Pi*periods*float64(y)/h+0.7)
			dy := amplitudePx * 0.6 * math.Sin(2*math.Pi*periods*float64(x)/w*0.8+1.9)
			out.SetRGBA(x, y, bilinear(src, float64(x)+dx, float64(y)+dy))
		}
	}
	return out
}
