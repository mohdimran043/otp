package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/pipeline"
)

// newUUID is a one-line convenience so the frame-building helpers below read as what they are
// building rather than how a transmission id is minted.
func newUUID(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.New()
}

// writeZipFile adds one stored (uncompressed) entry to a zip being built for a test upload.
func writeZipFile(t *testing.T, zw *zip.Writer, name string, body []byte) {
	t.Helper()
	w, err := zw.Create(name)
	require.NoError(t, err)
	_, err = w.Write(body)
	require.NoError(t, err)
}

// multipartWriter builds a single-field "file" upload body and returns the Content-Type header
// the request needs to carry alongside it.
func multipartWriter(t *testing.T, buf *bytes.Buffer, filename string, body []byte) string {
	t.Helper()
	mw := multipart.NewWriter(buf)
	part, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = io.Copy(part, bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return mw.FormDataContentType()
}

// multipartWriterNoFile builds a multipart body that carries no "file" field at all, for the
// case the handler must reject before it ever looks at bytes.
func multipartWriterNoFile(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	mw := multipart.NewWriter(buf)
	require.NoError(t, mw.WriteField("note", "no file here"))
	require.NoError(t, mw.Close())
	return mw.FormDataContentType()
}

// The import endpoint is the receiving half of the sender's frame archive — a zip of frame
// PNGs, or the single composite PNG a one-chunk transfer downloads as, replayed into the
// pipeline. These tests fake the pipeline (Ingest is covered end to end in the pipeline
// package's own tests) and check the handler's own job: pulling images out of an upload and
// handing each to Ingest, whatever shape the upload arrived in.

// fakeIngester records every image it was handed and, by default, always reports a clean
// decode — so the tests here can check what reached it without depending on the real decoder.
// It can also be told to hand back canned per-call results, to fail from some call onward (for
// the systemic-failure abort), or to run a hook on every call (for tests that need to trigger a
// side effect, such as cancelling the request's own context, partway through a batch).
// One call can now answer with several results, because one image can hold several frames — a
// photographed sheet printed four-up is four. `results` is still indexed by call, each entry the
// set that call answers with.
type fakeIngester struct {
	mu       sync.Mutex
	images   []image.Image
	raws     [][]byte
	results  [][]pipeline.IngestResult // consumed in call order; falls back to one Decoded:true past the end
	failAt   int                       // 1-based call number at and after which failWith is returned
	failWith error
	onCall   func(callNumber int)
}

func (f *fakeIngester) Ingest(_ context.Context, img image.Image, raw []byte) ([]pipeline.IngestResult, error) {
	f.mu.Lock()
	f.images = append(f.images, img)
	f.raws = append(f.raws, raw)
	n := len(f.images)
	results := []pipeline.IngestResult{{Decoded: true}}
	if n-1 < len(f.results) {
		results = f.results[n-1]
	}
	failAt, failWith, onCall := f.failAt, f.failWith, f.onCall
	f.mu.Unlock()

	if onCall != nil {
		onCall(n)
	}
	if failWith != nil && failAt > 0 && n >= failAt {
		return nil, failWith
	}
	return results, nil
}

func (f *fakeIngester) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.images)
}

// newImportServer builds a server with a fake Ingest and a real Probe, wired through
// pipeline.Decodable exactly as main.go wires it — real, because the composite-splitting cases
// need to actually tell a decodable half from an indecodable one.
func newImportServer(t *testing.T, fake *fakeIngester) http.Handler {
	t.Helper()
	cfg := config.Default()
	return New(Options{
		Config: config.NewWatcher("", cfg),
		Log:    zap.NewNop(),
		Ingest: fake.Ingest,
		Probe: func(img image.Image) bool {
			return pipeline.Decodable(img, cfg)
		},
	}).Routes()
}

// solidPNG renders a tiny, uniformly coloured PNG.
func solidPNG(t *testing.T, width, height int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// encodeJPEG writes an image the way a camera would, which is the format a photographed sheet
// arrives in. Quality 92 rather than the package default: a frame photographed for import is
// evidence, and the tests should not be the only place it is saved at a quality no operator
// would choose.
func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}))
	return buf.Bytes()
}

