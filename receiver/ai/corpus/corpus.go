// Package corpus runs stored camera captures back through the full decode-and-recover path.
//
// It exists because the synthetic profiles cannot answer the question that matters. Measured across
// every profile in shared/simulate, at every geometry that still locates its fiducials a colour8 frame
// either decodes cleanly or fails by hundreds of cells — so the regime recovery is built for, a handful
// of ambiguous cells in an otherwise good frame, never occurs there. Whether it occurs in reality is a
// property of real cameras, real screens and real hands, and the only way to find out is to run real
// captures.
//
// The receiver already stores every frame it photographs, before deciding whether it could read it, so
// that corpus exists without any new capture work. This package replays it.
//
// What it deliberately does not do is re-implement any of the pipeline. It calls the same
// encoding.Decode the receiver calls and the same engine the receiver runs, so a number produced here
// is a claim about the deployed system rather than about a reconstruction of it.
package corpus

import (
	"context"
	"fmt"
	"image"
	_ "image/jpeg" // stored captures are whatever the camera posted, and a browser posts JPEG
	_ "image/png"  // the file source writes PNG
	"os"
	"sort"
	"time"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/receiver/ai/engine"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Outcome is what happened to one capture.
type Outcome struct {
	// Path is the file replayed.
	Path string

	// Bucket is the stage the first decode failed at, or "decoded".
	Bucket classify.Bucket

	// Recovered reports whether the engine read a frame the decoder rejected.
	Recovered bool

	// Report is the engine's account, when it recovered something.
	Report engine.Report

	// Clipped is the fraction of pixels saturated in at least one channel.
	Clipped float64

	// FinderScore and Contrast come from the geometry, when one resolved.
	FinderScore float64
	Contrast    float64

	// Layout is the geometry the frame turned out to have. Reported because a corpus arrives without a
	// note saying what the sender was configured for, and the frames themselves are the only reliable
	// record — the grid a settings page claims is not evidence of what was actually transmitted.
	Layout string

	// MarginalCells is how many payload cells were read with a margin under a quarter of the
	// palette's separation, and TotalCells how many there were.
	//
	// This is the number that says whether recovery is the right tool for a given corpus. A frame
	// with a handful of marginal cells is recoverable; one with hundreds is telling you the cells are
	// not ambiguous, they are wrong, and a bounded search will not reach them however long it runs.
	MarginalCells int
	TotalCells    int

	// DecodeElapsed and RecoverElapsed separate the two costs, since only the second is this layer's.
	DecodeElapsed  time.Duration
	RecoverElapsed time.Duration
}

// Summary aggregates outcomes.
type Summary struct {
	Frames int

	// DecodedFirstPass is how many read without any help, and RecoveredByEngine how many the engine
	// then rescued. Their sum over Frames is the decode rate a deployment would see.
	DecodedFirstPass  int
	RecoveredByEngine int

	// Buckets counts the first-pass outcome by stage.
	Buckets map[classify.Bucket]int

	// RecoveredFrom counts recoveries by the bucket they were rescued from, which is what says where
	// the layer is actually earning its place.
	RecoveredFrom map[classify.Bucket]int

	// Engine and Version name what ran, so a figure is attributable.
	Engine, Version string

	// Layouts counts the geometries seen, so a corpus that spans more than one sender configuration
	// says so instead of averaging over them silently.
	Layouts map[string]int

	// ByLayout breaks the outcome down per geometry, which is the cross-tabulation that turns a corpus
	// into a decision. A corpus mixing two sender configurations has one decode rate and two causes,
	// and averaging them hides the only thing worth knowing: which geometry works.
	ByLayout map[string]*LayoutStats

	// MeanMarginalCells and MeanTotalCells are averaged over frames that located, and MeanClipped over
	// all of them.
	MeanMarginalCells float64
	MeanTotalCells    float64
	MeanClipped       float64

	// MedianFlips and MedianCandidates describe the recoveries that succeeded.
	MedianFlips      int
	MedianCandidates int

	// TotalDecode and TotalRecover are the wall-clock cost of each half.
	TotalDecode  time.Duration
	TotalRecover time.Duration
}

// LayoutStats is one geometry's outcome.
type LayoutStats struct {
	Frames    int
	Decoded   int
	Recovered int

	// MeanMarginal is the average number of ambiguous cells per frame at this geometry, and MeanTotal
	// the payload size. Their ratio is the figure that decides whether a bounded search can ever help:
	// a search correcting k cells is irrelevant at a geometry averaging many times k.
	MeanMarginal float64
	MeanTotal    float64
	MeanFinder   float64
}

// Options configures a replay.
type Options struct {
	// Layout, when set, is handed to the decoder as the expected geometry — the same hint the receiver
	// learns from its first readable frame. Without it every frame pays a descriptor search, which on
	// a marginal capture is the difference between locating and not.
	Layout *protocol.Layout

	// Engine reads frames the decoder rejects. Required.
	Engine engine.Engine

	// MarginalThreshold is the fraction of the palette's separation under which a cell counts as
	// marginal. A quarter is the figure used elsewhere in this tree.
	MarginalThreshold float64
}

// Replay runs one stored capture through decode and, on failure, through the engine.
func Replay(ctx context.Context, path string, opts Options) (Outcome, error) {
	out := Outcome{Path: path}

	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return out, fmt.Errorf("corpus: %s: %w", path, err)
	}
	out.Clipped = classify.Clipped(img)

	locateOpts := protocol.LocateOptions{}
	if opts.Layout != nil {
		locateOpts.ExpectedLayout = opts.Layout
	}

	// Locate separately from Decode so the geometry is available for the engine and for the margin
	// measurement, exactly as prepare does it in the receiver.
	started := time.Now()
	geometry, locateErr := protocol.Locate(img, locateOpts)
	var decodeErr error
	if locateErr != nil {
		decodeErr = locateErr
	} else {
		_, decodeErr = encoding.Decode(img, locateOpts)
	}
	out.DecodeElapsed = time.Since(started)

	out.Bucket = classify.Of(decodeErr)
	if geometry != nil {
		out.FinderScore = geometry.FinderScore
		out.Contrast = geometry.Contrast
		out.Layout = fmt.Sprintf("%dx%d@%dpx", geometry.Layout.GridWidth, geometry.Layout.GridHeight, geometry.Layout.CellPixels)
		out.MarginalCells, out.TotalCells = margins(geometry, img, opts.MarginalThreshold)
	}
	if decodeErr == nil {
		return out, nil
	}

	started = time.Now()
	res, rerr := opts.Engine.Recover(ctx, engine.Request{
		Image:    img,
		Geometry: geometry,
		Bucket:   out.Bucket,
		Clipped:  out.Clipped,
	})
	out.RecoverElapsed = time.Since(started)
	if rerr == nil {
		out.Recovered = true
		out.Report = res.Report
	}
	return out, nil
}

