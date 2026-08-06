package optical

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFileSinkBoundsTheChannel is the fix for an unbounded directory.
//
// The display writes a frame per tick while a receiver reads a few a second, so the sender outruns the
// reader by two orders of magnitude and the directory grows for as long as the display runs. It went
// unnoticed because the display sequence used to restart at one per transmission, so a later transfer
// overwrote the previous one's leavings — accidental cleanup that stopped the moment the counter became
// global, which it had to for concurrent transfers not to collide.
func TestFileSinkBoundsTheChannel(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewFileSink(dir, false)
	require.NoError(t, err)

	// More frames than the backlog, without any reader consuming them.
	const frames = displayBacklog + 500
	live := NewLive(sink)
	for range frames {
		require.NoError(t, live.Show(context.Background(), Frame{PNG: []byte("a frame")}))
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.LessOrEqual(t, len(entries), displayBacklog,
		"the channel must be bounded: %d frames written left %d files", frames, len(entries))

	// And what survived is the newest, because a reader wants the screen as it is rather than its history.
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Contains(t, names, fmt.Sprintf("%012d.png", frames), "the last frame shown must still be there")
	require.NotContains(t, names, fmt.Sprintf("%012d.png", 1), "the first frame is long off the screen")
}

// TestFileSinkKeepsEverythingWhenAsked covers the replay case: an installation being tuned wants the
// frames left behind, which is what the flag is for.
func TestFileSinkKeepsEverythingWhenAsked(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewFileSink(dir, true)
	require.NoError(t, err)

	live := NewLive(sink)
	const frames = displayBacklog + 10
	for range frames {
		require.NoError(t, live.Show(context.Background(), Frame{PNG: []byte("a frame")}))
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, frames, "retain_frames means retain them")
}

// TestFileSinkToleratesAFrameAlreadyRemoved is the ordinary case rather than an edge: the receiver deletes
// what it reads, so by the time the sink prunes, the file is usually gone already.
func TestFileSinkToleratesAFrameAlreadyRemoved(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewFileSink(dir, false)
	require.NoError(t, err)
	live := NewLive(sink)

	for i := range displayBacklog + 5 {
		require.NoError(t, live.Show(context.Background(), Frame{PNG: []byte("a frame")}))
		// A reader consuming as it goes, exactly as the receiver's file source does.
		if i%2 == 0 {
			_ = os.Remove(filepath.Join(dir, fmt.Sprintf("%012d.png", i+1)))
		}
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.LessOrEqual(t, len(entries), displayBacklog)
}