// realFramePair renders two real, independently decodable frames at the same layout — a
// manifest and a data frame, exactly as the sender would, and exactly as ingest_test.go builds
// them for the pipeline package's own round trip. Same layout means same pixel dimensions,
// which is what makes them stackable into one composite the way the sender's archive endpoint
// does.
func realFramePair(t *testing.T) (manifest, data image.Image) {
	t.Helper()
	layout, err := protocol.NewLayoutQuiet(128, 128, 4, 2)
	require.NoError(t, err)
	enc, err := encoding.ByName("color16")
	require.NoError(t, err)
	depth := enc.DefaultBitDepth()

	txID := newUUID(t)

	m := protocol.Manifest{
		Filename:       "hello.txt",
		OriginalSize:   5,
		CompressedSize: 5,
		ChunkCount:     1,
		ChunkSize:      5,
	}
	manifestFrame, err := protocol.NewManifestFrame(protocol.Header{TransmissionID: txID, TotalChunks: 1}, m)
	require.NoError(t, err)
	manifestImg, err := enc.Encode(manifestFrame, layout, depth)
	require.NoError(t, err)

	dataFrame := protocol.NewFrame(protocol.Header{
		TransmissionID: txID,
		ChunkNumber:    0,
		TotalChunks:    1,
		Flags:          protocol.FlagLastChunk | protocol.FlagEndOfStream,
	}, []byte("hello"))
	dataImg, err := enc.Encode(dataFrame, layout, depth)
	require.NoError(t, err)

	return manifestImg, dataImg
}

// stackVertically composites two equal-height images the way the sender's frame archive
// endpoint does for a one-chunk transfer: top above bottom, in one PNG.
func stackVertically(t *testing.T, top, bottom image.Image) []byte {
	t.Helper()
	tb, bb := top.Bounds(), bottom.Bounds()
	require.Equal(t, tb.Dy(), bb.Dy(), "the composite only exists because both halves are equal height")
	width := max(tb.Dx(), bb.Dx())
	composite := image.NewRGBA(image.Rect(0, 0, width, tb.Dy()+bb.Dy()))
	draw.Draw(composite, image.Rect(0, 0, tb.Dx(), tb.Dy()), top, tb.Min, draw.Src)
	draw.Draw(composite, image.Rect(0, tb.Dy(), bb.Dx(), tb.Dy()+bb.Dy()), bottom, bb.Min, draw.Src)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, composite))
	return buf.Bytes()
}

