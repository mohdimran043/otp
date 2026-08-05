package protocol

import (
	"image"
	"image/color"
	"math"

	"github.com/opticaltransport/otp/shared/internal/bitio"
)

// Sampler reads grid cells out of a captured image.
//
// It composes the two transforms in the order a real optical path applies them:
// the homography projects a grid coordinate to where a pinhole camera would see
// it, then the lens model displaces that point to where the actual sensor
// recorded it. Sampling forward like this costs a few multiplications per cell.
// Undistorting the whole image instead would cost a full resample of every frame
// and gain nothing in accuracy.
type Sampler struct {
	Layout     Layout
	Homography Homography
	Distortion Distortion
	Img        image.Image
	Gray       *GrayMap

	// Radius is the half-width of the averaging window, in pixels. Zero means
	// point sampling.
	Radius int

	imgW, imgH int
	threshold  uint8
	contrast   float64
	calibrated bool
}

// NewSampler builds a sampler over a captured image.
func NewSampler(l Layout, h Homography, d Distortion, img image.Image) *Sampler {
	g := Grayscale(img)
	b := img.Bounds()
	return &Sampler{
		Layout:     l,
		Homography: h,
		Distortion: d,
		Img:        img,
		Gray:       g,
		Radius:     defaultSampleRadius(l),
		imgW:       b.Dx(),
		imgH:       b.Dy(),
		threshold:  128,
	}
}

// defaultSampleRadius picks an averaging window from the cell size: about a
// quarter of a cell, so the window stays clear of cell borders where a
// neighbouring value bleeds in.
func defaultSampleRadius(l Layout) int {
	r := l.CellPixels / 4
	if r < 1 {
		return 0
	}
	if r > 4 {
		return 4
	}
	return r
}

// CellPoint returns the pixel a cell's centre lands on.
func (s *Sampler) CellPoint(c Cell) Point {
	cx, cy := float64(c.X)+0.5, float64(c.Y)+0.5
	p := s.Homography.Apply(Point{cx, cy})
	return s.Distortion.Apply(p, s.imgW, s.imgH)
}

// Luma returns the mean luminance of a cell.
func (s *Sampler) Luma(c Cell) uint8 {
	return SampleLuma(s.Gray, s.CellPoint(c), s.Radius)
}

// Color returns the mean colour of a cell.
func (s *Sampler) Color(c Cell) color.RGBA {
	return SampleColor(s.Img, s.CellPoint(c), s.Radius)
}

// Bit reports whether a cell reads as set, against the calibrated threshold.
func (s *Sampler) Bit(c Cell) bool {
	return s.Luma(c) > s.Threshold()
}

// Threshold is the luminance boundary between clear and set cells.
func (s *Sampler) Threshold() uint8 {
	if !s.calibrated {
		s.Calibrate()
	}
	return s.threshold
}

// Contrast is the luminance gap between set and clear fiducial cells, in 0..255.
// The receiver surfaces it as decode quality: a falling contrast is the first
// sign the camera is drifting out of focus or the room lights have come up.
func (s *Sampler) Contrast() float64 {
	if !s.calibrated {
		s.Calibrate()
	}
	return s.contrast
}

// Calibrate derives the binary threshold from the fiducials themselves.
//
// The four finder patterns are the one part of a frame whose every cell value is
// known in advance, which makes them a built-in reference: average the cells that
// must be bright, average the cells that must be dark, and put the threshold
// midway. That adapts automatically to exposure, panel brightness, and ambient
// light without any of them having to be configured.
func (s *Sampler) Calibrate() {
	bits := FinderBits()
	origins := s.Layout.FinderOrigins()

	var onSum, offSum, onN, offN float64
	for _, o := range origins {
		for dy := 0; dy < FinderPattern; dy++ {
			for dx := 0; dx < FinderPattern; dx++ {
				v := float64(SampleLuma(s.Gray, s.CellPoint(Cell{o.X + dx, o.Y + dy}), s.Radius))
				if bits[dy][dx] {
					onSum += v
					onN++
				} else {
					offSum += v
					offN++
				}
			}
		}
	}
	s.calibrated = true
	if onN == 0 || offN == 0 {
		s.threshold, s.contrast = 128, 0
		return
	}
	onMean, offMean := onSum/onN, offSum/offN
	s.threshold = uint8(clampF((onMean+offMean)/2, 1, 254))
	s.contrast = onMean - offMean
}

