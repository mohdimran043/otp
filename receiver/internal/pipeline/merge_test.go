package pipeline

import (
	"github.com/google/uuid"
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
		last = m.Add(oneFrame(1), shot)
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

	m.Add(oneFrame(1), shotOf(pal, []color.RGBA{{R: 255, A: 255}}))
	other := m.Add(oneFrame(2), shotOf(pal, []color.RGBA{{B: 255, A: 255}}))

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

	m.Add(oneFrame(1), shotOf(pal, []color.RGBA{indecisive}))
	m.Add(oneFrame(1), shotOf(pal, []color.RGBA{decisive}))
	got := m.Add(oneFrame(1), shotOf(pal, []color.RGBA{decisive}))

	assert.Equal(t, white, got.Symbols[0],
		"two clean readings should carry the cell over one that was nearly a coin toss")
}

// A geometry change mid-run must start a fresh accumulator rather than average across two shapes.
func TestCombiningRestartsWhenTheGridChanges(t *testing.T) {
	pal := encoding.Color8Palette
	m := newMerger()

	m.Add(oneFrame(1), shotOf(pal, []color.RGBA{{R: 255, A: 255}, {R: 255, A: 255}}))
	after := m.Add(oneFrame(1), shotOf(pal, []color.RGBA{{R: 255, A: 255}}))

	assert.Equal(t, 1, after.Shots, "a different cell count is a different geometry, so start again")
	assert.Len(t, after.Symbols, 1)
}

// Forgetting a frame releases it, so a long transmission does not accumulate every frame it ever saw.
func TestCombiningForgetsAFrameOnceRead(t *testing.T) {
	pal := encoding.Color8Palette
	m := newMerger()

	m.Add(oneFrame(9), shotOf(pal, []color.RGBA{{R: 255, A: 255}}))
	m.Forget(oneFrame(9))
	again := m.Add(oneFrame(9), shotOf(pal, []color.RGBA{{R: 255, A: 255}}))

	assert.Equal(t, 1, again.Shots, "a forgotten frame starts over")
}

// oneFrame keys a shot within a single notional transfer, which is what these tests are about. The
// transmission is fixed rather than zero so a test asserting that two transfers stay apart cannot pass
// by everything sharing the nil UUID.
func oneFrame(n uint64) shotKey {
	return shotKey{transmission: mergeTestTransmission, frame: n}
}

var mergeTestTransmission = uuid.MustParse("11111111-1111-1111-1111-111111111111")

// Two transfers on the display at once must not merge into each other.
//
// Every transfer numbers its frames from zero, so with two files in flight there is a frame 5 in each.
// Keyed on the frame number alone they shared an accumulator and were averaged as though they were two
// photographs of one picture, which they are not — the mean of two unrelated frames reads as neither.
//
// It could never deliver wrong bytes: a merged reading is still checked against the footer's CRC32 and
// SHA-256. What it did was quietly disable merging whenever two transfers overlapped, each poisoning
// the other's evidence, so one of the two mechanisms that rescue a marginal frame stopped working
// exactly when the display was busiest.
func TestTwoTransfersDoNotMergeIntoEachOther(t *testing.T) {
	pal := encoding.Color8Palette
	fileA := uuid.MustParse("aaaaaaaa-1111-2222-3333-444444444444")
	fileB := uuid.MustParse("bbbbbbbb-5555-6666-7777-888888888888")

	m := newMerger()

	// The same frame number in both transfers, reading opposite colours.
	red := m.Add(shotKey{transmission: fileA, frame: 5},
		shotOf(pal, []color.RGBA{{R: 255, A: 255}}))
	blue := m.Add(shotKey{transmission: fileB, frame: 5},
		shotOf(pal, []color.RGBA{{B: 255, A: 255}}))

	require.Equal(t, 1, red.Shots, "the first transfer has one photograph of its frame 5")
	require.Equal(t, 1, blue.Shots,
		"the second transfer's frame 5 is a different picture and must start its own accumulator")
	assert.NotEqual(t, red.Symbols[0], blue.Symbols[0],
		"two transfers' frame 5 must read as themselves rather than as each other's mean")

	// And a second photograph of the first transfer's frame still accumulates against its own.
	again := m.Add(shotKey{transmission: fileA, frame: 5},
		shotOf(pal, []color.RGBA{{R: 255, A: 255}}))
	assert.Equal(t, 2, again.Shots, "the first transfer's own second shot should still combine")
}

// Forgetting one transfer's frame leaves the other transfer's frame of the same number alone.
func TestForgettingOneTransfersFrameKeepsTheOthers(t *testing.T) {
	pal := encoding.Color8Palette
	fileA := uuid.MustParse("aaaaaaaa-1111-2222-3333-444444444444")
	fileB := uuid.MustParse("bbbbbbbb-5555-6666-7777-888888888888")

	m := newMerger()
	m.Add(shotKey{transmission: fileA, frame: 9}, shotOf(pal, []color.RGBA{{R: 255, A: 255}}))
	m.Add(shotKey{transmission: fileB, frame: 9}, shotOf(pal, []color.RGBA{{B: 255, A: 255}}))

	// A read frame is forgotten so it stops accumulating. It must take only its own with it.
	m.Forget(shotKey{transmission: fileA, frame: 9})

	kept := m.Add(shotKey{transmission: fileB, frame: 9}, shotOf(pal, []color.RGBA{{B: 255, A: 255}}))
	assert.Equal(t, 2, kept.Shots,
		"the other transfer's frame 9 should still hold the photograph it already had")
}