func postImportFile(t *testing.T, handler http.Handler, filename string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var form bytes.Buffer
	mw := multipartWriter(t, &form, filename, body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import", &form)
	req.Header.Set("Content-Type", mw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestImportZipOfFramesIngestsEachPNGAndSkipsTheRest covers the zip shape: every .png entry is
// handed to Ingest, and anything else is reported as skipped rather than failing the request —
// an operator's archive may reasonably carry a README beside the frames.
func TestImportZipOfFramesIngestsEachPNGAndSkipsTheRest(t *testing.T) {
	fake := &fakeIngester{}
	handler := newImportServer(t, fake)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	writeZipFile(t, zw, "frame-00000000-manifest.png", solidPNG(t, 4, 4, color.RGBA{R: 1, A: 255}))
	writeZipFile(t, zw, "frame-00000001.png", solidPNG(t, 4, 4, color.RGBA{R: 2, A: 255}))
	writeZipFile(t, zw, "README.txt", []byte("not a frame"))
	require.NoError(t, zw.Close())

	rec := postImportFile(t, handler, "archive.zip", zipBuf.Bytes())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 2, fake.count())

	var out struct {
		Ingested int `json:"ingested"`
		Skipped  int `json:"skipped"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 2, out.Ingested)
	require.Equal(t, 1, out.Skipped)
}

// TestImportAcceptsAPhotographOfAPrintedFrame is the printed-sheet path: an operator prints a
// transfer's frames, photographs a sheet, and uploads the picture. Every phone writes JPEG, so a
// PNG-only importer refuses the one upload this endpoint most needs to accept — and refuses it at
// the media-type check, before the frame is ever looked at, so the operator is told their perfectly
// good photograph is "neither a zip nor a PNG".
func TestImportAcceptsAPhotographOfAPrintedFrame(t *testing.T) {
	fake := &fakeIngester{}
	handler := newImportServer(t, fake)

	_, dataImg := realFramePair(t)

	rec := postImportFile(t, handler, "IMG_0431.JPG", encodeJPEG(t, dataImg))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, fake.count(), "a photographed frame must reach the pipeline")
}

// TestImportZipOfPhotographs covers the same thing in bulk: an operator who scanned or
// photographed a stack of sheets has a folder of JPEGs, and zipping them is the obvious way to
// import the lot. Mixed with PNGs in one archive, because a real archive will be.
func TestImportZipOfPhotographs(t *testing.T) {
	fake := &fakeIngester{}
	handler := newImportServer(t, fake)

	_, dataImg := realFramePair(t)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	writeZipFile(t, zw, "sheet-01.jpg", encodeJPEG(t, dataImg))
	writeZipFile(t, zw, "sheet-02.JPEG", encodeJPEG(t, dataImg))
	writeZipFile(t, zw, "sheet-03.png", solidPNG(t, 4, 4, color.RGBA{R: 3, A: 255}))
	writeZipFile(t, zw, "README.txt", []byte("not a frame"))
	require.NoError(t, zw.Close())

	rec := postImportFile(t, handler, "sheets.zip", zipBuf.Bytes())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 3, fake.count(), "both photographs and the PNG must be ingested")

	var out struct {
		Ingested int `json:"ingested"`
		Skipped  int `json:"skipped"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 3, out.Ingested)
	require.Equal(t, 1, out.Skipped, "the README is still not a frame")
}

// TestImportCompositePNGSplitsIntoBothHalves is the one-chunk-transfer shape: the sender's
// archive endpoint hands out one PNG with the manifest stacked above the data frame, and the
// import endpoint must feed the pipeline both frames, not the stacked image as one blob.
func TestImportCompositePNGSplitsIntoBothHalves(t *testing.T) {
	fake := &fakeIngester{}
	handler := newImportServer(t, fake)

	manifestImg, dataImg := realFramePair(t)
	composite := stackVertically(t, manifestImg, dataImg)

	rec := postImportFile(t, handler, "hello.txt-frames.png", composite)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 2, fake.count(), "both halves of a genuine composite must decode and be ingested separately")
}

// TestImportSingleFramePNGIsIngestedWhole covers the other PNG shape: an ordinary single frame,
// which must not be cut in half on the way in.
func TestImportSingleFramePNGIsIngestedWhole(t *testing.T) {
	fake := &fakeIngester{}
	handler := newImportServer(t, fake)

	_, dataImg := realFramePair(t)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, dataImg))

	rec := postImportFile(t, handler, "frame.png", buf.Bytes())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, fake.count(), "a lone frame must not be split")
}

// TestImportRejectsWhatIsNeitherAZipNorAPNG covers the upload shapes that must fail before
// anything is ingested: a plain text file, a corrupt zip, and a request with no file at all.
func TestImportRejectsWhatIsNeitherAZipNorAPNG(t *testing.T) {
	fake := &fakeIngester{}
	handler := newImportServer(t, fake)

	txtRec := postImportFile(t, handler, "notes.txt", []byte("just some text"))
	require.Equal(t, http.StatusUnsupportedMediaType, txtRec.Code)

	corruptZipRec := postImportFile(t, handler, "archive.zip", append([]byte("PK"), []byte("not actually a zip")...))
	require.Equal(t, http.StatusBadRequest, corruptZipRec.Code)

	var emptyForm bytes.Buffer
	mw := multipartWriterNoFile(t, &emptyForm)
	noFileReq := httptest.NewRequest(http.MethodPost, "/api/v1/import", &emptyForm)
	noFileReq.Header.Set("Content-Type", mw)
	noFileRec := httptest.NewRecorder()
	handler.ServeHTTP(noFileRec, noFileReq)
	require.Equal(t, http.StatusBadRequest, noFileRec.Code)

	require.Equal(t, 0, fake.count(), "nothing rejected before decoding should ever reach Ingest")
}

// TestImportWithNoIngestConfiguredIsConflict covers a receiver built without the import wiring
// at all, which must refuse cleanly rather than panic on a nil hook.
func TestImportWithNoIngestConfiguredIsConflict(t *testing.T) {
	handler := New(Options{
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
	}).Routes()

	rec := postImportFile(t, handler, "frame.png", solidPNG(t, 4, 4, color.RGBA{A: 255}))
	require.Equal(t, http.StatusConflict, rec.Code)
}

