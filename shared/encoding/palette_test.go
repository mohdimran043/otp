package encoding_test

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
)

// palettes is every palette the encodings use.
func palettes() []encoding.Palette {
	return []encoding.Palette{
		encoding.BinaryPalette,
		encoding.GrayPalette(2),
		encoding.GrayPalette(3),
		encoding.Color8Palette,
		encoding.Color16Palette,
	}
}

// TestPaletteSeparation holds each palette to a floor on its closest pair.
//
// The closest pair is the whole story for a nearest-neighbour decoder: that pair
// is where a noisy sensor starts confusing symbols, and every other pair is
// irrelevant until it does. A change that made a palette prettier or more evenly
// spread while bringing two entries closer together would be a straight
// regression in reach, and this is the test that catches it.
func TestPaletteSeparation(t *testing.T) {
	// Floors are set just under what the current constructions achieve, so a real
	// regression trips them but an improvement does not.
	//
	// color8's figure looks poor beside its behaviour in practice, and the reason is
	// instructive: its closest pair is black against blue, which differ in the blue
	// channel alone — the channel the luminance weighting discounts to a ninth of
	// green's, because that is how much less of it a sensor resolves. It takes an
	// almost purely blue error to confuse them, which is not what a defocused lens
	// or a dim room produces, and is why color8 still holds at the edge of the
	// optical envelope while the grey ramp does not.
	floors := map[string]float64{
		"binary":    250,
		"grayscale": 30,
		"color8":    85,
		"color16":   40,
	}

	var table strings.Builder
	table.WriteString("\npalette noise margin (weighted RGB distance between the closest pair):\n")

	for _, p := range palettes() {
		sep := p.MinSeparation()
		fmt.Fprintf(&table, "  %-10s %2d entries, %d bits/cell, closest pair %6.1f\n",
			p.Name, len(p.Colors), p.Bits(), sep)

		floor, ok := floors[p.Name]
		require.True(t, ok, "palette %q has no documented margin floor", p.Name)
		require.GreaterOrEqual(t, sep, floor,
			"%s: closest pair %.1f is below the documented floor of %.1f", p.Name, sep, floor)
	}
	t.Log(table.String())
}

// TestPaletteRoundTripsEverySymbol is the minimum a palette must do: every symbol
// it can encode must be the nearest match to the colour it encodes to. A palette
// with a duplicate entry would silently alias two symbols, and nothing downstream
// would report it as anything but payload corruption.
func TestPaletteRoundTripsEverySymbol(t *testing.T) {
	for _, p := range palettes() {
		t.Run(p.Name+fmt.Sprint(p.Bits()), func(t *testing.T) {
			require.Equal(t, 1<<p.Bits(), len(p.Colors),
				"a palette must be exactly a power of two so every symbol is representable")

			for v := 0; v < len(p.Colors); v++ {
				require.Equal(t, uint32(v), p.Value(p.Color(uint32(v))),
					"%s: symbol %d does not survive its own colour", p.Name, v)
			}
		})
	}
}

// TestPaletteToleratesNoise measures how far a sample can drift before it is
// misread, which is the figure the operating envelope is built on.
func TestPaletteToleratesNoise(t *testing.T) {
	var table strings.Builder
	table.WriteString("\nlargest uniform per-channel drift every symbol still survives:\n")

	for _, p := range palettes() {
		worst := 255
		for v := 0; v < len(p.Colors); v++ {
			base := p.Color(uint32(v))
			for _, sign := range []int{1, -1} {
				drift := 0
				for ; drift < 255; drift++ {
					shifted := color.RGBA{
						R: shift(base.R, sign*drift),
						G: shift(base.G, sign*drift),
						B: shift(base.B, sign*drift),
						A: 255,
					}
					if p.Value(shifted) != uint32(v) {
						break
					}
				}
				if drift < worst {
					worst = drift
				}
			}
		}
		fmt.Fprintf(&table, "  %-10s %d bits/cell, survives ±%d levels\n", p.Name, p.Bits(), worst)

		// Binary tolerates any drift that does not cross the midpoint; the dense
		// palettes have to tolerate at least the sensor noise the Typical profile
		// applies, or they would have no business being offered at all.
		require.GreaterOrEqual(t, worst, 6,
			"%s must survive the sensor noise of a normal installation", p.Name)
	}
	t.Log(table.String())
}

