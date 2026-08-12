// Package cellpatch samples a small patch around one grid cell, in cell coordinates.
//
// It lives in shared/ for one reason, and it is the reason that matters most for a learned model: the
// feature extraction used to *train* and the feature extraction used at *inference* must be identical to
// the last bilinear weight. A model is a function of its input representation, and two implementations of
// that representation — one in the sender's dataset exporter, one in the receiver's inference path — would
// drift, silently, and the symptom would be a model that scored well in training and performs worse than
// the rule it replaced. One implementation, imported by both, removes the possibility.
//
// Patches are sampled through the homography, in cell coordinates rather than pixel coordinates. That
// makes them invariant to scale, rotation and perspective for free: a cell photographed at four pixels and
// one at twelve produce the same shape of input, so one model covers every geometry instead of one model
// per geometry.
//
// What a patch carries that a single sample does not is context. The decoder reads a cell by averaging a
// window at its centre and matching against eight palette entries, which is optimal against zero-mean
// noise on an isolated sample and is not what the channel does: at four pixels a cell the neighbours bleed
// into the centre, so the colour there is a mixture whose composition depends on what surrounds the cell.
// A patch spanning one and a half cells carries the surroundings, so a learned function can undo the
// mixing. A distance metric on one number cannot, however good the palette.
package cellpatch

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"

	"github.com/opticaltransport/otp/shared/protocol"
)

// PatchSide is the number of samples taken across one cell patch.
//
// Nine, spanning one and a half cells, so the patch covers the cell and half of each neighbour. That is
// the span that carries the bleed: less and the context is missing, more and the model spends capacity on
// cells two away, whose influence at any realistic blur is negligible.
const PatchSide = 9

// PatchSpan is how many cell widths the patch covers, centred on the cell.
const PatchSpan = 1.5

// Channels is the sample depth: red, green, blue.
const Channels = 3

// RecordBytes is the fixed size of one exported record: the patch, the frame's photometric reference, and
// the label.
//
// Fixed-size records with no framing so that numpy can read the whole file with one fromfile call and
// reshape it. A self-describing format would be tidier and would also mean writing a reader.
const RecordBytes = PatchSide*PatchSide*Channels + 6*4 + 4 + 1

// Record is one labelled cell.
type Record struct {
	// Patch is PatchSide x PatchSide x Channels raw samples, row-major, channel-last.
	Patch [PatchSide * PatchSide * Channels]byte

	// Black and White are the frame's photometric reference per channel, measured from cells whose values
	// the format fixes.
	//
	// Handed to the model rather than applied to the patch, deliberately. Applying it would bake in the
	// existing linear correction and limit the model to whatever that leaves behind; passing it lets the
	// model learn its own mapping, including the gamma the linear model provably cannot represent. It also
	// costs six numbers.
	Black, White [3]float32

	// Frame identifies which rendered frame this cell came from.
	//
	// Carried so a training split can be made *by frame* rather than by cell. Cells from one frame share
	// its exposure, its blur and its geometry, so splitting them across train and test leaks: the model is
	// scored partly on frames it has already seen, and the score comes out flattering. Four bytes to avoid
	// a whole class of self-deception.
	Frame uint32

	// Label is the symbol the sender rendered, 0..7 for colour8. Unused at inference time.
	Label uint8
}

// MarshalBinary writes the record in the fixed layout numpy reads.
func (r Record) MarshalBinary() []byte {
	out := make([]byte, 0, RecordBytes)
	out = append(out, r.Patch[:]...)
	for _, v := range [][3]float32{r.Black, r.White} {
		for _, f := range v {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(f))
			out = append(out, b[:]...)
		}
	}
	var fid [4]byte
	binary.LittleEndian.PutUint32(fid[:], r.Frame)
	out = append(out, fid[:]...)
	return append(out, r.Label)
}