// TestSplitCompositeUnit exercises the splitter directly against an injected probe, so its
// branching does not depend on a real decoder: a stacked pair whose probe accepts both halves
// splits into two, and anything else — a lone frame, an odd height, a pair where only one half
// looks decodable — is returned whole.
func TestSplitCompositeUnit(t *testing.T) {
	markerAt := func(img image.Image) color.RGBA {
		r, g, b, a := img.At(img.Bounds().Min.X, img.Bounds().Min.Y).RGBA()
		return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
	}
	red := color.RGBA{R: 255, A: 255}
	blue := color.RGBA{B: 255, A: 255}
	green := color.RGBA{G: 255, A: 255}

	// The fake probe only accepts red or blue at the image's top-left pixel — a stand-in for
	// "this looks like a decodable frame" that does not need a real decoder to exercise the
	// splitter's own logic.
	probe := func(img image.Image) bool {
		c := markerAt(img)
		return c == red || c == blue
	}

	t.Run("a stacked pair of decodable halves splits in two", func(t *testing.T) {
		top := image.NewRGBA(image.Rect(0, 0, 4, 4))
		draw.Draw(top, top.Bounds(), &image.Uniform{C: red}, image.Point{}, draw.Src)
		bottom := image.NewRGBA(image.Rect(0, 0, 4, 4))
		draw.Draw(bottom, bottom.Bounds(), &image.Uniform{C: blue}, image.Point{}, draw.Src)
		composite := image.NewRGBA(image.Rect(0, 0, 4, 8))
		draw.Draw(composite, image.Rect(0, 0, 4, 4), top, image.Point{}, draw.Src)
		draw.Draw(composite, image.Rect(0, 4, 4, 8), bottom, image.Point{}, draw.Src)

		parts := splitComposite(composite, probe)
		require.Len(t, parts, 2)
		require.Equal(t, red, markerAt(parts[0]))
		require.Equal(t, blue, markerAt(parts[1]))
	})

	t.Run("a lone frame is returned whole", func(t *testing.T) {
		lone := image.NewRGBA(image.Rect(0, 0, 4, 8))
		draw.Draw(lone, lone.Bounds(), &image.Uniform{C: green}, image.Point{}, draw.Src)

		parts := splitComposite(lone, probe)
		require.Len(t, parts, 1)
		require.Equal(t, green, markerAt(parts[0]))
	})

	t.Run("a nil probe never splits", func(t *testing.T) {
		composite := image.NewRGBA(image.Rect(0, 0, 4, 8))
		parts := splitComposite(composite, nil)
		require.Len(t, parts, 1)
	})
}

