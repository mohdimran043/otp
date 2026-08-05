package protocol

import (
	"image"
	"image/color"
)

// GrayMap is an 8-bit luminance buffer. Decoding starts by flattening whatever
// the camera produced into one of these, so every later stage works on a single
// representation regardless of the source pixel format.
type GrayMap struct {
	W, H int
	Pix  []uint8 // len == W*H, row-major
}

// At returns the luminance at (x, y), or 0 outside the buffer.
func (g *GrayMap) At(x, y int) uint8 {
	if x < 0 || y < 0 || x >= g.W || y >= g.H {
		return 0
	}
	return g.Pix[y*g.W+x]
}

// Grayscale flattens an image to luminance using the Rec. 601 weights.
//
// The fast paths matter: a 1568x896 frame is 1.4 million pixels, and at 60 frames
// per second a generic At/RGBA conversion per pixel dominates decode time.
func Grayscale(img image.Image) *GrayMap {
	b := img.Bounds()
	g := &GrayMap{W: b.Dx(), H: b.Dy(), Pix: make([]uint8, b.Dx()*b.Dy())}

	switch src := img.(type) {
	case *image.Gray:
		for y := 0; y < g.H; y++ {
			copy(g.Pix[y*g.W:(y+1)*g.W], src.Pix[y*src.Stride:y*src.Stride+g.W])
		}
	case *image.RGBA:
		for y := 0; y < g.H; y++ {
			row := src.Pix[y*src.Stride:]
			out := g.Pix[y*g.W:]
			for x := 0; x < g.W; x++ {
				p := row[x*4:]
				out[x] = uint8((299*uint32(p[0]) + 587*uint32(p[1]) + 114*uint32(p[2])) / 1000)
			}
		}
	case *image.NRGBA:
		for y := 0; y < g.H; y++ {
			row := src.Pix[y*src.Stride:]
			out := g.Pix[y*g.W:]
			for x := 0; x < g.W; x++ {
				p := row[x*4:]
				out[x] = uint8((299*uint32(p[0]) + 587*uint32(p[1]) + 114*uint32(p[2])) / 1000)
			}
		}
	default:
		for y := 0; y < g.H; y++ {
			for x := 0; x < g.W; x++ {
				r, gg, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				g.Pix[y*g.W+x] = uint8((299*(r>>8) + 587*(gg>>8) + 114*(bb>>8)) / 1000)
			}
		}
	}
	return g
}

// Bitmap is a binarised image. A set bit means a bright cell, because the
// protocol renders bit 1 as white on a black field: an emissive display bleeds
// less light with a dark background, and it keeps the quiet zone from washing out
// the finder rings.
type Bitmap struct {
	W, H int
	Pix  []bool
}

// At reports whether (x, y) is bright, treating outside as dark.
func (b *Bitmap) At(x, y int) bool {
	if x < 0 || y < 0 || x >= b.W || y >= b.H {
		return false
	}
	return b.Pix[y*b.W+x]
}

// Gray renders the bitmap for debugging and for the receiver UI's overlay.
func (b *Bitmap) Gray() *image.Gray {
	out := image.NewGray(image.Rect(0, 0, b.W, b.H))
	for i, on := range b.Pix {
		if on {
			out.Pix[i] = 255
		}
	}
	return out
}

// binarizeBlock is the side length, in pixels, of the tiles used to estimate
// local thresholds.
const binarizeBlock = 16

// minBlockContrast is the luminance spread a tile needs before its own mean is
// trusted as a threshold. Flat tiles are all-background or all-foreground, and
// thresholding them against their own mean would turn sensor noise into speckle.
const minBlockContrast = 24

