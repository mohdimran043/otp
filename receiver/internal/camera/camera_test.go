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

	require.ErrorContains(t, Selection{Device: "/dev/video7"}.Validate(list),
		"not one of the 2 attached capture devices")
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
	// With devices to check against, a mode the camera does not have is refused.
	require.ErrorContains(t, Selection{Device: "/dev/video9", Width: 640, Height: 480}.Validate(list),
		"not one of the 2 attached capture devices")
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

// TestValidateAcceptsAChoiceWhenNothingCanBeEnumerated is the case that matters in development and inside a
// container that has not been given a camera yet.
//
// Refusing a mode the camera says it cannot do is only defensible when the camera can be asked. When
// enumeration finds nothing, every choice would be refused and an operator could configure nothing at all —
// so their word is taken instead. They can see the device on the host, or they are configuring for a
// passthrough that arrives at the next restart.
func TestValidateAcceptsAChoiceWhenNothingCanBeEnumerated(t *testing.T) {
	for _, path := range []string{"/dev/video0", "/dev/video11", "/dev/v4l/by-id/usb-Acme_Cam", "0", "3"} {
		require.NoError(t, Selection{Device: path}.Validate(nil), "%q", path)
		require.NoError(t,
			Selection{Device: path, Width: 1920, Height: 1080, FPS: 60, Format: "MJPG"}.Validate(nil),
			"a mode cannot be checked either, so it is taken on trust: %q", path)
	}
}

// TestValidateStillCatchesTyposWithNothingToEnumerate keeps "take the operator's word" from becoming "accept
// anything", because a typo that silently becomes the configuration is worse than a refusal.
func TestValidateStillCatchesTyposWithNothingToEnumerate(t *testing.T) {
	for _, path := range []string{"video0", "camera", " /dev/video0", "/dev/video0 ", "99", "-1", "./cam"} {
		require.Error(t, Selection{Device: path}.Validate(nil), "%q should be refused", path)
	}
	require.ErrorContains(t, Selection{Width: 640, Height: 480}.Validate(nil),
		"nothing to select", "no device named and none attached")
}