// zipOfFrames builds a zip of n distinct, valid PNG entries, for tests that only care about
// entry count and shape rather than real frame content.
func zipOfFrames(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < n; i++ {
		writeZipFile(t, zw, fmt.Sprintf("frame-%05d.png", i), solidPNG(t, 4, 4, color.RGBA{R: uint8(i), A: 255}))
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// TestImportEntryCapRejectsTooManyEntries is the protection against a request that could tie up
// the single applier (and this handler's own WriteTimeout) for minutes: an archive carrying more
// than maxImportEntries frames is refused outright, before a single one is ingested.
func TestImportEntryCapRejectsTooManyEntries(t *testing.T) {
	fake := &fakeIngester{}
	handler := newImportServer(t, fake)

	// Entries need not be many bytes each to hit the cap — the archive-file plumbing in
	// zipOfFrames uses tiny solid PNGs, so maxImportEntries+1 of them is still a small upload.
	rec := postImportFile(t, handler, "huge.zip", zipOfFrames(t, maxImportEntries+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	require.Equal(t, 0, fake.count(), "the cap must be enforced before any entry is ingested")
}

// TestImportStopsProcessingWhenTheRequestContextEnds covers the client-gone case: if the
// request's own context ends partway through a zip, the handler must stop feeding the applier
// rather than finish an archive nobody is waiting on, and must say so in the response.
func TestImportStopsProcessingWhenTheRequestContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeIngester{onCall: func(n int) {
		if n == 2 {
			cancel()
		}
	}}
	handler := newImportServer(t, fake)

	var form bytes.Buffer
	contentType := multipartWriter(t, &form, "archive.zip", zipOfFrames(t, 5))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import", &form).WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 2, fake.count(), "processing must stop as soon as the request context ends, not finish the archive")

	var out struct {
		Truncated bool `json:"truncated"`
		Ingested  int  `json:"ingested"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.True(t, out.Truncated, "the response must say the import did not run to completion")
	require.Equal(t, 2, out.Ingested)
}

// TestImportAbortsWith503WhenThePipelineStopsRunning covers the systemic-failure case: once
// Ingest starts failing with pipeline.ErrNotRunning, every remaining entry would fail the same
// way for the same reason, so the handler must abort rather than answer 200 having "skipped" an
// archive that never had a real chance.
func TestImportAbortsWith503WhenThePipelineStopsRunning(t *testing.T) {
	fake := &fakeIngester{failAt: 2, failWith: pipeline.ErrNotRunning}
	handler := newImportServer(t, fake)

	rec := postImportFile(t, handler, "archive.zip", zipOfFrames(t, 5))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, 2, fake.count(), "must abort the moment the systemic failure appears, not grind through the rest")
}

// pngChunk appends one PNG chunk (length, type, data, CRC) to buf, the on-the-wire shape
// image/png expects.
func pngChunk(buf *bytes.Buffer, chunkType string, data []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	buf.Write(lenBuf[:])
	typeAndData := append([]byte(chunkType), data...)
	buf.Write(typeAndData)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(typeAndData))
	buf.Write(crcBuf[:])
}

// hugePNGHeader hand-writes just a PNG signature and an IHDR chunk declaring width×height —
// enough for png.DecodeConfig (which reads only the header) to report those dimensions,
// without a single byte of real pixel data. It stands in for a hostile upload: a file that is a
// few dozen bytes on the wire but claims to be, say, a 60000×60000 image, which is exactly the
// case checkImageDimensions exists to catch before anything tries to allocate that image.
func hugePNGHeader(width, height uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // color type: truecolor with alpha
	pngChunk(&buf, "IHDR", ihdr)
	return buf.Bytes()
}

// TestImportRejectsAnOversizedPNGWithoutDecodingIt covers both places import.go decodes a PNG —
// a zip entry and the bare-file composite path — against a header that declares far more pixels
// than maxDecodedPixels allows. Neither upload carries any real pixel data, so if the guard were
// missing or came after png.Decode, this test would hang or exhaust memory rather than fail
// cleanly; that it returns quickly with a 4xx (or a per-entry skip) is the proof the check runs
// before decoding, not after.
func TestImportRejectsAnOversizedPNGWithoutDecodingIt(t *testing.T) {
	huge := hugePNGHeader(60000, 60000) // 3.6 billion pixels, far past the 64-megapixel bound

	t.Run("a zip entry with a lying header is skipped, not ingested", func(t *testing.T) {
		fake := &fakeIngester{}
		handler := newImportServer(t, fake)

		var zipBuf bytes.Buffer
		zw := zip.NewWriter(&zipBuf)
		writeZipFile(t, zw, "frame-00000000.png", huge)
		require.NoError(t, zw.Close())

		rec := postImportFile(t, handler, "archive.zip", zipBuf.Bytes())
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, 0, fake.count(), "an oversized entry must never reach Ingest")

		var out struct {
			Skipped int `json:"skipped"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 1, out.Skipped)
	})

	t.Run("a bare file with a lying header is rejected outright", func(t *testing.T) {
		fake := &fakeIngester{}
		handler := newImportServer(t, fake)

		rec := postImportFile(t, handler, "huge.png", huge)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		require.Equal(t, 0, fake.count())
	})
}

// TestImportRefusesAConcurrentImportWith409 covers the serialization added because this
// endpoint is unauthenticated, reads up to maxImportBytes into memory per request, and feeds
// the same single-threaded applier every captured frame also depends on: a second import
// arriving while one is already running must be refused immediately, not queued or run
// alongside it.
func TestImportRefusesAConcurrentImportWith409(t *testing.T) {
	fake := &fakeIngester{}
	srv := New(Options{
		Config: config.NewWatcher("", config.Default()),
		Log:    zap.NewNop(),
		Ingest: fake.Ingest,
		Probe: func(img image.Image) bool {
			return pipeline.Decodable(img, config.Default())
		},
	})
	handler := srv.Routes()

	// Simulates a request already in flight by holding the same lock the handler takes,
	// rather than racing a real concurrent request — deterministic, and it exercises exactly
	// the guard under test rather than a timing window.
	require.True(t, srv.importMu.TryLock())
	defer srv.importMu.Unlock()

	rec := postImportFile(t, handler, "archive.zip", zipOfFrames(t, 1))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "already running")
	require.Equal(t, 0, fake.count(), "a refused import must never reach Ingest")
}

