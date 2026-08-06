package pipeline

import "image"

// looksLikeAFrame is a cheap test for "is there anything on that screen".
//
// It exists to keep a camera that is waiting from filling the failure log. Locating a frame properly means
// finding fiducials, fitting a homography and sampling every cell — hundreds of milliseconds — and doing it on
// every image of a blank screen would spend the whole receiver on discovering nothing, as well as storing a
// picture of nothing each time.
//
// The test is a property every frame this protocol renders has and almost nothing else does: **a lot of pure
// black and a lot of pure white, at once**. The quiet zone, the four fiducials and the always-binary header and
// footer bands guarantee both. A dark room has the black and none of the white; a bright blank screen has the
// white and none of the black; a photograph of a desk has plenty of midtones and little of either.
//
// It is a gate, not a decision. Anything that passes still goes through the real decoder, which will reject it
// on its checksums if it was a false positive — so the threshold is set low enough to let a badly lit or
// off-axis frame through and accept the occasional wasted decode.
func looksLikeAFrame(img image.Image) bool {
	if img == nil {
		return false
	}
	bounds := img.Bounds()
	if bounds.Dx() < 32 || bounds.Dy() < 32 {
		return false
	}

	// Every eighth pixel in each direction: a sixty-fourth of the work, and a frame's bands are thousands of
	// pixels across so nothing that matters is missed.
	const step = 8
	var dark, light, total int
	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			r, g, b, _ := img.At(x, y).RGBA()
			// Rec. 601 luma on the 16-bit values the image package returns, shifted back to 0..255.
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(b>>8)) / 1000
			switch {
			case luma < 64:
				dark++
			case luma > 192:
				light++
			}
			total++
		}
	}
	if total == 0 {
		return false
	}

	// A twelfth of each. A frame's quiet zone alone is well past this, and it is far below what any evenly lit
	// scene produces at both ends of the range at once.
	const floor = 12
	return dark*floor >= total && light*floor >= total
}