// margins counts how many payload cells were read with little confidence.
//
// Returns zeroes when the payload region cannot be soft-read at all, which happens when the footer is
// unreadable — the same condition that makes the frame unrecoverable, so there is nothing to report.
func margins(g *protocol.Geometry, img image.Image, threshold float64) (marginal, total int) {
	if threshold <= 0 {
		threshold = 0.25
	}
	r, err := encoding.SoftRead(g, img)
	if err != nil {
		return 0, 0
	}
	limit := r.Palette.MinSeparation() * threshold
	for _, c := range r.Cells {
		if c.Margin < limit {
			marginal++
		}
	}
	return marginal, len(r.Cells)
}

// Summarise aggregates outcomes into the figures worth reporting.
func Summarise(outcomes []Outcome, engineName, version string) Summary {
	s := Summary{
		Frames:        len(outcomes),
		Buckets:       map[classify.Bucket]int{},
		RecoveredFrom: map[classify.Bucket]int{},
		Layouts:       map[string]int{},
		ByLayout:      map[string]*LayoutStats{},
		Engine:        engineName,
		Version:       version,
	}

	var flips, candidates []int
	var marginalSum, totalSum, clippedSum float64
	var located int

	for _, o := range outcomes {
		s.Buckets[o.Bucket]++
		s.TotalDecode += o.DecodeElapsed
		s.TotalRecover += o.RecoverElapsed
		clippedSum += o.Clipped

		if o.Layout != "" {
			s.Layouts[o.Layout]++
			ls := s.ByLayout[o.Layout]
			if ls == nil {
				ls = &LayoutStats{}
				s.ByLayout[o.Layout] = ls
			}
			ls.Frames++
			ls.MeanMarginal += float64(o.MarginalCells)
			ls.MeanTotal += float64(o.TotalCells)
			ls.MeanFinder += o.FinderScore
			if o.Bucket == classify.BucketDecoded {
				ls.Decoded++
			}
			if o.Recovered {
				ls.Recovered++
			}
		}
		if o.TotalCells > 0 {
			located++
			marginalSum += float64(o.MarginalCells)
			totalSum += float64(o.TotalCells)
		}
		switch {
		case o.Bucket == classify.BucketDecoded:
			s.DecodedFirstPass++
		case o.Recovered:
			s.RecoveredByEngine++
			s.RecoveredFrom[o.Bucket]++
			flips = append(flips, o.Report.Flips)
			candidates = append(candidates, o.Report.Candidates)
		}
	}

	if located > 0 {
		s.MeanMarginalCells = marginalSum / float64(located)
		s.MeanTotalCells = totalSum / float64(located)
	}
	if len(outcomes) > 0 {
		s.MeanClipped = clippedSum / float64(len(outcomes))
	}
	// Turn the per-layout sums into means now that every frame has been seen.
	for _, ls := range s.ByLayout {
		if ls.Frames > 0 {
			ls.MeanMarginal /= float64(ls.Frames)
			ls.MeanTotal /= float64(ls.Frames)
			ls.MeanFinder /= float64(ls.Frames)
		}
	}

	s.MedianFlips = median(flips)
	s.MedianCandidates = median(candidates)
	return s
}

