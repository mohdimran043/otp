package optical

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
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

// TestNoneSinkDiscardsFrames covers camera-only mode: the receiver watches the physical display with
// its own camera, so writing frames into a shared directory as well would be a second, pointless
// channel. The none sink accepts every frame — Live still needs a Show that succeeds, so it can go on
// publishing to Current/Next — but writes nothing anywhere.
func TestNoneSinkDiscardsFrames(t *testing.T) {
	sink := newNoneSink()
	require.Equal(t, "none", sink.Name())
	require.Equal(t, int64(0), sink.Shown())

	require.NoError(t, sink.Show(context.Background(), Frame{PNG: []byte("a frame")}))
	require.Equal(t, int64(1), sink.Shown(), "a discarded frame still counts as displayed")

	require.NoError(t, sink.Close())
}

// TestNoneSinkLeavesTheDisplayPageWorking is the point of the sink sitting under Live rather than
// replacing it: the browser's Display page and camera-view read Current/Next from Live, not from the
// sink, so a sink that writes nothing must still let a frame become "current".
func TestNoneSinkLeavesTheDisplayPageWorking(t *testing.T) {
	live := NewLive(newNoneSink())

	require.NoError(t, live.Show(context.Background(), Frame{PNG: []byte("a frame")}))

	frame, _, have := live.Current()
	require.True(t, have, "the display page must still see a frame with the none sink")
	require.Equal(t, []byte("a frame"), frame.PNG)
}

// TestOpenReturnsTheConfiguredSink covers the switch in open(): "none" must reach the discard sink,
// the same way "file" reaches the file sink.
func TestOpenReturnsTheConfiguredSink(t *testing.T) {
	sink, err := open(config.Display{Sink: "none"})
	require.NoError(t, err)
	require.Equal(t, "none", sink.Name())

	require.NoError(t, sink.Show(context.Background(), Frame{PNG: []byte("a frame")}))
	require.Equal(t, int64(1), sink.Shown())
}
