// Package dataset labels cell patches for training the optical symbol classifier.
//
// The sampling itself lives in shared/cellpatch, imported by both this exporter and the receiver's
// inference path, so training features and production features cannot drift apart. What remains here is
// the part only the sender can do: saying what each cell was *supposed* to be.
//
// The sender is the right side for that because it rendered the frame. A receiver looking at a photograph
// does not know the truth, and a classifier trained on the receiver's own guesses would learn to reproduce
// them — including their mistakes, which are precisely what it is meant to fix.
package dataset

import (
	"fmt"
	"image"
	"io"

	"github.com/opticaltransport/otp/shared/cellpatch"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Re-exported so a caller writing a dataset need only import this package.
const (
	PatchSide   = cellpatch.PatchSide
	PatchSpan   = cellpatch.PatchSpan
	Channels    = cellpatch.Channels
	RecordBytes = cellpatch.RecordBytes
)

// Export writes one labelled record per payload cell. See cellpatch.Export.
func Export(w io.Writer, img image.Image, g *protocol.Geometry, truth []uint32, frame uint32) (int, error) {
	return cellpatch.Export(w, img, g, truth, frame)
}

// TruthFor returns the symbol sequence a pristine render carries, proven against its own footer.
//
// Derived by soft-reading the encoder's own output rather than by re-packing the payload bytes, because
// re-packing would need the encoder's private symbol packing and a second implementation of it could drift.
// Reading the pristine render instead is exact for a different reason, and the reason is checkable: the
// symbols are unpacked to a payload and that payload is verified against the footer's CRC32 and SHA-256, so
// a returned label set has been *proved* correct rather than assumed. If verification fails the labels are
// refused, because mislabelled training data is worse than none.
func TruthFor(pristine image.Image, g *protocol.Geometry) ([]uint32, error) {
	r, err := encoding.SoftRead(g, pristine)
	if err != nil {
		return nil, fmt.Errorf("dataset: could not read the pristine render: %w", err)
	}
	if _, err := r.Verify(r.Symbols); err != nil {
		return nil, fmt.Errorf("dataset: the pristine render does not verify, so its labels are not trustworthy: %w", err)
	}
	return r.Symbols, nil
}
