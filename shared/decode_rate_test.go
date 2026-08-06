package shared_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"

	"github.com/google/uuid"
)

// TestDecodeCost measures how long one frame takes to decode.
//
// It is the figure that actually sets a deployment's throughput, and it is not the same as the channel's
// capacity: the display can show frames faster than a receiver can read them, and when it does the extra
// frames become keep-alive repeats rather than progress. Measuring it says how many cores a given frame rate
// needs.
func TestDecodeCost(t *testing.T) {
	for _, g := range []struct {
		w, h, cell int
		label      string
	}{
		{96, 96, 6, "96x96 @6px"},
		{128, 128, 8, "128x128 @8px"},
		// Eight-pixel cells rather than four. At four, a typical optical path's blur is over a quarter of a
		// cell width, which the operating envelope puts past the limit — and it is: the frame decodes
		// pristine and fails through the optics. A 4K panel has the pixels for eight-pixel cells at this
		// grid, so that is the honest configuration to measure.
		{256, 256, 8, "256x256 @8px"},
	} {
		layout, err := protocol.NewLayout(g.w, g.h, g.cell)
		if err != nil {
			t.Fatal(err)
		}
		encoder, err := encoding.ByName("color8")
		if err != nil {
			t.Fatal(err)
		}
		capacity, err := encoder.EstimateCapacity(layout, 0)
		if err != nil {
			t.Fatal(err)
		}

		payload := make([]byte, capacity.PayloadBytes)
		for i := range payload {
			payload[i] = byte(i*31 + 7)
		}
		frame := protocol.NewFrame(protocol.Header{TransmissionID: uuid.New()}, payload)
		img, err := encoder.Encode(frame, layout, 0)
		if err != nil {
			t.Fatal(err)
		}

		for _, channel := range []struct {
			name    string
			profile simulate.Profile
		}{
			{"pristine", simulate.Pristine},
			{"typical optics", simulate.Typical},
		} {
			captured := channel.profile.Apply(img)

			// Warm once, then measure: the first decode pays for lazily built tables.
			if _, err := encoding.Decode(captured, protocol.LocateOptions{}); err != nil {
				t.Fatalf("%s %s: %v", g.label, channel.name, err)
			}

			const runs = 12
			started := time.Now()
			for i := 0; i < runs; i++ {
				if _, err := encoding.Decode(captured, protocol.LocateOptions{}); err != nil {
					t.Fatalf("%s %s: %v", g.label, channel.name, err)
				}
			}
			each := time.Since(started) / runs
			perSecond := float64(time.Second) / float64(each)

			fmt.Printf("%-14s %-16s %7s per frame  %6.1f frames/s/core  %8.0f KB/s/core\n",
				g.label, channel.name, each.Round(time.Millisecond*1/10),
				perSecond, perSecond*float64(capacity.PayloadBytes)/1000)
		}
	}
}
