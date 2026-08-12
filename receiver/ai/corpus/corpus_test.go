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
	"github.com/opticaltransport/otp/receiver/ai/soft"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Replaying real captures needs real captures, so this is driven by an environment variable rather than
// by fixtures checked into the repository. The corpus is gigabytes of photographs of a screen; it belongs
// in the object store the receiver already writes it to, not in git.
//
//	OTP_CORPUS_DIR=/path/to/captures go test ./ai/corpus/ -run TestCorpus -v
//
// Optionally OTP_CORPUS_GRID / OTP_CORPUS_CELL name the sender's geometry, which is the same hint the
// receiver learns from its first readable frame. Without it every frame pays a descriptor search, and on
// a marginal capture that is the difference between locating and not — so a run without it understates
// the decode rate rather than measuring it.

func TestCorpusReplay(t *testing.T) {
	dir := os.Getenv("OTP_CORPUS_DIR")
	if dir == "" {
		t.Skip("set OTP_CORPUS_DIR to a directory of stored captures to replay a real corpus")
	}

	paths, err := filepath.Glob(filepath.Join(dir, "*"))
	require.NoError(t, err)
	sort.Strings(paths)

	files := make([]string, 0, len(paths))
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Size() > 1024 {
			files = append(files, p)
		}
	}
	require.NotEmpty(t, files, "no captures found in %s", dir)

	opts := corpus.Options{
		Engine:            engine.NewGo(searchOptions()),
		MarginalThreshold: 0.25,
		Layout:            layoutFromEnv(t),
	}

	outcomes := make([]corpus.Outcome, 0, len(files))
	for _, p := range files {
		out, err := corpus.Replay(context.Background(), p, opts)
		if err != nil {
			t.Logf("skipping %s: %v", filepath.Base(p), err)
			continue
		}
		outcomes = append(outcomes, out)
	}
	require.NotEmpty(t, outcomes, "every capture failed to load")

	summary := corpus.Summarise(outcomes, opts.Engine.Name(), opts.Engine.Version())
	fmt.Printf("\n=== real corpus: %s ===\n%s\n", dir, summary)

	// Per-frame detail for the frames that located, so the distribution behind the means is visible.
	// Bounded, because a corpus can be thousands of frames and a wall of them is not a report.
	fmt.Printf("%-14s %-14s %8s %9s %8s %7s %s\n",
		"frame", "bucket", "finder", "marginal", "clipped", "flips", "recovered")
	shown := 0
	for _, o := range outcomes {
		if o.TotalCells == 0 || shown >= 25 {
			continue
		}
		shown++
		fmt.Printf("%-14s %-14s %8.3f %5d/%-4d %8.3f %7d %v\n",
			filepath.Base(o.Path), o.Bucket, o.FinderScore,
			o.MarginalCells, o.TotalCells, o.Clipped, o.Report.Flips, o.Recovered)
	}
	fmt.Println()

	// The only assertion: recovery cannot lower the decode count, because it runs after a failure and
	// returns a frame only when the footer confirms it. Everything else here is a measurement, and a
	// threshold on a real corpus would be a threshold on how steady someone's hands were that day.
	require.GreaterOrEqual(t, summary.DecodedFirstPass+summary.RecoveredByEngine, summary.DecodedFirstPass)
}

func searchOptions() soft.Options {
	o := soft.DefaultOptions()
	// No time bound when replaying: the question is what the search can find, not whether it fits in a
	// frame interval. The live path keeps its budget.
	o.Budget = 0
	return o
}

func layoutFromEnv(t *testing.T) *protocol.Layout {
	t.Helper()
	grid := intFromEnv("OTP_CORPUS_GRID")
	cell := intFromEnv("OTP_CORPUS_CELL")
	if grid == 0 || cell == 0 {
		t.Log("no OTP_CORPUS_GRID/OTP_CORPUS_CELL: every frame will pay a descriptor search")
		return nil
	}
	l, err := protocol.NewLayout(grid, grid, cell)
	require.NoError(t, err)
	t.Logf("using layout hint %s", l)
	return &l
}

func intFromEnv(key string) int {
	var v int
	if _, err := fmt.Sscanf(os.Getenv(key), "%d", &v); err != nil {
		return 0
	}
	return v
}
