// Package mediatype decides how a transferred file may be shown in a browser.
//
// It lives in shared, beside the protocol rather than inside either application, for one reason: it is a
// security decision, and two copies of a security decision drift. Both ends serve transferred files to a
// browser — the sender the original, the receiver the reassembled copy — and both must make exactly the
// same judgement about what is safe to render.
//
// The judgement is narrow on purpose. A file that crossed this channel came from outside: the sender's
// caller uploaded it, and the receiver got it from a camera pointed at a screen. Serving it back with a
// content type that makes a browser *execute* it would turn a file transfer into script running on the
// origin that also hosts the operator's controls.
package mediatype

import (
	"path/filepath"
	"strings"
)

// Kind is how a file may be presented.
type Kind string

const (
	// KindImage, KindAudio, KindVideo, and KindText can be shown in place.
	KindImage Kind = "image"
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
	KindText  Kind = "text"

	// KindDownload is everything else — an archive, a disk image, an executable. It is offered as a
	// download and never rendered.
	KindDownload Kind = "download"
)

// inlineTypes maps an extension to the content type it may be served as.
//
// An allowlist rather than a denylist, and mapped from the extension rather than from anything the file
// or its sender claimed. Two omissions are deliberate and worth naming, because both look like images and
// neither is safe:
//
//   - **SVG.** An SVG is an XML document that may contain <script>. Served inline from the receiver's own
//     origin it is stored cross-site scripting against the page an operator uses to run the receiver.
//     It downloads instead.
//   - **HTML, XML, and anything that can host script.** Same argument, less disguised.
//
// PDF is left out for the adjacent reason: browsers run a full document viewer for it, with its own
// history of escapes, and a transferred PDF has no need to be rendered in the receiver's origin.
var inlineTypes = map[string]struct {
	kind        Kind
	contentType string
}{
	// Raster images. The browser decodes these as pixels; there is no script surface.
	".png":  {KindImage, "image/png"},
	".jpg":  {KindImage, "image/jpeg"},
	".jpeg": {KindImage, "image/jpeg"},
	".gif":  {KindImage, "image/gif"},
	".webp": {KindImage, "image/webp"},
	".bmp":  {KindImage, "image/bmp"},
	".avif": {KindImage, "image/avif"},
	".ico":  {KindImage, "image/vnd.microsoft.icon"},

	// Audio and video, for the formats a browser plays natively. A container it cannot play is better
	// downloaded than rendered as a broken player.
	".wav":  {KindAudio, "audio/wav"},
	".mp3":  {KindAudio, "audio/mpeg"},
	".m4a":  {KindAudio, "audio/mp4"},
	".aac":  {KindAudio, "audio/aac"},
	".flac": {KindAudio, "audio/flac"},
	".oga":  {KindAudio, "audio/ogg"},
	".opus": {KindAudio, "audio/ogg"},

	".mp4":  {KindVideo, "video/mp4"},
	".m4v":  {KindVideo, "video/mp4"},
	".webm": {KindVideo, "video/webm"},
	".ogv":  {KindVideo, "video/ogg"},
	".mov":  {KindVideo, "video/quicktime"},

	// Text, always as text/plain whatever it actually is. A JSON or Markdown file served as itself is
	// harmless; served as text/plain it is harmless *and* cannot be reinterpreted by a browser that
	// decides it knows better.
	".txt":  {KindText, "text/plain; charset=utf-8"},
	".log":  {KindText, "text/plain; charset=utf-8"},
	".md":   {KindText, "text/plain; charset=utf-8"},
	".csv":  {KindText, "text/plain; charset=utf-8"},
	".tsv":  {KindText, "text/plain; charset=utf-8"},
	".json": {KindText, "text/plain; charset=utf-8"},
	".yaml": {KindText, "text/plain; charset=utf-8"},
	".yml":  {KindText, "text/plain; charset=utf-8"},
	".toml": {KindText, "text/plain; charset=utf-8"},
	".ini":  {KindText, "text/plain; charset=utf-8"},
}

// downloadType is what anything not on the allowlist is served as: an opaque byte stream, which no browser
// will try to interpret.
const downloadType = "application/octet-stream"

// Of returns how a filename may be presented, and the content type to serve it with.
//
// The second return is the header value, and it is always safe to use: a file that may not be rendered
// gets application/octet-stream, so a caller that forgets to check the kind still cannot serve script.
func Of(filename string) (Kind, string) {
	base := filepath.Base(filename)
	extension := filepath.Ext(base)

	// A name that is nothing but an extension — ".png" — is a dotfile with no extension at all, whatever
	// filepath.Ext says about it. Requiring a basename keeps the rule honest: the extension is a suffix of
	// a name, not the whole of one.
	if strings.TrimSuffix(base, extension) == "" {
		return KindDownload, downloadType
	}

	if entry, ok := inlineTypes[strings.ToLower(extension)]; ok {
		return entry.kind, entry.contentType
	}
	return KindDownload, downloadType
}

// Inline reports whether a file may be shown in place rather than downloaded.
func Inline(filename string) bool {
	kind, _ := Of(filename)
	return kind != KindDownload
}

// Archive reports whether a filename looks like a container of other files.
//
// It is not a security decision — an archive is served as an opaque download either way — but it is worth
// knowing for what an interface says. "Archive, 48 MB, download" is a more useful sentence than "no
// preview available", and it is the case an operator moving a large backup across the gap will hit every
// time.
func Archive(filename string) bool {
	name := strings.ToLower(filename)
	for _, suffix := range []string{
		".zip", ".tar", ".tgz", ".tar.gz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz", ".tar.zst",
		".7z", ".rar", ".gz", ".bz2", ".xz", ".zst", ".iso", ".dmg",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