func clampF(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// FinderScore is the fraction of fiducial cells that read as expected, in 0..1.
//
// It is the decoder's cheap test for "is this geometry right at all". Sampling
// 196 known cells is a fraction of the cost of reading a header band, so the
// decoder scores every candidate geometry this way and only spends a full header
// read on the best ones.
func (s *Sampler) FinderScore() float64 {
	bits := FinderBits()
	thr := s.Threshold()
	var hits, total float64
	for _, o := range s.Layout.FinderOrigins() {
		for dy := 0; dy < FinderPattern; dy++ {
			for dx := 0; dx < FinderPattern; dx++ {
				got := SampleLuma(s.Gray, s.CellPoint(Cell{o.X + dx, o.Y + dy}), s.Radius) > thr
				if got == bits[dy][dx] {
					hits++
				}
				total++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return hits / total
}

// TimingScore is the fraction of timing cells that read as expected, in 0..1.
//
// The timing columns run in opposite phase, so a geometry off by a single row
// scores near a half rather than passing by luck on one column. The decoder uses
// it to confirm a geometry that already matched the fiducials.
func (s *Sampler) TimingScore() float64 {
	thr := s.Threshold()
	var hits, total float64
	for _, t := range s.Layout.TimingCells() {
		if (s.Luma(t.Cell) > thr) == t.On {
			hits++
		}
		total++
	}
	if total == 0 {
		return 0
	}
	return hits / total
}

// ReadHeader reads and majority-votes the header band.
func (s *Sampler) ReadHeader() (Header, error) {
	rec, err := s.readBand(s.Layout.HeaderCells(), HeaderSize, s.Layout.HeaderCopies())
	if err != nil {
		return Header{}, err
	}
	var h Header
	if err := h.UnmarshalBinary(rec); err != nil {
		return Header{}, err
	}
	return h, nil
}

// ReadFooter reads and majority-votes the footer band.
func (s *Sampler) ReadFooter() (Footer, error) {
	rec, err := s.readBand(s.Layout.FooterCells(), FooterSize, s.Layout.FooterCopies())
	if err != nil {
		return Footer{}, err
	}
	var f Footer
	if err := f.UnmarshalBinary(rec); err != nil {
		return Footer{}, err
	}
	return f, nil
}

// readBand samples every copy of a repeated record and votes bit by bit.
func (s *Sampler) readBand(cells []Cell, recordBytes, copies int) ([]byte, error) {
	bits := recordBytes * 8
	if copies*bits > len(cells) {
		return nil, ErrShortBuffer
	}
	thr := s.Threshold()
	raw := make([]byte, recordBytes*copies)
	w := bitio.NewWriterBytes(raw)
	for i := 0; i < copies*bits; i++ {
		if err := w.WriteBit(s.Luma(cells[i]) > thr); err != nil {
			return nil, err
		}
	}
	return bitio.MajorityVote(raw, recordBytes, copies)
}

// BandErrorRate reports the fraction of band bits that disagreed with the voted
// result. It is a direct measurement of the channel's bit error rate on cells
// whose correct value is known after the fact, which is what the receiver charts.
func (s *Sampler) BandErrorRate() float64 {
	cells := s.Layout.HeaderCells()
	copies := s.Layout.HeaderCopies()
	bits := HeaderSize * 8
	if copies*bits > len(cells) {
		return 0
	}
	thr := s.Threshold()
	raw := make([]byte, HeaderSize*copies)
	w := bitio.NewWriterBytes(raw)
	for i := 0; i < copies*bits; i++ {
		_ = w.WriteBit(s.Luma(cells[i]) > thr)
	}
	voted, err := bitio.MajorityVote(raw, HeaderSize, copies)
	if err != nil {
		return 0
	}

	var wrong int
	for k := 0; k < copies; k++ {
		copyBits := bitio.NewReader(raw[k*HeaderSize : (k+1)*HeaderSize])
		truth := bitio.NewReader(voted)
		for i := 0; i < bits; i++ {
			a, err1 := copyBits.ReadBit()
			b, err2 := truth.ReadBit()
			if err1 != nil || err2 != nil {
				break
			}
			if a != b {
				wrong++
			}
		}
	}
	return float64(wrong) / float64(copies*bits)
}