// Binarize converts luminance to a bitmap using locally adapted thresholds.
//
// A single global threshold fails on camera captures, because a display is
// brighter at the centre than at the corners and room light falls off unevenly.
// This estimates a threshold per tile, smooths the estimates across neighbouring
// tiles so tile seams do not become artefacts, and falls back to a neighbourhood
// estimate on tiles too flat to judge alone.
func Binarize(g *GrayMap) *Bitmap {
	bm := &Bitmap{W: g.W, H: g.H, Pix: make([]bool, g.W*g.H)}
	if g.W == 0 || g.H == 0 {
		return bm
	}

	bw := (g.W + binarizeBlock - 1) / binarizeBlock
	bh := (g.H + binarizeBlock - 1) / binarizeBlock

	means := make([]float64, bw*bh)
	spreads := make([]float64, bw*bh)
	var globalSum float64
	for by := 0; by < bh; by++ {
		for bx := 0; bx < bw; bx++ {
			x0, y0 := bx*binarizeBlock, by*binarizeBlock
			x1, y1 := min(x0+binarizeBlock, g.W), min(y0+binarizeBlock, g.H)
			var sum, n float64
			lo, hi := uint8(255), uint8(0)
			for y := y0; y < y1; y++ {
				row := g.Pix[y*g.W:]
				for x := x0; x < x1; x++ {
					v := row[x]
					sum += float64(v)
					n++
					if v < lo {
						lo = v
					}
					if v > hi {
						hi = v
					}
				}
			}
			if n == 0 {
				n = 1
			}
			means[by*bw+bx] = sum / n
			spreads[by*bw+bx] = float64(hi) - float64(lo)
			globalSum += sum / n
		}
	}
	globalMean := globalSum / float64(bw*bh)

	// Smooth the per-tile thresholds over a 3x3 neighbourhood. Low-contrast tiles
	// borrow from their neighbours instead of voting, so a blank region adjacent
	// to content inherits a sane threshold rather than inventing one.
	thresh := make([]float64, bw*bh)
	for by := 0; by < bh; by++ {
		for bx := 0; bx < bw; bx++ {
			var sum, n float64
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := bx+dx, by+dy
					if nx < 0 || ny < 0 || nx >= bw || ny >= bh {
						continue
					}
					sum += means[ny*bw+nx]
					n++
				}
			}
			local := sum / n
			if spreads[by*bw+bx] < minBlockContrast {
				// Nothing to separate inside this tile. Bias the threshold away
				// from the tile's own mean so a uniform tile resolves to a single
				// value instead of splitting on noise.
				if means[by*bw+bx] < globalMean {
					local = means[by*bw+bx] + minBlockContrast
				} else {
					local = means[by*bw+bx] - minBlockContrast
				}
			}
			thresh[by*bw+bx] = local
		}
	}

	for y := 0; y < g.H; y++ {
		by := y / binarizeBlock
		row := g.Pix[y*g.W:]
		out := bm.Pix[y*g.W:]
		for x := 0; x < g.W; x++ {
			out[x] = float64(row[x]) > thresh[by*bw+x/binarizeBlock]
		}
	}
	return bm
}

// Median3 applies a 3x3 median filter to luminance.
//
// It is the decoder's fallback when fiducial detection comes up short. A noisy
// sensor binarises into thousands of one- and two-pixel speckles, and speckle
// lands inside a fiducial's separator ring as often as anywhere else, bridging the
// core to the outer ring so the structure no longer reads as a ring. A median is
// the right tool rather than a blur: it removes isolated outliers outright while
// leaving the straight, high-contrast cell edges the protocol depends on exactly
// where they were.
func Median3(g *GrayMap) *GrayMap {
	if g.W < 3 || g.H < 3 {
		return g
	}
	out := &GrayMap{W: g.W, H: g.H, Pix: make([]uint8, len(g.Pix))}
	var window [9]uint8
	for y := 0; y < g.H; y++ {
		for x := 0; x < g.W; x++ {
			n := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					px, py := x+dx, y+dy
					if px < 0 || py < 0 || px >= g.W || py >= g.H {
						continue
					}
					window[n] = g.Pix[py*g.W+px]
					n++
				}
			}
			// Insertion sort: nine elements, so this beats any general sort.
			for i := 1; i < n; i++ {
				v := window[i]
				j := i - 1
				for j >= 0 && window[j] > v {
					window[j+1] = window[j]
					j--
				}
				window[j+1] = v
			}
			out.Pix[y*g.W+x] = window[n/2]
		}
	}
	return out
}

// Component is one connected run of bright pixels.
type Component struct {
	Label int
	Area  int
	MinX  int
	MinY  int
	MaxX  int
	MaxY  int
	SumX  float64
	SumY  float64
}

// Width is the bounding-box width.
func (c Component) Width() int { return c.MaxX - c.MinX + 1 }

// Height is the bounding-box height.
func (c Component) Height() int { return c.MaxY - c.MinY + 1 }

// Centroid is the area-weighted centre, which is stabler than the bounding-box
// centre when blur eats asymmetrically into an edge.
func (c Component) Centroid() Point {
	if c.Area == 0 {
		return Point{}
	}
	return Point{c.SumX / float64(c.Area), c.SumY / float64(c.Area)}
}

