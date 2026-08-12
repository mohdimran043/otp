// Package classify names the stage a decode failed at.
//
// A failed frame currently yields one error string, and the receiver counts it as "not decoded".
// That is enough to know a camera is unwell and useless for deciding what to do about it: a frame
// whose fiducials were never found needs a different intervention from one whose geometry locked
// perfectly and whose payload missed its checksum by three cells. The first is aim, focus or
// exposure; the second is recoverable in software.
//
// Bucketing them is therefore the first thing this layer does, and it is deliberately done before
// any recovery is attempted — the distribution over these buckets is what says whether recovery is
// worth attempting at all, and on this project's own measurements it was not obvious in advance.
// Clipping in particular correlates monotonically with how early the failure happens: 5.8% of
// pixels clipped still decoded, 17.4% located the geometry and failed the payload, and 25.8% could
// not find the fiducials, because a payload flattened to white swallows their rings. No amount of
// processing undoes a 255, so a corpus dominated by clipping is telling you to fix the capture,
// not to build a network.
package classify

import (
	"errors"
	"image"

	"github.com/opticaltransport/otp/shared/protocol"
)

// Bucket is the stage a frame failed at.
type Bucket string

const (
	// BucketDecoded is a frame that read cleanly.
	BucketDecoded Bucket = "decoded"

	// BucketNoQuad means fewer than four fiducials were found, so there was no geometry to work
	// from. Nothing that operates at a known geometry can help this one.
	BucketNoQuad Bucket = "no_quad"

	// BucketDegenerate means four fiducials were found but they were collinear or coincident, so
	// the homography is not invertible.
	BucketDegenerate Bucket = "degenerate_geometry"

	// BucketDescriptorCRC means the fiducials were found and the descriptor block beside one of
	// them could not be read, so the grid dimensions stayed unknown.
	BucketDescriptorCRC Bucket = "descriptor_crc"

	// BucketHeaderCRC means the header band failed its checksum even after majority voting across
	// its repeated copies.
	BucketHeaderCRC Bucket = "header_crc"

	// BucketFooterCRC means the footer band failed. Unrecoverable in the same way an unreadable
	// oracle is: the footer is what a corrected payload would have to be checked against.
	BucketFooterCRC Bucket = "footer_crc"

	// BucketPayloadCRC means the geometry and both bands read correctly and the payload did not.
	// This is the bucket worth working on: everything about the frame was right except some
	// number of individual cell decisions.
	BucketPayloadCRC Bucket = "payload_crc"

	// BucketBelowFloors means the frame decoded but its fiducial or timing score fell under the
	// receiver's confidence floor. A policy rejection rather than a channel failure.
	BucketBelowFloors Bucket = "below_floors"

	// BucketUnsupported means the frame came from a newer protocol version.
	BucketUnsupported Bucket = "unsupported_version"

	// BucketOther is anything unrecognised. A rising count here means this table has fallen
	// behind the decoder, which is worth surfacing rather than absorbing.
	BucketOther Bucket = "other"
)

// ErrBelowFloors marks a frame rejected by the receiver's confidence floors rather than by the
// protocol.
//
// It exists because those rejections were built with fmt.Errorf and carried their reason only in
// prose. Classifying them by matching that prose would break the first time someone improved the
// wording, so the receiver wraps this value instead.
var ErrBelowFloors = errors.New("receiver: decode confidence below the configured floors")

// Of maps a decode error onto the stage that produced it.
//
// The payload cases are tested before the band cases deliberately. An error can satisfy more than
// one test once the decoder has wrapped it with context, and a payload failure reported alongside
// header context belongs to the payload — which is where it actually went wrong, and the only
// attribution that leads an operator somewhere useful.
func Of(err error) Bucket {
	switch {
	case err == nil:
		return BucketDecoded
	case errors.Is(err, protocol.ErrFindersNotFound):
		return BucketNoQuad
	case errors.Is(err, protocol.ErrDegenerateGeometry):
		return BucketDegenerate
	case errors.Is(err, protocol.ErrDescriptorCRC):
		return BucketDescriptorCRC
	case errors.Is(err, protocol.ErrPayloadCRC), errors.Is(err, protocol.ErrPayloadHash):
		return BucketPayloadCRC
	case errors.Is(err, protocol.ErrFooterCRC):
		return BucketFooterCRC
	case errors.Is(err, protocol.ErrHeaderCRC):
		return BucketHeaderCRC
	case errors.Is(err, protocol.ErrUnsupportedVersion):
		return BucketUnsupported
	case errors.Is(err, ErrBelowFloors):
		return BucketBelowFloors
	default:
		return BucketOther
	}
}

// Recoverable reports whether a bucket describes a failure a geometry-aware retry could fix.
//
// Deliberately conservative. A bucket listed here is one where the frame is known to be present
// and located well enough to sample, and where the footer — the only thing that can confirm a
// correction — was readable. Everything else either has no geometry to work from, has no oracle,
// or is not a channel failure at all.
func Recoverable(b Bucket) bool {
	switch b {
	case BucketPayloadCRC, BucketDescriptorCRC, BucketHeaderCRC, BucketBelowFloors:
		return true
	default:
		return false
	}
}

// Clipped is the fraction of sampled pixels saturated in at least one channel.
//
// One channel rather than all three: a red cell clipped in red has lost the information that
// distinguished it from white, whatever the other channels say, and colour8 puts every symbol at a
// corner of the RGB cube precisely so that channels can be read independently.
//
// Sampled every eighth pixel in each direction, matching looksLikeAFrame, because clipping is a
// large-area property and a frame's bands are thousands of pixels across.
func Clipped(img image.Image) float64 {
	if img == nil {
		return 0
	}
	b := img.Bounds()
	const step = 8
	var clipped, total int
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 >= 255 || g>>8 >= 255 || bl>>8 >= 255 {
				clipped++
			}
			total++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(clipped) / float64(total)
}
