package corpus_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/ai/corpus"
	"github.com/opticaltransport/otp/receiver/ai/engine"
)

// Comparing the deterministic engine against the learned one on the same real captures.
//
//	OTP_CORPUS_SESSIONS=/dir OTP_CLASSIFIER_URL=http://localhost:9800 \
//	  go test ./ai/corpus/ -run TestCorpusEngines -v -timeout 3600s
//
// Both engines see identical frames in the same run, because that is the only way the comparison means
// anything: decode rates on this channel vary more between sessions than between engines, so two runs on
// two samples could show any result at all.
func TestCorpusEngines(t *testing.T) {
	root := os.Getenv("OTP_CORPUS_SESSIONS")
	url := os.Getenv("OTP_CLASSIFIER_URL")
	if root == "" || url == "" {
		t.Skip("set OTP_CORPUS_SESSIONS and OTP_CLASSIFIER_URL to compare engines on real captures")
	}

	ctx := context.Background()
	deterministic := engine.NewGo(searchOptions())
	learned, err := engine.NewClassifier(ctx, engine.ClassifierOptions{
		URL:           url,
		Timeout:       60 * time.Second,
		MaxCells:      12,
		MaxCandidates: 4096,
	})
	require.NoError(t, err, "the classifier service must be reachable")
	t.Logf("classifier version: %s", learned.Version())

	engines := []struct {
		name string
		e    engine.Engine
	}{
		{"go", deterministic},
		{"classifier", learned},
		// The composition a deployment actually runs: cheap search first, model only on what it missed.
		{"go+classifier", engine.NewChain(deterministic, learned)},
	}

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		paths, _ := filepath.Glob(filepath.Join(root, e.Name(), "*"))
		for _, p := range paths {
			if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Size() > 1024 {
				files = append(files, p)
			}
		}
	}
	sort.Strings(files)
	require.NotEmpty(t, files)

	fmt.Printf("\n=== %d real captures, engines compared on identical frames ===\n\n", len(files))
	fmt.Printf("%-16s %8s %10s %12s %10s %12s\n",
		"engine", "decoded", "recovered", "total", "rate", "recover ms")

	for _, en := range engines {
		outcomes := make([]corpus.Outcome, 0, len(files))
		for _, p := range files {
			out, err := corpus.Replay(ctx, p, corpus.Options{Engine: en.e, MarginalThreshold: 0.25})
			if err != nil {
				continue
			}
			outcomes = append(outcomes, out)
		}
		s := corpus.Summarise(outcomes, en.e.Name(), en.e.Version())
		total := s.DecodedFirstPass + s.RecoveredByEngine
		var msPer float64
		if s.RecoveredByEngine > 0 {
			msPer = float64(s.TotalRecover.Milliseconds()) / float64(s.RecoveredByEngine)
		}
		fmt.Printf("%-16s %8d %10d %12d %9.1f%% %12.0f\n",
			en.name, s.DecodedFirstPass, s.RecoveredByEngine, total,
			100*float64(total)/float64(s.Frames), msPer)

		// Where each engine earned its recoveries, which is the part a headline rate hides.
		keys := make([]string, 0, len(s.RecoveredFrom))
		for b := range s.RecoveredFrom {
			keys = append(keys, string(b))
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("    from %-18s %d\n", k, s.RecoveredFrom[corpus.BucketKey(k)])
		}
	}
	fmt.Println()
}
