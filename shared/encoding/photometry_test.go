package encoding_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// TestPhotometricEnvelope maps how much the optical path may distort brightness
// before each encoding stops decoding.
//
// Three distortions are swept separately, because they fail for different reasons.
//
// Corner falloff is removed outright, at any severity the sweep can produce: the
// scaffold measures both reference levels across the frame and the fitted model
// reproduces the bowl. Nothing here is a limit on the decoder.
//
// A flat brightness shift is removed too, right up to the point where it clips.
// Past that, the distortion is not a distortion but a loss: shifted down far
// enough, the grey ramp's two darkest levels both land on zero and are no longer
// two levels. Nothing recovers them, and the ramp that fails first is the one
// whose levels were closest together to begin with.
//
// Gamma fails the same way for a subtler reason. The model is linear, so it can
// rescale the range but not restore its shape, and a power curve does not merely
// move the levels — it crowds them together at one end. Correcting that would need
// a third reference level, which a frame does not carry. So what survives gamma is
// whatever margin the palette had spare, and the three-bit ramp had the least.
func TestPhotometricEnvelope(t *testing.T) {
	l := testLayout(t)

	type sweep struct {
		name    string
		values  []float64
		profile func(float64) simulate.Profile
	}
	sweeps := []sweep{
		{
			name:   "vignette",
			values: []float64{0, 0.2, 0.4, 0.6, 0.8},
			profile: func(v float64) simulate.Profile {
				return simulate.Profile{Vignette: v, Pad: 0.05, Seed: 31}
			},
		},
		{
			name:   "brightness",
			values: []float64{-80, -40, 0, 40, 80},
			profile: func(v float64) simulate.Profile {
				return simulate.Profile{Brightness: v, Pad: 0.05, Seed: 32}
			},
		},
		{
			name:   "gamma",
			values: []float64{0.6, 0.8, 1.0, 1.4, 1.8},
			profile: func(v float64) simulate.Profile {
				return simulate.Profile{Gamma: v, Pad: 0.05, Seed: 33}
			},
		},
	}

	type key struct {
		variant, sweep string
		value          float64
	}
	outcome := map[key]bool{}

	var table strings.Builder
	for _, s := range sweeps {
		fmt.Fprintf(&table, "\n%s, decode success:\n  encoding      ", s.name)
		for _, v := range s.values {
			fmt.Fprintf(&table, " %6.2f", v)
		}
		table.WriteString("\n")

		for _, v := range variants(t) {
			frame := frameFor(t, v, l, 23)
			base, err := v.enc.Encode(frame, l, v.depth)
			require.NoError(t, err)

			fmt.Fprintf(&table, "  %-14s", v.name)
			for _, value := range s.values {
				_, err := encoding.Decode(s.profile(value).Apply(base), protocol.LocateOptions{})
				ok := err == nil
				outcome[key{v.name, s.name, value}] = ok
				if ok {
					table.WriteString("      +")
				} else {
					table.WriteString("      .")
				}
			}
			table.WriteString("\n")
		}
	}
	t.Log(table.String())

	// Corner falloff is modelled and removed, so every encoding must survive far
	// more of it than any real lens produces. This is the assertion that fails if
	// the radial term is dropped and the model goes back to interpolating between
	// the corners, which cannot represent a frame whose middle is its brightest
	// part.
	for _, v := range variants(t) {
		require.True(t, outcome[key{v.name, "vignette", 0.8}],
			"%s: corner falloff is fitted out of the frame, at any depth", v.name)
	}

	// Short of clipping, a brightness shift and a mild gamma curve are both
	// absorbed. The three-bit ramp is excluded because it is the one encoding whose
	// levels are close enough together that a third of the range in either
	// direction merges two of them — a loss of information rather than a
	// distortion of it, and the documented reason it is offered only for a
	// controlled installation.
	for _, v := range variants(t) {
		if v.name == "grayscale/d3" {
			continue
		}
		for _, b := range []float64{-40, 40} {
			require.True(t, outcome[key{v.name, "brightness", b}],
				"%s: a flat shift of %.0f levels is fitted out of the frame", v.name, b)
		}
		require.True(t, outcome[key{v.name, "gamma", 1.4}],
			"%s must tolerate a mild gamma curve", v.name)
	}

	// The grey ramp at three bits still has to work when the panel is behaving,
	// since that is what it is offered for.
	require.True(t, outcome[key{"grayscale/d3", "gamma", 1.0}])
	require.True(t, outcome[key{"grayscale/d3", "vignette", 0.8}])
}