func shift(v uint8, by int) uint8 {
	n := int(v) + by
	if n < 0 {
		return 0
	}
	if n > 255 {
		return 255
	}
	return uint8(n)
}

// TestGrayPaletteSpansFullRange checks the ramp reaches both ends. A ramp that
// stopped short of black or white would waste the margin the encoding is
// specifically trading away density for.
func TestGrayPaletteSpansFullRange(t *testing.T) {
	for _, bits := range []int{2, 3} {
		p := encoding.GrayPalette(bits)
		require.Equal(t, uint8(0), p.Colors[0].R)
		require.Equal(t, uint8(255), p.Colors[len(p.Colors)-1].R)

		for _, c := range p.Colors {
			require.Equal(t, c.R, c.G, "a grey has equal channels")
			require.Equal(t, c.R, c.B, "a grey has equal channels")
		}
	}
}

// TestBandChecksumMatchesDescriptor holds the rolling encoding's band checksum to
// the same function the grid descriptor uses, so the protocol has one sixteen-bit
// checksum rather than two that merely look alike.
func TestBandChecksumMatchesDescriptor(t *testing.T) {
	// The canonical check value for CRC-16/CCITT-FALSE, the same vector the
	// descriptor's own test pins.
	require.Equal(t, uint16(0x29B1), encoding.CRC16ForTest([]byte("123456789")))
}

// TestValueWithMarginAgreesWithValue is the property that makes the margin safe to add: the
// symbol chosen must be identical to the one Value chooses, or a recovery layer built on it
// would silently disagree with the decoder it is trying to help.
func TestValueWithMarginAgreesWithValue(t *testing.T) {
	for _, p := range palettes() {
		t.Run(p.Name, func(t *testing.T) {
			for r := 0; r < 256; r += 17 {
				for g := 0; g < 256; g += 17 {
					for b := 0; b < 256; b += 17 {
						c := color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
						best, second, margin := p.ValueWithMargin(c)
						require.Equal(t, p.Value(c), best, "sample %v", c)
						require.NotEqual(t, best, second, "sample %v", c)
						require.GreaterOrEqual(t, margin, 0.0, "sample %v", c)
					}
				}
			}
		})
	}
}

// TestValueWithMarginZeroBetweenEntries checks the case the whole idea rests on: a sample
// exactly between two palette entries is a coin toss, and must report a margin of nearly zero
// so a candidate search ranks it first.
func TestValueWithMarginZeroBetweenEntries(t *testing.T) {
	p := encoding.Color8Palette
	a, b := p.Colors[0], p.Colors[1]
	mid := color.RGBA{
		R: uint8((int(a.R) + int(b.R)) / 2),
		G: uint8((int(a.G) + int(b.G)) / 2),
		B: uint8((int(a.B) + int(b.B)) / 2),
		A: 255,
	}
	_, _, margin := p.ValueWithMargin(mid)
	require.Less(t, margin, 1.0, "a midpoint sample should have almost no margin")
}

// TestValueWithMarginAtPaletteEntry checks the other end: an exact palette colour is maximally
// confident, and its margin is at least the palette's own separation figure.
func TestValueWithMarginAtPaletteEntry(t *testing.T) {
	for _, p := range []encoding.Palette{encoding.Color8Palette, encoding.GrayPalette(2)} {
		t.Run(p.Name, func(t *testing.T) {
			for i, c := range p.Colors {
				best, _, margin := p.ValueWithMargin(c)
				require.Equal(t, uint32(i), best)
				require.GreaterOrEqual(t, margin, p.MinSeparation()-1e-9)
			}
		})
	}
}
