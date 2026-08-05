package encoding

import (
	"image/color"
	"math"
)

// Palette maps symbol values to colours and back.
//
// Decoding is nearest-neighbour in RGB, which is why the palettes below are built
// to maximise the *minimum* distance between any two entries rather than to look
// even. What limits a palette is its closest pair: that pair is where a noisy
// sensor first starts confusing symbols, and every other pair is irrelevant until
// it does. TestPaletteSeparation holds each palette to a floor on that figure.
type Palette struct {
	Name   string
	Colors []color.RGBA
}

// Bits is how many bits one cell carries under this palette.
func (p Palette) Bits() int {
	n := len(p.Colors)
	bits := 0
	for 1<<bits < n {
		bits++
	}
	return bits
}

// Color returns the colour for a symbol value.
func (p Palette) Color(v uint32) color.RGBA {
	if int(v) >= len(p.Colors) {
		return p.Colors[len(p.Colors)-1]
	}
	return p.Colors[v]
}

// Value returns the symbol whose colour is nearest the sample.
//
// Distances are weighted by the luminance response of the eye — or rather, of a
// camera sensor, which is close enough. An unweighted distance treats a shift in
// blue as seriously as the same shift in green, but sensors resolve green far
// better, so unweighted matching throws away real discriminating power.
func (p Palette) Value(c color.RGBA) uint32 {
	best, bestDist := 0, math.Inf(1)
	for i, ref := range p.Colors {
		dr := float64(c.R) - float64(ref.R)
		dg := float64(c.G) - float64(ref.G)
		db := float64(c.B) - float64(ref.B)
		d := 0.299*dr*dr + 0.587*dg*dg + 0.114*db*db
		if d < bestDist {
			best, bestDist = i, d
		}
	}
	return uint32(best)
}

// MinSeparation returns the smallest weighted distance between any two entries,
// which is the palette's noise margin.
func (p Palette) MinSeparation() float64 {
	best := math.Inf(1)
	for i := 0; i < len(p.Colors); i++ {
		for j := i + 1; j < len(p.Colors); j++ {
			a, b := p.Colors[i], p.Colors[j]
			dr := float64(a.R) - float64(b.R)
			dg := float64(a.G) - float64(b.G)
			db := float64(a.B) - float64(b.B)
			d := math.Sqrt(0.299*dr*dr + 0.587*dg*dg + 0.114*db*db)
			if d < best {
				best = d
			}
		}
	}
	return best
}

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{R: r, G: g, B: b, A: 255} }

// BinaryPalette is one bit per cell: the widest possible margin, and the
// modulation every fixed band uses.
var BinaryPalette = Palette{
	Name:   "binary",
	Colors: []color.RGBA{rgb(0, 0, 0), rgb(255, 255, 255)},
}

// GrayPalette builds an evenly spaced grey ramp of 2^bits levels.
//
// Grey trades margin for density on a monochrome camera, which is what industrial
// sensors usually are. Three bits gives eight levels only 36 apart, so it needs
// good optics; two bits leaves 85 between levels and is the practical choice.
func GrayPalette(bits int) Palette {
	n := 1 << bits
	colors := make([]color.RGBA, n)
	for i := 0; i < n; i++ {
		v := uint8(i * 255 / (n - 1))
		colors[i] = rgb(v, v, v)
	}
	return Palette{Name: "grayscale", Colors: colors}
}

// Color8Palette is the eight corners of the RGB cube: three bits per cell with
// every channel fully on or fully off, so no two entries share a channel value.
// Nothing denser keeps this much margin.
var Color8Palette = Palette{
	Name: "color8",
	Colors: []color.RGBA{
		rgb(0, 0, 0),
		rgb(255, 0, 0),
		rgb(0, 255, 0),
		rgb(0, 0, 255),
		rgb(0, 255, 255),
		rgb(255, 0, 255),
		rgb(255, 255, 0),
		rgb(255, 255, 255),
	},
}

// Color16Palette is four bits per cell: four grey levels and twelve fully
// saturated hues.
//
// The obvious construction — the eight cube corners at two brightnesses — does
// not work, because a corner at half brightness collides with a different corner
// at full brightness, and black is black at any brightness. Separating hue from
// luminance instead keeps every pair apart: the greys differ in luminance, the
// hues differ in chroma, and the two groups differ in saturation.
var Color16Palette = buildColor16()

func buildColor16() Palette {
	colors := make([]color.RGBA, 0, 16)
	for _, v := range []uint8{0, 85, 170, 255} {
		colors = append(colors, rgb(v, v, v))
	}
	for h := 0; h < 12; h++ {
		colors = append(colors, hueColor(float64(h)*30))
	}
	return Palette{Name: "color16", Colors: colors}
}

// hueColor returns a fully saturated, fully bright colour at the given hue.
func hueColor(deg float64) color.RGBA {
	h := math.Mod(deg, 360) / 60
	i := int(h)
	f := h - float64(i)
	up := uint8(f*255 + 0.5)
	down := uint8((1-f)*255 + 0.5)
	switch i {
	case 0:
		return rgb(255, up, 0)
	case 1:
		return rgb(down, 255, 0)
	case 2:
		return rgb(0, 255, up)
	case 3:
		return rgb(0, down, 255)
	case 4:
		return rgb(up, 0, 255)
	default:
		return rgb(255, 0, down)
	}
}
