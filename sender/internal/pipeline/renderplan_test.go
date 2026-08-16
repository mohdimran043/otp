package pipeline

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/store"
)

// The plan is what makes rendering parallel, so it is the part worth pinning.
//
// Every decision that depends on a frame's position is made here and nowhere else: the numbering, where the
// manifest repeats, and the flags that mark the last chunk and the end of the stream. If any of that leaked
// into the renderer it would be order-dependent, and the renderer runs in no particular order at all.

func planChunks(n, parity int) []store.Chunk {
	chunks := make([]store.Chunk, 0, n+parity)
	for i := range n {
		chunks = append(chunks, store.Chunk{ID: uuid.New(), ESI: i})
	}
	for i := range parity {
		chunks = append(chunks, store.Chunk{ID: uuid.New(), ESI: n + i, IsParity: true})
	}
	return chunks
}

func planOf(t *testing.T, chunks []store.Chunk, sourceChunks, interval int) []plannedFrame {
	t.Helper()
	return planFrames(store.Transmission{ID: uuid.New()}, protocol.Manifest{}, uuid.New(),
		chunks, sourceChunks, interval)
}

// Frame numbers are dense, ascending, and match each entry's index in the output slice.
//
// The renderer writes results at out[number], so a gap or a repeat here would silently leave a zero-valued
// frame record in the middle of a transmission — a row with no stored image, discovered at display time.
func TestEveryPlannedFrameHasItsOwnIndex(t *testing.T) {
	plan := planOf(t, planChunks(50, 5), 50, 8)

	require.NotEmpty(t, plan)
	for i, planned := range plan {
		assert.Equal(t, i, planned.number,
			"frame numbers are the output slice's indices, so they must be dense and in order")
	}
}

// The manifest leads and then repeats at the configured interval.
func TestTheManifestLeadsAndRepeats(t *testing.T) {
	const interval = 8
	plan := planOf(t, planChunks(40, 0), 40, interval)

	require.True(t, plan[0].manifest, "a receiver watching from the start should learn what is coming first")

	var manifests int
	for _, planned := range plan {
		if planned.manifest {
			manifests++
			assert.Zero(t, planned.number%interval,
				"a repeat should land on the interval, so a camera joining late waits a bounded time")
		}
	}
	assert.Greater(t, manifests, 1,
		"the manifest repeats so a camera coming online mid-transfer can join the stream")
}

// Turning the interval off leaves exactly one manifest.
func TestNoIntervalMeansOneManifest(t *testing.T) {
	plan := planOf(t, planChunks(30, 0), 30, 0)

	var manifests int
	for _, planned := range plan {
		if planned.manifest {
			manifests++
		}
	}
	assert.Equal(t, 1, manifests)
}

// Every chunk is planned exactly once, parity included.
func TestEveryChunkIsPlannedOnce(t *testing.T) {
	chunks := planChunks(64, 10)
	plan := planOf(t, chunks, 64, 8)

	seen := map[uuid.UUID]int{}
	for _, planned := range plan {
		if !planned.manifest {
			seen[planned.chunk.ID]++
		}
	}

	require.Len(t, seen, len(chunks), "every chunk should appear, and none twice")
	for id, n := range seen {
		assert.Equal(t, 1, n, "chunk %s was planned %d times", id, n)
	}
}

// The flags that depend on position are set on the right frames.
//
// last-chunk marks the final *source* chunk, which is not the final frame when parity follows it, and
// end-of-stream marks the final frame whatever it carries. Getting these the same would tell a receiver the
// data was complete while parity was still arriving.
func TestPositionalFlagsLandOnTheRightFrames(t *testing.T) {
	const source, parity = 20, 5
	chunks := planChunks(source, parity)
	plan := planOf(t, chunks, source, 0)

	var lastChunk, endOfStream, parityFlagged int
	for _, planned := range plan {
		if planned.manifest {
			continue
		}
		if planned.flags.Has(protocol.FlagLastChunk) {
			lastChunk++
			assert.False(t, planned.chunk.IsParity, "the last source chunk is not a parity one")
			assert.Equal(t, source-1, planned.chunk.ESI)
		}
		if planned.flags.Has(protocol.FlagEndOfStream) {
			endOfStream++
			assert.Equal(t, plan[len(plan)-1].number, planned.number,
				"end of stream is the final frame, which is parity when parity is sent")
		}
		if planned.flags.Has(protocol.FlagParity) {
			parityFlagged++
		}
	}

	assert.Equal(t, 1, lastChunk)
	assert.Equal(t, 1, endOfStream)
	assert.Equal(t, parity, parityFlagged, "every parity chunk should be marked as one")
}

// A smaller grid is why this stage needed the help: it plans far more frames for the same file.
//
// Not a property of the planner so much as a statement of the problem it was changed for. Grid 60 carries
// well under half the payload of grid 80 per frame, so the same transfer becomes twice the encoding work —
// and it was the work that ran one frame at a time.
func TestASmallerGridPlansMoreFrames(t *testing.T) {
	// Standing in for two geometries by their chunk counts, since the planner sees chunks rather than cells.
	small := planOf(t, planChunks(400, 40), 400, 32)
	large := planOf(t, planChunks(180, 18), 180, 32)

	assert.Greater(t, len(small), len(large),
		"a denser-packed file is more frames, which is exactly the case that was slow")
}

// One worker at minimum, however few cores are reported.
func TestThereIsAlwaysAtLeastOneRenderWorker(t *testing.T) {
	assert.GreaterOrEqual(t, renderWorkers(), 1, "a single-core host still has to make progress")
}