// Export samples every payload cell of a located frame and writes one labelled record each.
//
// truth is the symbol sequence the sender rendered, in the same plan order the decoder reads. Supplying it
// rather than deriving it here is what keeps this honest: the caller must have encoded the frame, so the
// labels come from the encoder and not from a second opinion about what the image shows.
//
// Returns how many records were written.
func Export(w io.Writer, img image.Image, g *protocol.Geometry, truth []uint32, frame uint32) (int, error) {
	if g == nil {
		return 0, fmt.Errorf("dataset: export needs a resolved geometry")
	}

	s := protocol.NewSamplerAt(g, img)
	cells := g.Layout.PayloadCells()
	if len(truth) < len(cells) {
		return 0, fmt.Errorf("dataset: %d labels for %d payload cells", len(truth), len(cells))
	}

	black, white := reference(s, g.Layout)

	written := 0
	for i, cell := range cells {
		rec := Record{Black: black, White: white, Frame: frame, Label: uint8(truth[i])}
		samplePatch(&rec, img, g, cell)
		if _, err := w.Write(rec.MarshalBinary()); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// samplePatch fills a record's patch by sampling through the homography around a cell's centre.
func samplePatch(rec *Record, img image.Image, g *protocol.Geometry, cell protocol.Cell) {
	// Offsets run from -PatchSpan/2 to +PatchSpan/2 cell widths about the centre.
	step := PatchSpan / float64(PatchSide-1)
	idx := 0
	for row := 0; row < PatchSide; row++ {
		dy := -PatchSpan/2 + float64(row)*step
		for col := 0; col < PatchSide; col++ {
			dx := -PatchSpan/2 + float64(col)*step

			// A fractional cell coordinate, mapped to the image by the same homography and lens model
			// the decoder samples through, so the patch is in cell space and therefore scale-,
			// rotation- and perspective-invariant.
			r, gg, b := sampleAt(img, g, float64(cell.X)+0.5+dx, float64(cell.Y)+0.5+dy)
			rec.Patch[idx] = r
			rec.Patch[idx+1] = gg
			rec.Patch[idx+2] = b
			idx += 3
		}
	}
}

// sampleAt reads the image at a fractional cell coordinate, bilinearly.
//
// Bilinear rather than nearest, because the whole point of a patch is sub-cell structure and nearest
// sampling would quantise it back onto the pixel grid — at four pixels a cell that would throw away most
// of what distinguishes one patch from another.
//
// The two transforms are composed in the order the optical path applies them, matching the decoder's own
// sampler: the homography projects the grid coordinate to where a pinhole camera would see it, then the
// lens model displaces it to where the sensor actually recorded it.
func sampleAt(img image.Image, g *protocol.Geometry, cx, cy float64) (r, gg, b uint8) {
	p := g.Homography.Apply(protocol.Point{X: cx, Y: cy})
	bounds := img.Bounds()
	p = g.Distortion.Apply(p, bounds.Dx(), bounds.Dy())

	x0, y0 := math.Floor(p.X), math.Floor(p.Y)
	fx, fy := p.X-x0, p.Y-y0

	var acc [3]float64
	for _, c := range [4]struct {
		dx, dy int
		w      float64
	}{
		{0, 0, (1 - fx) * (1 - fy)},
		{1, 0, fx * (1 - fy)},
		{0, 1, (1 - fx) * fy},
		{1, 1, fx * fy},
	} {
		px, py := int(x0)+c.dx, int(y0)+c.dy
		if px < bounds.Min.X {
			px = bounds.Min.X
		}
		if px >= bounds.Max.X {
			px = bounds.Max.X - 1
		}
		if py < bounds.Min.Y {
			py = bounds.Min.Y
		}
		if py >= bounds.Max.Y {
			py = bounds.Max.Y - 1
		}
		cr, cg, cb, _ := img.At(px, py).RGBA()
		acc[0] += c.w * float64(cr>>8)
		acc[1] += c.w * float64(cg>>8)
		acc[2] += c.w * float64(cb>>8)
	}
	return u8(acc[0]), u8(acc[1]), u8(acc[2])
}

func u8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// reference measures the frame's black and white levels per channel from cells whose values the format
// fixes: the fiducial patterns and the timing columns.
//
// A single pair per frame rather than the decoder's fitted surface. The surface models lens falloff across
// the frame and is the better correction, but it is package-private to the encoder and reproducing it here
// would be a second implementation that could drift. One global pair is enough for what it is used for —
// telling the model roughly where this frame's black and white sit — and the model sees the patch itself,
// which carries the local variation the global pair misses.
func reference(s *protocol.Sampler, l protocol.Layout) (black, white [3]float32) {
	var onSum, offSum [3]float64
	var onN, offN float64

	bits := protocol.FinderBits()
	for _, o := range l.FinderOrigins() {
		for dy := 0; dy < protocol.FinderPattern; dy++ {
			for dx := 0; dx < protocol.FinderPattern; dx++ {
				c := s.Color(protocol.Cell{X: o.X + dx, Y: o.Y + dy})
				add(&onSum, &offSum, &onN, &offN, c, bits[dy][dx])
			}
		}
	}
	for _, t := range l.TimingCells() {
		add(&onSum, &offSum, &onN, &offN, s.Color(t.Cell), t.On)
	}

	for ch := 0; ch < 3; ch++ {
		if offN > 0 {
			black[ch] = float32(offSum[ch] / offN)
		}
		if onN > 0 {
			white[ch] = float32(onSum[ch] / onN)
		} else {
			white[ch] = 255
		}
	}
	return black, white
}

func add(onSum, offSum *[3]float64, onN, offN *float64, c color.RGBA, on bool) {
	v := [3]float64{float64(c.R), float64(c.G), float64(c.B)}
	if on {
		for ch := 0; ch < 3; ch++ {
			onSum[ch] += v[ch]
		}
		*onN++
		return
	}
	for ch := 0; ch < 3; ch++ {
		offSum[ch] += v[ch]
	}
	*offN++
}

// Sample builds one unlabelled record per payload cell, for inference.
//
// The same code path Export uses, minus the labels, so what the model sees in production is bit-identical
// to what it saw in training.
func Sample(img image.Image, g *protocol.Geometry) ([]Record, error) {
	if g == nil {
		return nil, fmt.Errorf("cellpatch: sampling needs a resolved geometry")
	}
	s := protocol.NewSamplerAt(g, img)
	black, white := reference(s, g.Layout)

	cells := g.Layout.PayloadCells()
	out := make([]Record, len(cells))
	for i, cell := range cells {
		out[i] = Record{Black: black, White: white}
		samplePatch(&out[i], img, g, cell)
	}
	return out, nil
}