func median(v []int) int {
	if len(v) == 0 {
		return 0
	}
	sort.Ints(v)
	return v[len(v)/2]
}

// String renders a summary as the report a person reads.
func (s Summary) String() string {
	out := fmt.Sprintf("frames %d   engine %s (%s)\n", s.Frames, s.Engine, s.Version)
	if s.Frames == 0 {
		return out
	}
	first := 100 * float64(s.DecodedFirstPass) / float64(s.Frames)
	withAI := 100 * float64(s.DecodedFirstPass+s.RecoveredByEngine) / float64(s.Frames)
	out += fmt.Sprintf("decoded first pass  %d (%.1f%%)\n", s.DecodedFirstPass, first)
	out += fmt.Sprintf("recovered by engine %d (+%.1f points -> %.1f%%)\n",
		s.RecoveredByEngine, withAI-first, withAI)

	out += "\nfirst-pass buckets:\n"
	keys := make([]string, 0, len(s.Buckets))
	for b := range s.Buckets {
		keys = append(keys, string(b))
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := classify.Bucket(k)
		line := fmt.Sprintf("  %-20s %5d", k, s.Buckets[b])
		if got := s.RecoveredFrom[b]; got > 0 {
			line += fmt.Sprintf("   recovered %d", got)
		}
		out += line + "\n"
	}

	if len(s.ByLayout) > 0 {
		out += "\nby geometry:\n"
		out += fmt.Sprintf("  %-16s %6s %8s %10s %9s %14s\n",
			"layout", "frames", "decoded", "recovered", "finder", "marginal/frame")
		names := make([]string, 0, len(s.ByLayout))
		for k := range s.ByLayout {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			ls := s.ByLayout[k]
			pct := 100 * float64(ls.Decoded) / float64(ls.Frames)
			out += fmt.Sprintf("  %-16s %6d %5d(%3.0f%%) %10d %9.3f %8.0f of %.0f (%.1f%%)\n",
				k, ls.Frames, ls.Decoded, pct, ls.Recovered, ls.MeanFinder,
				ls.MeanMarginal, ls.MeanTotal, 100*ls.MeanMarginal/max64(ls.MeanTotal, 1))
		}
	}

	out += fmt.Sprintf("\nmean marginal cells   %.1f of %.0f (%.2f%%)\n",
		s.MeanMarginalCells, s.MeanTotalCells,
		100*s.MeanMarginalCells/max64(s.MeanTotalCells, 1))
	out += fmt.Sprintf("mean clipped fraction %.3f\n", s.MeanClipped)
	if s.RecoveredByEngine > 0 {
		out += fmt.Sprintf("median flips %d, median candidates %d\n", s.MedianFlips, s.MedianCandidates)
	}
	out += fmt.Sprintf("decode %s total, recovery %s total\n", s.TotalDecode, s.TotalRecover)
	return out
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