// TestImportSheetReportsARowPerFrame covers the printed-sheet response shape. One uploaded image
// holding four frames is four results, and the operator needs to see all four verdicts — a single
// row saying "decoded" for a sheet where three frames failed reads as success and is worse than no
// row at all. The rows are numbered off the file's name so they can be told apart.
func TestImportSheetReportsARowPerFrame(t *testing.T) {
	txID := uuid.New()
	fake := &fakeIngester{results: [][]pipeline.IngestResult{{
		{Decoded: true, IsManifest: true, TransmissionID: &txID},
		{Decoded: true, TransmissionID: &txID},
		{Error: "payload_crc"},
		{Decoded: true, TransmissionID: &txID},
	}}}
	handler := newImportServer(t, fake)

	rec := postImportFile(t, handler, "sheet-01.png", solidPNG(t, 8, 8, color.RGBA{R: 9, A: 255}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, fake.count(), "one image is one call into the pipeline")

	var out struct {
		Entries []struct {
			Name    string `json:"name"`
			Decoded bool   `json:"decoded"`
			Error   string `json:"error"`
		} `json:"entries"`
		Ingested      int      `json:"ingested"`
		Transmissions []string `json:"transmissions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Entries, 4, "a row per frame on the sheet")
	require.Equal(t, 4, out.Ingested)
	require.Equal(t, []string{txID.String()}, out.Transmissions)

	// Numbered, so four rows of one sheet are distinguishable.
	require.Equal(t, "sheet-01.png#0", out.Entries[0].Name)
	require.Equal(t, "sheet-01.png#3", out.Entries[3].Name)

	// And the frame that failed says so rather than being folded in with the ones that read.
	require.Equal(t, "payload_crc", out.Entries[2].Error)
	require.False(t, out.Entries[2].Decoded)
}

// TestImportOfALoneFrameKeepsItsPlainName is the common case, which must not pick up a "#0" suffix
// just because the sheet case needs numbering: an archive of one-frame entries should read as the
// filenames the operator recognises.
func TestImportOfALoneFrameKeepsItsPlainName(t *testing.T) {
	fake := &fakeIngester{}
	handler := newImportServer(t, fake)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	writeZipFile(t, zw, "frame-00000000.png", solidPNG(t, 4, 4, color.RGBA{R: 1, A: 255}))
	require.NoError(t, zw.Close())

	rec := postImportFile(t, handler, "archive.zip", zipBuf.Bytes())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Entries, 1)
	require.Equal(t, "frame-00000000.png", out.Entries[0].Name)
}

// TestImportResponseListsTouchedTransmissionIDs is the Task 9 contract: the UI navigates
// straight to each transmission an import touched, so the response has to carry the ids
// themselves, not merely how many there were.
func TestImportResponseListsTouchedTransmissionIDs(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	fake := &fakeIngester{results: [][]pipeline.IngestResult{
		{{Decoded: true, TransmissionID: &first}},
		{{Decoded: true, TransmissionID: &second}},
		{{Decoded: true, TransmissionID: &first}}, // a repeat, e.g. the manifest and a data frame
	}}
	handler := newImportServer(t, fake)

	rec := postImportFile(t, handler, "archive.zip", zipOfFrames(t, 3))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out struct {
		Transmissions []string `json:"transmissions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	expected := []string{first.String(), second.String()}
	sort.Strings(expected)
	require.Equal(t, expected, out.Transmissions, "distinct transmission ids, sorted, not a bare count")
}
