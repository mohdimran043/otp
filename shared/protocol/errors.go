package protocol

import "errors"

// Sentinel errors returned across the protocol package. Callers match on these
// with errors.Is; the receiver's decode pipeline uses them to decide whether a
// frame is worth retrying, worth reporting as a bit error, or simply noise the
// camera picked up between real frames.
var (
	// ErrUnsupportedVersion means the frame was produced by a newer protocol.
	ErrUnsupportedVersion = errors.New("protocol: unsupported version")

	// ErrBadMagic means the header or footer magic did not match, which usually
	// means the sampled region was not an OTP frame at all.
	ErrBadMagic = errors.New("protocol: bad magic")

	// ErrHeaderCRC means the header band failed its checksum even after majority
	// voting across its repeated copies.
	ErrHeaderCRC = errors.New("protocol: header CRC mismatch")

	// ErrFooterCRC means the footer band failed its checksum.
	ErrFooterCRC = errors.New("protocol: footer CRC mismatch")

	// ErrPayloadCRC means the payload did not match the CRC32 in the footer.
	ErrPayloadCRC = errors.New("protocol: payload CRC mismatch")

	// ErrPayloadHash means the payload did not match the SHA-256 in the footer.
	ErrPayloadHash = errors.New("protocol: payload hash mismatch")

	// ErrShortBuffer means a record was truncated.
	ErrShortBuffer = errors.New("protocol: buffer too short")

	// ErrGridTooSmall means the requested grid cannot hold the header band, the
	// footer band, and at least one payload row.
	ErrGridTooSmall = errors.New("protocol: grid too small for header and footer bands")

	// ErrGridBounds means a grid dimension was outside the permitted range.
	ErrGridBounds = errors.New("protocol: grid dimension out of range")

	// ErrPayloadTooLarge means the payload exceeds what the grid can carry.
	ErrPayloadTooLarge = errors.New("protocol: payload exceeds grid capacity")

	// ErrFindersNotFound means fewer than four finder patterns were located, so
	// no homography could be computed.
	ErrFindersNotFound = errors.New("protocol: could not locate four finder patterns")

	// ErrDegenerateGeometry means the located finders are collinear or
	// coincident, so the homography is not invertible.
	ErrDegenerateGeometry = errors.New("protocol: finder geometry is degenerate")
)
