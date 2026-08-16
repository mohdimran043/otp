package pipeline

import (
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// shotOf builds one photograph's reading: the colour each cell came back as, and the symbol that
// colour was nearest to.
func shotOf(pal encoding.Palette, seen []color.RGBA) *encoding.SoftReading {
	r := &encoding.SoftReading{
		Symbols:     make([]uint32, len(seen)),
		Cells:       make([]encoding.SoftCell, len(seen)),
		Palette:     pal,
		BitsPerCell: pal.Bits(),
	}
	for i, c := range seen {
		best, second, margin := pal.ValueWithMargin(c)
		r.Symbols[i] = best
		r.Cells[i] = encoding.SoftCell{
			Index: i, Cell: protocol.Cell{X: i, Y: 0},
			Symbol: best, Second: second, Margin: margin, Normalised: c,
		}
	}
	return r
}

// The whole claim, in one test: several photographs that each read a cell wrongly, combining to read
// it correctly.
//
// The cell is white. Every photograph is pulled toward a different neighbouring palette entry hard
// enough that each one alone reads it as that neighbour — one says red, one says green, one says
// blue. No majority exists to vote with, and any two of them agree on nothing. Their mean is white.
func TestCombiningReadsACellNoSingleShotCould(t *testing.T) {
	pal := encoding.Color8Palette
	white := uint32(7) // the palette's last entry, 255/255/255

	pulled := []color.RGBA{
		{R: 255, G: 90, B: 90, A: 255}, // reads red
		{R: 90, G: 255, B: 90, A: 255}, // reads green
		{R: 90, G: 90, B: 255, A: 255}, // reads blue
	}

	m := newMerger()
	var last mergeResult
	for i, seen := range pulled {
		shot := shotOf(pal, []color.RGBA{seen})
		require.NotEqual(t, white, shot.Symbols[0],
			"photograph %d must read this cell wrongly on its own, or the test proves nothing", i)
		last = m.Add(1, shot)
	}

	require.Equal(t, 3, last.Shots)
	assert.Equal(t, white, last.Symbols[0],
		"three photographs that each read the cell wrongly should average to the right answer")
}

// Photographs of different displayed frames must never be averaged together.
//
// They are different pictures. Combining them would produce a reading of nothing, and — worse — one
// that still verifies against a footer if it happened to land, which would deliver wrong bytes.
func TestCombiningKeepsFramesApart(t *testing.T) {
	pal := encoding.Color8Palette
	m := newMerger()

	m.Add(1, shotOf(pal, []color.RGBA{{R: 255, A: 255}}))
	other := m.Add(2, shotOf(pal, []color.RGBA{{B: 255, A: 255}}))

	assert.Equal(t, 1, other.Shots, "a different frame number starts its own accumulator")
}

// A photograph that read a cell poorly moves the mean less than one that read it cleanly.
//
// This is what averaging the colours gives that voting on the symbols cannot. Two photographs land
// nearly midway between red and white — each nominally "reads red", but only barely. One lands on
// white cleanly. As votes it is two to one for red and the cell is wrong; as colours the two
// indecisive readings sit near the midpoint and pull the mean far less than the decisive one, so the
// answer follows the evidence rather than the head count.
//
// Note this falls out of the plain mean rather than being weighted in. Weighting by margin was tried
// and removed as circular — see the comment in Add.
func TestCombiningFollowsTheEvidenceNotTheHeadCount(t *testing.T) {
	pal := encoding.Color8Palette
	m := newMerger()

	// Just on red's side of the midpoint between red and white.
	indecisive := color.RGBA{R: 255, G: 120, B: 120, A: 255}
	decisive := color.RGBA{R: 255, G: 255, B: 255, A: 255}

	red, _, _ := pal.ValueWithMargin(indecisive)
	white, _, _ := pal.ValueWithMargin(decisive)
	require.NotEqual(t, red, white, "the fixture must actually disagree, or this proves nothing")

	m.Add(1, shotOf(pal, []color.RGBA{indecisive}))
	m.Add(1, shotOf(pal, []color.RGBA{decisive}))
	got := m.Add(1, shotOf(pal, []color.RGBA{decisive}))

	assert.Equal(t, white, got.Symbols[0],
		"two clean readings should carry the cell over one that was nearly a coin toss")
}

// A geometry change mid-run must start a fresh accumulator rather than average across two shapes.
func TestCombiningRestartsWhenTheGridChanges(t *testing.T) {
	pal := encoding.Color8Palette
	m := newMerger()

	m.Add(1, shotOf(pal, []color.RGBA{{R: 255, A: 255}, {R: 255, A: 255}}))
	after := m.Add(1, shotOf(pal, []color.RGBA{{R: 255, A: 255}}))

	assert.Equal(t, 1, after.Shots, "a different cell count is a different geometry, so start again")
	assert.Len(t, after.Symbols, 1)
}

// Forgetting a frame releases it, so a long transmission does not accumulate every frame it ever saw.
func TestCombiningForgetsAFrameOnceRead(t *testing.T) {
	pal := encoding.Color8Palette
	m := newMerger()

	m.Add(9, shotOf(pal, []color.RGBA{{R: 255, A: 255}}))
	m.Forget(9)
	again := m.Add(9, shotOf(pal, []color.RGBA{{R: 255, A: 255}}))

	assert.Equal(t, 1, again.Shots, "a forgotten frame starts over")
}
