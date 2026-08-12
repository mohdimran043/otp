package pipeline

import (
	"context"
	"image"
	"image/color"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// These tests drive prepare, which is where the recovery attempt actually sits. A recovery that
// works in isolation and is never reached is the failure mode worth guarding against, and testing
// the search on its own — which receiver/ai/soft already does thoroughly — cannot catch it.
//
// prepare rather than Ingest because prepare needs no database: it stores the capture and decodes
// it, and both the object store and the decoder are real here. The database-backed path is covered
// by the ingest tests, which skip when no Postgres is reachable — and a wiring test that skips on
// the machine doing the wiring is worth very little.

// recoveryFixture is a receiver with a real object store, no database, and a session set, which is
// everything prepare touches.
func recoveryFixture(t *testing.T, recovery config.Recovery) *Receiver {
	t.Helper()

	objects, err := objectstore.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = objects.Close() })

	cfg := config.Default()
	cfg.Decoder.Recovery = recovery

	r := New(nil, objects, nil, noFrameSource{}, config.NewWatcher("", cfg), zap.NewNop())
	id := uuid.New()
	r.session.Store(&id)
	return r
}

// cubeEdgeNeighbours lists, for each colour8 symbol, the symbols one channel away — the other end
// of a cube edge.
//
// A plant has to move a cell along an edge of the RGB cube. Moving it across the interior lands
// nearer a third corner than the original, so the second-nearest entry is no longer the true symbol
// and a correct search produces the wrong answer. See receiver/ai/soft's perturb_test.go for the
// worked example.
var cubeEdgeNeighbours = map[uint32][]uint32{
	0: {1, 2, 3}, 1: {0, 5, 6}, 2: {0, 4, 6}, 3: {0, 4, 5},
	4: {2, 3, 7}, 5: {1, 3, 7}, 6: {1, 2, 7}, 7: {4, 5, 6},
}

// plantedFrame renders a colour8 frame at the geometry that achieved a byte-exact camera transfer
// here, and pushes n payload cells just past the decision boundary toward a neighbouring symbol.
func plantedFrame(t *testing.T, n int) (*image.RGBA, []byte) {
	t.Helper()

	l, err := protocol.NewLayout(80, 80, 8)
	require.NoError(t, err)

	capacity, err := encoding.Color8.EstimateCapacity(l, 3)
	require.NoError(t, err)
	payload := make([]byte, capacity.PayloadBytes)
	for i := range payload {
		payload[i] = byte(i * 11)
	}

	f := protocol.NewFrame(protocol.Header{TransmissionID: uuid.New()}, payload)
	img, err := encoding.Color8.Encode(f, l, 3)
	require.NoError(t, err)

	cells := l.PayloadCells()
	for i := 0; i < n; i++ {
		cell := cells[(i*len(cells)/n+37)%len(cells)]
		rect := l.CellRect(cell)
		mid := color.RGBAModel.Convert(img.At(rect.Min.X+rect.Dx()/2, rect.Min.Y+rect.Dy()/2)).(color.RGBA)
		symbol := encoding.Color8Palette.Value(mid)
		neighbours := cubeEdgeNeighbours[symbol]
		toward := encoding.Color8Palette.Colors[neighbours[i%len(neighbours)]]

		// 0.55 of the way along the edge: the match flips and the margin left behind is a tenth of
		// the edge, which is what makes the cell rank first in the search.
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				from := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
				img.Set(x, y, color.RGBA{
					R: uint8(float64(from.R) + (float64(toward.R)-float64(from.R))*0.55),
					G: uint8(float64(from.G) + (float64(toward.G)-float64(from.G))*0.55),
					B: uint8(float64(from.B) + (float64(toward.B)-float64(from.B))*0.55),
					A: 255,
				})
			}
		}
	}
	return img, payload
}

