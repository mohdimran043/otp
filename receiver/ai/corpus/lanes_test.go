package corpus_test

import (
	"context"
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/receiver/ai/engine"
	"github.com/opticaltransport/otp/receiver/ai/soft"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"

	_ "image/jpeg"
	_ "image/png"
)

// Every lane of a tiled capture must get the full read, recovery included.
//
// The receiver ran recovery for one lane of each photograph and gave the rest a bare decode, which
// looked reasonable and cost most of a tiled transfer. The numbers say why: over six real two-lane
// captures, a raw decode reads 1 lane of 12, and the same 12 all read once recovery is allowed to
// run on each of them. A tiled display therefore does not merely benefit from per-lane recovery, it
// depends on it — the raw decode is the exception on a camera, not the rule.
//
// Env-gated like the rest of this package: it needs real captures and a running model server, and a
// test that silently passes without them would be worse than absent. Point OTP_LANE_DIR at a
// directory of captures pulled from the receiver's object store.
func TestRealLanesWithRecovery(t *testing.T) {
	dir := os.Getenv("OTP_LANE_DIR")
	if dir == "" {
		t.Skip("set OTP_LANE_DIR to a directory of captures, with a model server on :9800")
	}
	files, _ := filepath.Glob(dir + "/*")
	if len(files) == 0 {
		t.Skip("no captures")
	}

	ctx := context.Background()
	eng, err := engine.Open(ctx, engine.Settings{
		Enabled:        true,
		Engine:         "classifier",
		SidecarURL:     "http://localhost:9800",
		SidecarTimeout: 60 * time.Second,
		Search: soft.Options{
			MaxCells:      12,
			MaxCandidates: 4096,
			Budget:        50 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Logf("engine=%s version=%s", eng.Name(), eng.Version())

	var lanes, rawOK, afterRecovery int
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		img, _, err := image.Decode(fh)
		fh.Close()
		if err != nil {
			continue
		}

		opts := protocol.LocateOptions{}
		found := protocol.LocateAll(img, opts, 4)

		var perFile []string
		for _, g := range found {
			lanes++
			frame, derr := encoding.DecodeAt(g, img, opts)
			if derr == nil {
				rawOK++
				afterRecovery++
				perFile = append(perFile, "raw-ok")
				continue
			}
			res, rerr := eng.Recover(ctx, engine.Request{
				Image:    img,
				Geometry: g,
				Bucket:   classify.Of(derr),
				Clipped:  classify.Clipped(img),
			})
			if rerr == nil && res.Frame != nil {
				afterRecovery++
				perFile = append(perFile, "recovered("+res.Report.Engine+")")
			} else {
				perFile = append(perFile, "lost")
			}
			_ = frame
		}
		t.Logf("%-14s lanes=%d %v", filepath.Base(f)[:12], len(found), perFile)
	}

	t.Logf("TOTAL lanes=%d  raw-decode-ok=%d  after-recovery=%d", lanes, rawOK, afterRecovery)
}