// BoxCenter is the bounding-box centre.
func (c Component) BoxCenter() Point {
	return Point{float64(c.MinX+c.MaxX) / 2, float64(c.MinY+c.MaxY) / 2}
}

// Labeling is the result of connected-component analysis.
type Labeling struct {
	W, H       int
	Labels     []int32 // 0 means background
	Components []Component
}

// LabelAt returns the label at (x, y), or 0 outside the image.
func (l *Labeling) LabelAt(x, y int) int32 {
	if x < 0 || y < 0 || x >= l.W || y >= l.H {
		return 0
	}
	return l.Labels[y*l.W+x]
}

// ComponentAt returns the component covering (x, y), or nil for background.
func (l *Labeling) ComponentAt(x, y int) *Component {
	lb := l.LabelAt(x, y)
	if lb == 0 {
		return nil
	}
	return &l.Components[lb-1]
}

// LabelComponents finds eight-connected runs of bright pixels.
//
// Eight-connectivity rather than four matters for the finder rings: a ring
// thinned by blur or a slightly rotated capture can end up joined only
// diagonally, and four-connectivity would split it into arcs.
func LabelComponents(bm *Bitmap) *Labeling {
	l := &Labeling{W: bm.W, H: bm.H, Labels: make([]int32, bm.W*bm.H)}
	stack := make([]int32, 0, 1024)

	for start := 0; start < len(bm.Pix); start++ {
		if !bm.Pix[start] || l.Labels[start] != 0 {
			continue
		}
		label := int32(len(l.Components) + 1)
		c := Component{
			Label: int(label),
			MinX:  bm.W, MinY: bm.H,
			MaxX: 0, MaxY: 0,
		}

		stack = stack[:0]
		stack = append(stack, int32(start))
		l.Labels[start] = label

		for len(stack) > 0 {
			idx := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := int(idx)%bm.W, int(idx)/bm.W

			c.Area++
			c.SumX += float64(x)
			c.SumY += float64(y)
			if x < c.MinX {
				c.MinX = x
			}
			if x > c.MaxX {
				c.MaxX = x
			}
			if y < c.MinY {
				c.MinY = y
			}
			if y > c.MaxY {
				c.MaxY = y
			}

			for dy := -1; dy <= 1; dy++ {
				ny := y + dy
				if ny < 0 || ny >= bm.H {
					continue
				}
				for dx := -1; dx <= 1; dx++ {
					nx := x + dx
					if (dx == 0 && dy == 0) || nx < 0 || nx >= bm.W {
						continue
					}
					n := int32(ny*bm.W + nx)
					if bm.Pix[n] && l.Labels[n] == 0 {
						l.Labels[n] = label
						stack = append(stack, n)
					}
				}
			}
		}
		l.Components = append(l.Components, c)
	}
	return l
}

// grayAt is a small helper for callers holding an image rather than a GrayMap.
func grayAt(img image.Image, x, y int) uint8 {
	r, g, b, _ := img.At(x, y).RGBA()
	return uint8((299*(r>>8) + 587*(g>>8) + 114*(b>>8)) / 1000)
}

// SampleColor averages a square neighbourhood around a point and returns the
// mean colour. Averaging rather than point-sampling is what makes the decoder
// tolerant of sensor noise: a single hot pixel shifts a 5x5 mean by four percent
// instead of deciding the cell outright.
func SampleColor(img image.Image, p Point, radius int) color.RGBA {
	b := img.Bounds()
	cx, cy := int(p.X+0.5), int(p.Y+0.5)
	var sr, sg, sb, n uint32
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			x, y := cx+dx, cy+dy
			if x < b.Min.X || y < b.Min.Y || x >= b.Max.X || y >= b.Max.Y {
				continue
			}
			r, g, bb, _ := img.At(x, y).RGBA()
			sr += r >> 8
			sg += g >> 8
			sb += bb >> 8
			n++
		}
	}
	if n == 0 {
		return color.RGBA{A: 255}
	}
	return color.RGBA{R: uint8(sr / n), G: uint8(sg / n), B: uint8(sb / n), A: 255}
}

// SampleLuma averages luminance over a square neighbourhood.
func SampleLuma(g *GrayMap, p Point, radius int) uint8 {
	cx, cy := int(p.X+0.5), int(p.Y+0.5)
	var sum, n int
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= g.W || y >= g.H {
				continue
			}
			sum += int(g.Pix[y*g.W+x])
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return uint8(sum / n)
}
