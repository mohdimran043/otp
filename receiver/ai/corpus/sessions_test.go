package corpus_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/ai/corpus"
	"github.com/opticaltransport/otp/receiver/ai/engine"
)

// Replaying several capture sessions at once, one per subdirectory.
//
//	OTP_CORPUS_SESSIONS=/path/to/dir go test ./ai/corpus/ -run TestCorpusSessions -v -timeout 3600s
//
// Where /path/to/dir holds one subdirectory per session. Sessions rather than a single pile because the
// question a corpus answers is comparative: the same camera in the same afternoon produces wildly
// different decode rates, and the interesting part is which variable moved. Averaging across sessions
// destroys exactly that.
//
// Sample uniformly when building these directories — every Nth frame — not by picking interesting ones.
// A stratified sample gives a decode rate that is an artefact of the stratification, which is a mistake
// worth making only once.
func TestCorpusSessions(t *testing.T) {
	root := os.Getenv("OTP_CORPUS_SESSIONS")
	if root == "" {
		t.Skip("set OTP_CORPUS_SESSIONS to a directory of per-session capture directories")
	}

	entries, err := os.ReadDir(root)
	require.NoError(t, err)

	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	require.NotEmpty(t, dirs, "no session directories in %s", root)
	sort.Strings(dirs)

	eng := engine.NewGo(searchOptions())
	opts := corpus.Options{Engine: eng, MarginalThreshold: 0.25}

	type row struct {
		session string
		summary corpus.Summary
	}
	rows := make([]row, 0, len(dirs))

	for _, name := range dirs {
		paths, err := filepath.Glob(filepath.Join(root, name, "*"))
		require.NoError(t, err)

		outcomes := make([]corpus.Outcome, 0, len(paths))
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil || info.IsDir() || info.Size() < 1024 {
				continue
			}
			out, err := corpus.Replay(context.Background(), p, opts)
			if err != nil {
				t.Logf("%s/%s: %v", name, filepath.Base(p), err)
				continue
			}
			outcomes = append(outcomes, out)
		}
		if len(outcomes) == 0 {
			t.Logf("%s: nothing replayable", name)
			continue
		}
		rows = append(rows, row{session: name, summary: corpus.Summarise(outcomes, eng.Name(), eng.Version())})
	}
	require.NotEmpty(t, rows)

	fmt.Printf("\n=== %d camera sessions, engine %s (%s) ===\n\n", len(rows), eng.Name(), eng.Version())
	fmt.Printf("%-10s %6s %9s %10s %8s %9s %-16s\n",
		"session", "frames", "decoded", "recovered", "clipped", "marginal", "geometry")
	for _, r := range rows {
		s := r.summary
		decodedPct := 100 * float64(s.DecodedFirstPass) / float64(s.Frames)
		marginalPct := 100 * s.MeanMarginalCells / maxf(s.MeanTotalCells, 1)
		fmt.Printf("%-10s %6d %4d(%3.0f%%) %10d %8.3f %8.1f%% %-16s\n",
			r.session, s.Frames, s.DecodedFirstPass, decodedPct, s.RecoveredByEngine,
			s.MeanClipped, marginalPct, dominantLayout(s))
	}

	fmt.Println("\nper session, by geometry:")
	for _, r := range rows {
		names := make([]string, 0, len(r.summary.ByLayout))
		for k := range r.summary.ByLayout {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			ls := r.summary.ByLayout[k]
			fmt.Printf("  %-10s %-16s %4d frames  %4d decoded (%3.0f%%)  %4d recovered  finder %.3f  frame %4.1f%% of photo  marginal %.0f of %.0f\n",
				r.session, k, ls.Frames, ls.Decoded,
				100*float64(ls.Decoded)/float64(ls.Frames), ls.Recovered,
				ls.MeanFinder, 100*ls.MeanShare, ls.MeanMarginal, ls.MeanTotal)
		}
		if len(names) == 0 {
			fmt.Printf("  %-10s %-16s %4d frames  no geometry resolved on any frame\n",
				r.session, "-", r.summary.Frames)
		}
	}

	fmt.Println("\nfailure buckets:")
	for _, r := range rows {
		keys := make([]string, 0, len(r.summary.Buckets))
		for b := range r.summary.Buckets {
			keys = append(keys, string(b))
		}
		sort.Strings(keys)
		line := fmt.Sprintf("  %-10s", r.session)
		for _, k := range keys {
			line += fmt.Sprintf(" %s=%d", k, r.summary.Buckets[corpus.BucketKey(k)])
		}
		fmt.Println(line)
	}
	fmt.Println()

	for _, r := range rows {
		require.GreaterOrEqual(t, r.summary.RecoveredByEngine, 0)
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// dominantLayout names the geometry most frames in a session had, for the one-line summary.
func dominantLayout(s corpus.Summary) string {
	best, count := "-", 0
	for k, ls := range s.ByLayout {
		if ls.Frames > count {
			best, count = k, ls.Frames
		}
	}
	if count > 0 && count < s.Frames {
		return fmt.Sprintf("%s (%d/%d)", best, count, s.Frames)
	}
	return best
}
