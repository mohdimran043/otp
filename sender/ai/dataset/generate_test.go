package dataset_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/ai/dataset"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// Generating the training set.
//
//	OTP_DATASET_OUT=/path/to/dir go test ./ai/dataset/ -run TestGenerate -v -timeout 3600s
//
// Written as a test rather than a main package because it needs the encoder, the geometry and the
// degradation model, all of which already have test-grade helpers here, and because a generator nobody
// runs in CI rots. The environment variable keeps it inert on an ordinary `go test ./...`.

// geometries span the range that matters for a camera channel.
//
// Both the configurations that work and the ones that do not, deliberately. A classifier trained only on
// legible frames learns nothing about the case it is needed for; one trained only on hopeless frames learns
// noise. Cell sizes 8, 4 and 2 bracket the measured cliff: 8 px decodes everything, 4 px is where cells
// turn ambiguous, 2 px is where the fiducials themselves start going.
var geometries = []struct{ grid, cell int }{
	{80, 8}, {96, 8}, {128, 8},
	{80, 4}, {96, 4}, {128, 4},
	{96, 2}, {128, 2},
}

// profiles are the optical paths to train against.
//
// Pristine is included on purpose even though it is trivially decodable: it anchors the model on what a
// clean cell looks like, and without it a classifier trained only on damage will happily reinterpret a
// perfect frame.
func profiles() []struct {
	name string
	p    simulate.Profile
} {
	return append([]struct {
		name string
		p    simulate.Profile
	}{
		{"pristine", simulate.Pristine},
		{"clean", simulate.Clean},
		{"typical", simulate.Typical},
		{"harsh", simulate.Harsh},
	}, hardProfiles()...)
}

// hardProfiles fill the gap the named profiles leave, and without them this dataset proves nothing.
//
// Measured: on the four named profiles the decoder's own rule reads held-out cells with 99.993%
// accuracy — ten errors in 136,584. A training set where the baseline is already almost perfect cannot
// demonstrate that a model beats the baseline, however good the model is; there is nothing left to fix.
// That is the same bimodality the decode sweep found, seen from the other side: these profiles either
// leave cells clean or destroy the fiducials, and skip the regime in between.
//
// The regime in between is *blur relative to cell size*, because that is what makes neighbours bleed
// into a cell's centre — the one error a patch-based model can undo and a nearest-neighbour rule cannot.
// So these sweep blur upward in small steps while keeping geometry mild enough that the frame still
// locates, and add sensor noise so the model cannot simply learn a deterministic mixing kernel.
func hardProfiles() []struct {
	name string
	p    simulate.Profile
} {
	out := make([]struct {
		name string
		p    simulate.Profile
	}, 0, 8)
	for i, sigma := range []float64{1.3, 1.8, 2.3, 2.8} {
		for j, noise := range []float64{8, 16} {
			out = append(out, struct {
				name string
				p    simulate.Profile
			}{
				name: fmt.Sprintf("bleed-b%.1f-n%.0f", sigma, noise),
				p: simulate.Profile{
					BlurSigma:  sigma,
					NoiseSigma: noise,
					Tilt:       0.04,
					Rotation:   1.0,
					Pad:        0.08,
					Brightness: -4,
					Gamma:      1.08,
					Vignette:   0.12,
					// Compression is part of the real path — the browser posts JPEG — and it damages
					// chroma specifically, which is what colour8 reads.
					JPEGQuality: 88,
					Seed:        int64(100 + i*10 + j),
				},
			})
		}
	}
	return out
}

func TestGenerateTrainingSet(t *testing.T) {
	outDir := os.Getenv("OTP_DATASET_OUT")
	if outDir == "" {
		t.Skip("set OTP_DATASET_OUT to generate the classifier training set")
	}
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	framesPerPoint := 3
	if v := os.Getenv("OTP_DATASET_FRAMES"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &framesPerPoint)
	}

	binPath := filepath.Join(outDir, "cells.bin")
	f, err := os.Create(binPath)
	require.NoError(t, err)
	w := bufio.NewWriterSize(f, 1<<22)

	rng := rand.New(rand.NewSource(7))
	perLabel := map[uint8]int{}
	perGeometry := map[string]int{}
	total := 0
	skipped := 0
	// Numbered across the whole run, so a training split by frame separates whole frames regardless of
	// which geometry or profile produced them.
	frameID := uint32(0)

	for _, geom := range geometries {
		l, err := protocol.NewLayout(geom.grid, geom.grid, geom.cell)
		if err != nil {
			t.Logf("grid %d @%dpx: %v", geom.grid, geom.cell, err)
			continue
		}
		capacity, err := encoding.Color8.EstimateCapacity(l, 3)
		require.NoError(t, err)

		for _, pr := range profiles() {
			for i := 0; i < framesPerPoint; i++ {
				// Random payload, so the label distribution is uniform over the eight symbols and the
				// model cannot learn a positional prior. A repeating pattern would give it one.
				payload := make([]byte, capacity.PayloadBytes)
				rng.Read(payload)

				frame := protocol.NewFrame(protocol.Header{TransmissionID: uuid.New()}, payload)
				pristine, err := encoding.Color8.Encode(frame, l, 3)
				require.NoError(t, err)

				// Labels come from the pristine render and are proved against its footer.
				pg, err := protocol.Locate(pristine, protocol.LocateOptions{ExpectedLayout: &l})
				require.NoError(t, err, "a pristine render must always locate")
				truth, err := dataset.TruthFor(pristine, pg)
				require.NoError(t, err)

				// Patches come from the degraded render, at its own geometry. Locating the degraded frame
				// separately matters: its homography is where the cells actually are in that image, and
				// using the pristine geometry would sample the wrong pixels on anything warped.
				degraded := pr.p.Apply(pristine)
				dg, err := protocol.Locate(degraded, protocol.LocateOptions{ExpectedLayout: &l})
				if err != nil {
					// A profile that destroyed the fiducials yields no training data, and that is the
					// honest outcome: there is no cell to label if there is no geometry to find it at.
					skipped++
					continue
				}

				n, err := dataset.Export(w, degraded, dg, truth, frameID)
				frameID++
				require.NoError(t, err)

				total += n
				key := fmt.Sprintf("%dx%d@%dpx/%s", geom.grid, geom.grid, geom.cell, pr.name)
				perGeometry[key] += n
				for _, sym := range truth[:min(n, len(truth))] {
					perLabel[uint8(sym)]++
				}
			}
		}
	}

	require.NoError(t, w.Flush())
	require.NoError(t, f.Close())
	require.Positive(t, total, "no cells exported")

	meta := map[string]any{
		"record_bytes": dataset.RecordBytes,
		"patch_side":   dataset.PatchSide,
		"patch_span":   dataset.PatchSpan,
		"channels":     dataset.Channels,
		"classes":      8,
		"records":      total,
		"per_label":    perLabel,
		"per_geometry": perGeometry,
		"skipped":      skipped,
		"frames":       frameID,
		"layout":       "patch[side*side*channels] uint8, black[3] float32, white[3] float32, frame uint32, label uint8",
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "cells.json"), metaJSON, 0o644))

	info, err := os.Stat(binPath)
	require.NoError(t, err)
	require.Equal(t, int64(total)*int64(dataset.RecordBytes), info.Size(),
		"the file must be exactly the records claimed, or numpy will reshape garbage")

	t.Logf("%d cells, %.1f MiB, %d frames skipped for lost geometry",
		total, float64(info.Size())/(1<<20), skipped)
	for k, v := range perGeometry {
		t.Logf("  %-22s %8d", k, v)
	}
	t.Logf("label balance: %v", perLabel)
}
