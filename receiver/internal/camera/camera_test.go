package camera

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// devices is a camera that behaves like the ones this is written for: an integrated webcam offering the
// same sizes in a compressed format at full rate and an uncompressed one at a fraction of it.
func devices() []Device {
	return []Device{{
		Path:    "/dev/video0",
		Name:    "Integrated Camera",
		Driver:  "uvcvideo",
		Default: true,
		Modes: []Mode{
			{Format: "MJPG", Width: 1920, Height: 1080, FPS: []float64{30}},
			{Format: "YUYV", Width: 1920, Height: 1080, FPS: []float64{5}},
			{Format: "MJPG", Width: 1280, Height: 720, FPS: []float64{60, 30}},
		},
	}, {
		Path:   "/dev/video2",
		Name:   "Industrial Camera",
		Driver: "uvcvideo",
		Modes: []Mode{
			{Format: "MJPG", Width: 3840, Height: 2160, FPS: []float64{25}},
		},
	}}
}

// TestBestPrefersResolutionThenRate pins the ordering decision. Cells the camera cannot resolve do not
// decode at all; a slow camera merely makes the sender wait, which the acknowledgement rule makes safe.
func TestBestPrefersResolutionThenRate(t *testing.T) {
	best, ok := Best(devices()[0].Modes)
	require.True(t, ok)
	require.Equal(t, 1920, best.Width)
	require.Equal(t, "MJPG", best.Format,
		"at equal size the faster format must win: MJPG at 30 fps over YUYV at 5")

	best, ok = Best([]Mode{
		{Format: "MJPG", Width: 1280, Height: 720, FPS: []float64{30}},
		{Format: "MJPG", Width: 1280, Height: 720, FPS: []float64{60}},
	})
	require.True(t, ok)
	require.Equal(t, []float64{60}, best.FPS)

	_, ok = Best(nil)
	require.False(t, ok)
}

func TestSelectedFallsBackToTheDefault(t *testing.T) {
	list := devices()

	device, substituted, ok := Selected(list, "/dev/video2")
	require.True(t, ok)
	require.False(t, substituted)
	require.Equal(t, "/dev/video2", device.Path)

	device, substituted, ok = Selected(list, "")
	require.True(t, ok)
	require.False(t, substituted, "asking for nothing and getting the default is not a substitution")
	require.Equal(t, "/dev/video0", device.Path)

	// A camera unplugged between deploys. The substitution is reported rather than hidden, because
	// capturing from a different camera than the one configured is a thing an operator must be told.
	device, substituted, ok = Selected(list, "/dev/video9")
	require.True(t, ok)
	require.True(t, substituted)
	require.Equal(t, "/dev/video0", device.Path)

	_, _, ok = Selected(nil, "/dev/video0")
	require.False(t, ok)
}

func TestPreferredIsTheDefaultCameraAtItsBest(t *testing.T) {
	selection, ok := Preferred(devices())
	require.True(t, ok)
	require.Equal(t, Selection{Device: "/dev/video0", Format: "MJPG", Width: 1920, Height: 1080, FPS: 30},
		selection)

	_, ok = Preferred(nil)
	require.False(t, ok)
}

// TestValidateRefusesModesTheCameraDoesNotHave is the check that matters most, because a driver given an
// unsupported mode substitutes one rather than failing — and a receiver that asked for 1920×1080 and
// silently got 640×480 reports the consequence as an optical fault.
func TestValidateRefusesModesTheCameraDoesNotHave(t *testing.T) {
	list := devices()

	require.NoError(t, Selection{}.Validate(list), "choosing nothing is always valid")
	require.NoError(t, Selection{Device: "/dev/video0"}.Validate(list))
	require.NoError(t, Selection{Device: "/dev/video0", Format: "MJPG", Width: 1920, Height: 1080, FPS: 30}.Validate(list))
	require.NoError(t, Selection{Device: "/dev/video0", Width: 1280, Height: 720, FPS: 60}.Validate(list))

	require.ErrorContains(t, Selection{Device: "/dev/video7"}.Validate(list), "not an attached capture device")
	require.ErrorContains(t,
		Selection{Device: "/dev/video0", Width: 4096, Height: 2160}.Validate(list),
		"does not offer 4096×2160")
	require.ErrorContains(t,
		Selection{Device: "/dev/video0", Format: "H264", Width: 1920, Height: 1080}.Validate(list),
		"does not offer the H264 format")
	require.ErrorContains(t,
		Selection{Device: "/dev/video0", Width: 1920, Height: 1080, FPS: 120}.Validate(list),
		"does not offer 120 fps")
	require.ErrorContains(t, Selection{Device: "/dev/video0", Width: 1920}.Validate(list),
		"a width needs a height")
	require.ErrorContains(t, Selection{Device: "/dev/video0", FPS: -1}.Validate(list), "cannot be negative")
	require.ErrorContains(t, Selection{Device: "/dev/video0"}.Validate(nil), "not an attached capture device")
}

// TestValidateToleratesRationalFrameRates covers 29.97, which V4L2 reports as 30000/1001 and which is
// therefore never exactly what a UI sends back.
func TestValidateToleratesRationalFrameRates(t *testing.T) {
	list := []Device{{
		Path:    "/dev/video0",
		Default: true,
		Modes:   []Mode{{Format: "MJPG", Width: 1920, Height: 1080, FPS: []float64{30000.0 / 1001.0}}},
	}}
	require.NoError(t, Selection{Device: "/dev/video0", Width: 1920, Height: 1080, FPS: 29.97}.Validate(list))
	require.Error(t, Selection{Device: "/dev/video0", Width: 1920, Height: 1080, FPS: 30}.Validate(list))
}

func TestSelectionSurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	loaded, err := LoadSelection(dir)
	require.NoError(t, err)
	require.True(t, loaded.Zero(), "nothing chosen yet")

	chosen := Selection{Device: "/dev/video0", Format: "MJPG", Width: 1920, Height: 1080, FPS: 30}
	require.NoError(t, SaveSelection(dir, chosen))

	loaded, err = LoadSelection(dir)
	require.NoError(t, err)
	require.Equal(t, chosen, loaded)
	require.False(t, loaded.Zero())
}

func TestSelectionStringReadsLikeASentence(t *testing.T) {
	require.Equal(t, "default camera", Selection{}.String())
	require.Equal(t, "/dev/video0", Selection{Device: "/dev/video0"}.String())
	require.Equal(t, "/dev/video0 at 1920×1080, 30 fps MJPG",
		Selection{Device: "/dev/video0", Width: 1920, Height: 1080, FPS: 30, Format: "MJPG"}.String())
}

func TestModeLabel(t *testing.T) {
	require.Equal(t, "1920×1080 at 30 fps (MJPG)",
		Mode{Format: "MJPG", Width: 1920, Height: 1080, FPS: []float64{30}}.Label())
	require.Equal(t, "640×480 YUYV", Mode{Format: "YUYV", Width: 640, Height: 480}.Label())
}
