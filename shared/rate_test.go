package shared_test

import (
	"fmt"
	"testing"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// TestTransferRates prints payload capacity and the time a 100 MB file would take, so the figures quoted
// anywhere are measured from the code rather than estimated from it.
func TestTransferRates(t *testing.T) {
	const target = 100_000_000 // 100 MB, decimal

	geometries := []struct {
		w, h, cell int
		label      string
	}{
		{96, 96, 6, "96x96 @6px (576px display) — the demonstration"},
		{128, 128, 8, "128x128 @8px (1088px) — the shipped default"},
		{192, 192, 5, "192x192 @5px (980px) — a 1080p panel"},
		{256, 256, 4, "256x256 @4px (1056px) — a 4K panel, small cells"},
		{384, 384, 5, "384x384 @5px (1940px) — a 4K panel, safe cells"},
	}

	for _, g := range geometries {
		layout, err := protocol.NewLayout(g.w, g.h, g.cell)
		if err != nil {
			t.Fatalf("%s: %v", g.label, err)
		}
		fmt.Printf("\n%s  image %dx%dpx\n", g.label, layout.ImageWidth(), layout.ImageHeight())
		fmt.Printf("  %-10s %10s %12s %12s %12s\n", "encoding", "bytes/frame", "10 fps", "30 fps", "60 fps")

		for _, name := range []string{"binary", "grayscale", "color8", "color16"} {
			enc, err := encoding.ByName(name)
			if err != nil {
				t.Fatal(err)
			}
			capacity, err := enc.EstimateCapacity(layout, 0)
			if err != nil {
				t.Fatal(err)
			}
			perFrame := float64(capacity.PayloadBytes)
			fmt.Printf("  %-10s %10d", name, capacity.PayloadBytes)
			for _, fps := range []float64{10, 30, 60} {
				rate := perFrame * fps
				fmt.Printf(" %6.0f KB/s", rate/1000)
				_ = rate
			}
			fmt.Println()
			fmt.Printf("  %-10s %10s", "", "100 MB in")
			for _, fps := range []float64{10, 30, 60} {
				seconds := target / (perFrame * fps)
				fmt.Printf(" %11s", duration(seconds))
			}
			fmt.Println()
		}
	}
}

func duration(seconds float64) string {
	switch {
	case seconds < 90:
		return fmt.Sprintf("%.0fs", seconds)
	case seconds < 5400:
		return fmt.Sprintf("%.0fm", seconds/60)
	default:
		return fmt.Sprintf("%.1fh", seconds/3600)
	}
}