// TestRecoveryTurnsAFailedFrameIntoADecodedOne is the wiring claim: the same image is a failure
// with recovery off and a decoded frame with it on.
//
// Both directions matter. Asserting only the enabled case would pass just as well if the plants
// were too weak to break the frame in the first place, which would make the test a tautology.
func TestRecoveryTurnsAFailedFrameIntoADecodedOne(t *testing.T) {
	img, payload := plantedFrame(t, 3)
	ctx := context.Background()

	off := recoveryFixture(t, config.Recovery{Enabled: false})
	result := off.prepare(ctx, Capture{Image: img})
	require.NoError(t, result.err, "storing the capture must succeed")
	require.Error(t, result.decodeErr, "three cells over the boundary must fail the payload")
	require.Nil(t, result.frame)
	require.Equal(t, uint64(0), off.RecoveryStats().Attempted, "disabled recovery must not be attempted")

	on := recoveryFixture(t, config.Recovery{Enabled: true, MaxCells: 12, MaxCandidates: 4096})
	result = on.prepare(ctx, Capture{Image: img})
	require.NoError(t, result.err)
	require.NoError(t, result.decodeErr, "recovery should have repaired the frame")
	require.NotNil(t, result.frame)
	require.Equal(t, payload, result.frame.Payload, "the recovered bytes must be the original bytes")

	stats := on.RecoveryStats()
	require.Equal(t, uint64(1), stats.Attempted)
	require.Equal(t, uint64(1), stats.Recovered)
	require.Positive(t, stats.Candidates)
	require.Equal(t, uint64(1), stats.Buckets[string(classify.BucketDecoded)],
		"a recovered frame must be counted as decoded, since that is what it now is")
}

// TestRecoveryBucketsAFrameItCannotFix records that an unrecoverable frame is still classified, so
// the panel says which stage it died at rather than only that recovery failed.
func TestRecoveryBucketsAFrameItCannotFix(t *testing.T) {
	img, _ := plantedFrame(t, 200)

	r := recoveryFixture(t, config.Recovery{Enabled: true, MaxCells: 12, MaxCandidates: 4096})
	result := r.prepare(context.Background(), Capture{Image: img})
	require.Error(t, result.decodeErr)

	stats := r.RecoveryStats()
	require.Equal(t, uint64(0), stats.Recovered)
	require.Equal(t, uint64(1), stats.Attempted+stats.Buckets[string(classify.BucketNoQuad)],
		"the frame was either searched or lost its geometry, and either way it is counted")
	require.NotEmpty(t, stats.Buckets)
}

// TestRecoveryIsNotAttemptedWithoutGeometry guards the gate on the failure bucket. A frame with no
// fiducials has no geometry to search at, and attempting anyway would spend a sampling pass on an
// image that is not a frame — the case the tone gate exists to keep off the decoder entirely.
func TestRecoveryIsNotAttemptedWithoutGeometry(t *testing.T) {
	blank := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			blank.Set(x, y, color.RGBA{R: 40, G: 40, B: 40, A: 255})
		}
	}

	r := recoveryFixture(t, config.Recovery{Enabled: true, MaxCells: 12, MaxCandidates: 4096})
	result := r.prepare(context.Background(), Capture{Image: blank})
	require.Error(t, result.decodeErr)
	require.Equal(t, uint64(0), r.RecoveryStats().Attempted)
	require.Equal(t, uint64(1), r.RecoveryStats().Buckets[string(classify.BucketNoQuad)])
}

// TestRecoveryCountersResetWithTheSession records that the figures are session-scoped, because a
// lifetime average reads healthy all afternoon once anything has ever worked.
func TestRecoveryCountersResetWithTheSession(t *testing.T) {
	img, _ := plantedFrame(t, 3)
	r := recoveryFixture(t, config.Recovery{Enabled: true, MaxCells: 12, MaxCandidates: 4096})
	r.prepare(context.Background(), Capture{Image: img})
	require.Equal(t, uint64(1), r.RecoveryStats().Recovered)

	r.recovery.reset()
	stats := r.RecoveryStats()
	require.Equal(t, uint64(0), stats.Recovered)
	require.Equal(t, uint64(0), stats.Attempted)
	require.Empty(t, stats.Buckets)
}
