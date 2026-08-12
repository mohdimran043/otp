//go:build linux

package pipeline

import (
	"image"
	"image/color"
	"math/rand"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// A real rendered frame, made the way the sender makes them.
func realFrame(t *testing.T) image.Image {
	t.Helper()
	layout, err := protocol.NewLayout(96, 96, 6)
	require.NoError(t, err)
	enc, err := encoding.ByName("color8")
	require.NoError(t, err)

	capacity, err := enc.EstimateCapacity(layout, 0)
	require.NoError(t, err)
	payload := make([]byte, capacity.PayloadBytes)
	for i := range payload {
		payload[i] = byte(i * 7)
	}

	frame := protocol.NewFrame(protocol.Header{FrameNumber: 1, TotalChunks: 1}, payload)
	img, err := enc.Encode(frame, layout, 0)
	require.NoError(t, err)
	return img
}

// TestTheIdleGateLetsARealFrameThrough is the half that must never be wrong: a frame the sender actually
// rendered has to reach the decoder, or the receiver would sit idle while a transfer was on screen.
func TestTheIdleGateLetsARealFrameThrough(t *testing.T) {
	require.True(t, looksLikeAFrame(realFrame(t), defaultMinToneFraction),
		"a rendered frame must pass the gate that decides whether to decode at all")
}

// TestTheIdleGateRejectsWhatAWaitingCameraSees is the other half. Each of these is what a camera produces while
// a transfer has not started, and each would otherwise be persisted and recorded as an unreadable capture —
// thousands of rows saying nothing but "not yet".
// enabledToneFraction is the historical twelfth, used here because these cases document what the gate
// turns away *when it is switched on*. The default is now zero — see defaultMinToneFraction for the two
// measured outages that decided that — so testing them against the default would assert the opposite.
const enabledToneFraction = 1.0 / 12.0

// TestTheDefaultGateLetsEverythingThrough pins the default itself, because it is a deliberate reversal and
// the kind of thing a later tidy-up would quietly restore.
//
// Measured 2026-08-13: at 0.02 the gate rejected 312 of 312 posted frames from a phone filming a monitor,
// with the operator watching a preview that showed the frame perfectly. A gated frame reaches neither the
// decoder nor the failure log, so that reads as "camera not running" and has cost debugging time three
// times now. A bounded waste of decode attempts is the cheaper failure.
func TestTheDefaultGateLetsEverythingThrough(t *testing.T) {
	const w, h = 640, 480
	blank := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(blank.Pix); i += 4 {
		blank.Pix[i], blank.Pix[i+1], blank.Pix[i+2], blank.Pix[i+3] = 128, 128, 128, 255
	}
	require.True(t, looksLikeAFrame(blank, defaultMinToneFraction),
		"the default must not reject anything; the decoder's checksums are the filter")

	// The size floor still applies, whatever the threshold: a thumbnail cannot be a frame.
	tiny := image.NewRGBA(image.Rect(0, 0, 16, 16))
	require.False(t, looksLikeAFrame(tiny, defaultMinToneFraction))
}

func TestTheIdleGateRejectsWhatAWaitingCameraSees(t *testing.T) {
	const w, h = 640, 480

	dark := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range dark.Pix {
		dark.Pix[i] = 0
	}
	require.False(t, looksLikeAFrame(dark, enabledToneFraction), "a dark room or a screen that is off")

	white := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(white.Pix); i += 4 {
		white.Pix[i], white.Pix[i+1], white.Pix[i+2], white.Pix[i+3] = 255, 255, 255, 255
	}
	require.False(t, looksLikeAFrame(white, enabledToneFraction), "a blank white screen")

	grey := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(grey.Pix); i += 4 {
		grey.Pix[i], grey.Pix[i+1], grey.Pix[i+2], grey.Pix[i+3] = 128, 128, 128, 255
	}
	require.False(t, looksLikeAFrame(grey, enabledToneFraction), "a mid-grey desktop")

	// A photograph of a room: mid-tones with noise, which is what a misaimed camera sees.
	room := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(90 + r.Intn(70))
			room.Set(x, y, color.RGBA{v, v, uint8(int(v) * 9 / 10), 255})
		}
	}
	require.False(t, looksLikeAFrame(room, enabledToneFraction), "a camera pointed at a room rather than a display")

	require.False(t, looksLikeAFrame(nil, defaultMinToneFraction), "no image at all")
	require.False(t, looksLikeAFrame(image.NewRGBA(image.Rect(0, 0, 8, 8)), defaultMinToneFraction), "too small to be a frame")
}

// TestTheIdleGateSurvivesTheRealCamera runs against the attached camera when asked. It is a probe: it says
// whether this room, at this moment, would be mistaken for a transfer in progress.
func TestTheIdleGateSurvivesTheRealCamera(t *testing.T) {
	if os.Getenv("OTP_CAMERA_PROBE") == "" {
		t.Skip("set OTP_CAMERA_PROBE to look through the real camera")
	}
	t.Skip("covered by the camera package's probe; kept as a marker of what to check by hand")
}
