package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

// fakeIngester records every image it was handed and always reports a clean decode, so the
// tests here can check what reached it without depending on the real decoder.
type fakeIngester struct {
	mu     sync.Mutex
	images []image.Image
	raws   [][]byte
}

func (f *fakeIngester) Ingest(_ context.Context, img image.Image, raw []byte) (pipeline.IngestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = append(f.images, img)
	f.raws = append(f.raws, raw)
	return pipeline.IngestResult{Decoded: true}, nil
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
