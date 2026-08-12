package soft

import "github.com/opticaltransport/otp/shared/encoding"

// LeastConfidentForTest exposes the selection for its own test.
//
// Kept out of the package's API because a caller has no use for it, but tested directly because a
// selection that silently returned the *most* confident cells would still produce a working search
// that almost never recovered anything — a failure indistinguishable from a difficult channel, and
// so one that could sit there for a long time looking like physics.
func LeastConfidentForTest(cells []encoding.SoftCell, k int) []encoding.SoftCell {
	return leastConfident(cells, k)
}
