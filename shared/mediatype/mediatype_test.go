package mediatype_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/mediatype"
)

func TestRasterImagesAndMediaAreShownInPlace(t *testing.T) {
	for name, want := range map[string]mediatype.Kind{
		"holiday.png":   mediatype.KindImage,
		"scan.JPEG":     mediatype.KindImage,
		"clip.mp4":      mediatype.KindVideo,
		"clip.WebM":     mediatype.KindVideo,
		"chime.wav":     mediatype.KindAudio,
		"podcast.mp3":   mediatype.KindAudio,
		"notes.md":      mediatype.KindText,
		"records.csv":   mediatype.KindText,
		"manifest.json": mediatype.KindText,
	} {
		kind, contentType := mediatype.Of(name)
		require.Equal(t, want, kind, "%s", name)
		require.NotEqual(t, "application/octet-stream", contentType, "%s", name)
		require.True(t, mediatype.Inline(name), "%s", name)
	}
}

// TestScriptableTypesAreNeverShownInPlace is the reason this package exists.
//
// A file that crossed this channel came from outside — a caller uploaded it, or a camera photographed it —
// and serving it back with a content type a browser executes would turn a file transfer into script running
// on the origin that also hosts the operator's controls. SVG is the trap: it looks like an image and is an
// XML document that may contain <script>.
func TestScriptableTypesAreNeverShownInPlace(t *testing.T) {
	for _, name := range []string{
		"logo.svg", "logo.SVG", "page.html", "page.htm", "data.xml", "feed.xhtml",
		"script.js", "styles.css", "doc.pdf", "app.wasm", "sheet.xsl",
	} {
		kind, contentType := mediatype.Of(name)
		require.Equal(t, mediatype.KindDownload, kind, "%s must not be rendered", name)
		require.Equal(t, "application/octet-stream", contentType, "%s", name)
		require.False(t, mediatype.Inline(name), "%s", name)
	}
}

// TestTextIsAlwaysServedAsPlainText keeps a browser from deciding it knows better about a .json or .md.
func TestTextIsAlwaysServedAsPlainText(t *testing.T) {
	for _, name := range []string{"a.json", "b.yaml", "c.md", "d.csv", "e.log"} {
		_, contentType := mediatype.Of(name)
		require.Equal(t, "text/plain; charset=utf-8", contentType, "%s", name)
	}
}

// TestUnknownAndAwkwardNamesFallBackToADownload covers the cases a caller can actually produce: no
// extension at all, a dotfile, a trailing dot, and a name that is only an extension.
func TestUnknownAndAwkwardNamesFallBackToADownload(t *testing.T) {
	for _, name := range []string{"", "backup", ".bashrc", "archive.", "file.unknown", ".png.exe"} {
		kind, contentType := mediatype.Of(name)
		require.Equal(t, mediatype.KindDownload, kind, "%q", name)
		require.Equal(t, "application/octet-stream", contentType, "%q", name)
	}

	// ".png" alone is an extensionless dotfile named ".png", not a PNG. Treating it as an image would mean
	// a file called ".png" is rendered, which is the kind of edge a name-based rule has to get right.
	kind, _ := mediatype.Of(".png")
	require.Equal(t, mediatype.KindDownload, kind)
}

func TestArchivesAreRecognisedForWhatAnInterfaceSays(t *testing.T) {
	for _, name := range []string{
		"backup.zip", "logs.tar.gz", "data.TGZ", "image.iso", "src.7z", "dump.tar.zst", "app.dmg",
	} {
		require.True(t, mediatype.Archive(name), "%s", name)
		require.False(t, mediatype.Inline(name), "%s must still be a download", name)
	}
	for _, name := range []string{"holiday.png", "notes.md", "clip.mp4", "report.pdf"} {
		require.False(t, mediatype.Archive(name), "%s", name)
	}
}
