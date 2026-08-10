package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/objectstore"
	"github.com/opticaltransport/otp/sender/internal/store"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// The frame archive is the sneakernet half of this API: everything a receiver's import
// endpoint needs to replay a transfer without a camera in between. It needs a real database,
// because the 409 it must return depends on comparing the frame rows actually written against
// the count the transmission recorded for itself — a distinction an in-memory fake would have
// to reimplement rather than exercise.

// archiveHarness is a database, an object store, and a server, with nothing else running —
// the frame archive endpoint reads rows and stored bytes and touches no other subsystem.
type archiveHarness struct {
	store   *store.Store
	objects objectstore.Store
	handler http.Handler
}

func newArchiveHarness(t *testing.T) *archiveHarness {
	t.Helper()
	pool := testdb.New(t)

	cfg := config.Default()
	cfg.Storage.Root = t.TempDir()

	objects, err := objectstore.Open(context.Background(), cfg.Storage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, objects.Close()) })

	st := store.New(pool)
	handler := New(Options{
		Store:   st,
		Objects: objects,
		Config:  config.NewWatcher("", cfg),
		Log:     zap.NewNop(),
	}).Routes()

	return &archiveHarness{store: st, objects: objects, handler: handler}
}

// putFile records an uploaded file, the row every transmission points back to for its name.
func (h *archiveHarness) putFile(t *testing.T, name string, body []byte) store.File {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	key, err := objectstore.Key("files", id.String())
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, key, body))
	sum := sha256.Sum256(body)
	file, err := h.store.Files.Create(ctx, store.File{
		ID:         id,
		Filename:   name,
		StoredPath: key,
		SizeBytes:  int64(len(body)),
		SHA256:     sum[:],
	})
	require.NoError(t, err)
	return file
}

// putTransmission creates a transmission row for a file, with frameCount as the caller wants
// it — independent of how many frame rows actually get inserted, so the 409 path can be
// exercised.
func (h *archiveHarness) putTransmission(t *testing.T, fileID uuid.UUID, frameCount int) store.Transmission {
	t.Helper()
	ctx := context.Background()
	tx, err := h.store.Transmissions.Create(ctx, store.Transmission{FileID: fileID})
	require.NoError(t, err)
	require.NoError(t, h.store.Transmissions.SetFrameCount(ctx, tx.ID, frameCount))
	tx, err = h.store.Transmissions.Get(ctx, tx.ID)
	require.NoError(t, err)
	return tx
}

// putFrame stores a frame's pixels and writes its row.
func (h *archiveHarness) putFrame(t *testing.T, txID uuid.UUID, number int, manifest bool, pngBytes []byte) store.Frame {
	t.Helper()
	ctx := context.Background()
	key, err := objectstore.Key("frames", txID.String(), fmt.Sprintf("%d.png", number))
	require.NoError(t, err)
	require.NoError(t, objectstore.PutBytes(ctx, h.objects, key, pngBytes))

	decoded, err := png.Decode(bytes.NewReader(pngBytes))
	require.NoError(t, err)

	sum := sha256.Sum256(pngBytes)
	frame := store.Frame{
		TransmissionID: txID,
		FrameNumber:    number,
		IsManifest:     manifest,
		WidthPx:        decoded.Bounds().Dx(),
		HeightPx:       decoded.Bounds().Dy(),
		StoredPath:     key,
		PayloadBytes:   len(pngBytes),
		SHA256:         sum[:],
	}
	require.NoError(t, h.store.Frames.InsertMany(ctx, []store.Frame{frame}))
	return frame
}

// putFrameWithMissingObject writes a frame row whose stored path was never written to the
// object store — a row surviving without its bytes, the way a botched migration or a purge
// race might leave one — so a test can drive the "the read failed" branch without needing
// the object store itself to misbehave.
func (h *archiveHarness) putFrameWithMissingObject(t *testing.T, txID uuid.UUID, number int, manifest bool, width, height int) store.Frame {
	t.Helper()
	ctx := context.Background()
	key, err := objectstore.Key("frames", txID.String(), fmt.Sprintf("%d-never-written.png", number))
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(key))
	frame := store.Frame{
		TransmissionID: txID,
		FrameNumber:    number,
		IsManifest:     manifest,
		WidthPx:        width,
		HeightPx:       height,
		StoredPath:     key,
		SHA256:         sum[:],
	}
	require.NoError(t, h.store.Frames.InsertMany(ctx, []store.Frame{frame}))
	return frame
}

// solidPNG renders a tiny, uniformly-coloured PNG — enough to be a distinct, valid image
// without needing the real encoder.
func solidPNG(t *testing.T, width, height int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestFrameArchiveZipsAllFramesManifestOnce(t *testing.T) {
	h := newArchiveHarness(t)
	file := h.putFile(t, "report.pdf", []byte("the original file"))

	manifestPNG := solidPNG(t, 4, 4, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	chunk1 := solidPNG(t, 4, 4, color.RGBA{R: 1, A: 255})
	chunk2 := solidPNG(t, 4, 4, color.RGBA{R: 2, A: 255})
	chunk3 := solidPNG(t, 4, 4, color.RGBA{R: 3, A: 255})

	tx := h.putTransmission(t, file.ID, 5)
	h.putFrame(t, tx.ID, 0, true, manifestPNG)
	h.putFrame(t, tx.ID, 1, false, chunk1)
	h.putFrame(t, tx.ID, 2, false, chunk2)
	h.putFrame(t, tx.ID, 3, false, chunk3)
	// The re-emitted manifest: a mid-stream joiner's copy, byte-identical to frame 0's
	// payload, that a file only needs once.
	h.putFrame(t, tx.ID, 4, true, manifestPNG)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/transfers/"+tx.ID.String()+"/frames/archive", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "application/zip", response.Header().Get("Content-Type"))
	require.Contains(t, response.Header().Get("Content-Disposition"), `filename="report.pdf-frames.zip"`)

	reader, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	require.NoError(t, err)

	names := make([]string, len(reader.File))
	contents := make(map[string][]byte, len(reader.File))
	for i, f := range reader.File {
		names[i] = f.Name
		require.Equal(t, zip.Store, f.Method, "frame images are already compressed; re-deflating them wastes time for nothing")
		rc, err := f.Open()
		require.NoError(t, err)
		body, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		contents[f.Name] = body
	}

	require.Equal(t, []string{
		"frame-00000000-manifest.png",
		"frame-00000001.png",
		"frame-00000002.png",
		"frame-00000003.png",
	}, names, "the repeated manifest at frame 4 must be skipped")

	require.Equal(t, manifestPNG, contents["frame-00000000-manifest.png"])
	require.Equal(t, chunk1, contents["frame-00000001.png"])
	require.Equal(t, chunk2, contents["frame-00000002.png"])
	require.Equal(t, chunk3, contents["frame-00000003.png"])
}

func TestFrameArchiveSingleChunkIsOneCompositePNG(t *testing.T) {
	h := newArchiveHarness(t)
	file := h.putFile(t, "note.txt", []byte("x"))

	manifestPNG := solidPNG(t, 8, 6, color.RGBA{R: 200, A: 255})
	dataPNG := solidPNG(t, 8, 6, color.RGBA{B: 200, A: 255})

	tx := h.putTransmission(t, file.ID, 2)
	h.putFrame(t, tx.ID, 0, true, manifestPNG)
	h.putFrame(t, tx.ID, 1, false, dataPNG)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/transfers/"+tx.ID.String()+"/frames/archive", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "image/png", response.Header().Get("Content-Type"))
	require.Contains(t, response.Header().Get("Content-Disposition"), `filename="note.txt-frames.png"`)

	composite, err := png.Decode(bytes.NewReader(response.Body.Bytes()))
	require.NoError(t, err)
	require.Equal(t, 8, composite.Bounds().Dx(), "width matches the source frames")
	require.Equal(t, 12, composite.Bounds().Dy(), "height is the two frames stacked, equally")

	// The manifest band is on top, whatever order the rows happened to be selected in.
	require.Equal(t, color.RGBA{R: 200, A: 255}, rgbaAt(composite, 0, 0))
	require.Equal(t, color.RGBA{B: 200, A: 255}, rgbaAt(composite, 0, 6))
}

func rgbaAt(img image.Image, x, y int) color.RGBA {
	r, g, b, a := img.At(x, y).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}

func TestFrameArchiveWhileRenderingIs409(t *testing.T) {
	h := newArchiveHarness(t)
	file := h.putFile(t, "big.bin", []byte("data"))

	// The transmission expects five frames; rendering has only produced two so far.
	tx := h.putTransmission(t, file.ID, 5)
	h.putFrame(t, tx.ID, 0, true, solidPNG(t, 4, 4, color.RGBA{A: 255}))
	h.putFrame(t, tx.ID, 1, false, solidPNG(t, 4, 4, color.RGBA{A: 255}))

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/transfers/"+tx.ID.String()+"/frames/archive", nil))
	require.Equal(t, http.StatusConflict, response.Code)
}

// TestFrameArchiveUnknownTransferIs404 covers the id that simply does not exist — a typo, or
// a transfer that was purged — as distinct from one that exists but is not ready yet.
func TestFrameArchiveUnknownTransferIs404(t *testing.T) {
	h := newArchiveHarness(t)

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/transfers/"+uuid.New().String()+"/frames/archive", nil))
	require.Equal(t, http.StatusNotFound, response.Code)
}

// TestFrameArchiveFirstFrameReadFailureIsNot200 is the regression the review caught: reading
// the first selected frame happens before any header is written specifically so that a
// failure there can still answer with a real status. Before that fix, nothing was written
// until the loop reached a working entry, so a first-frame failure fell through to
// net/http's default 200 with an empty body — a "successful" response carrying an invalid
// zip.
func TestFrameArchiveFirstFrameReadFailureIsNot200(t *testing.T) {
	h := newArchiveHarness(t)
	file := h.putFile(t, "broken.bin", []byte("data"))

	tx := h.putTransmission(t, file.ID, 4)
	// Frame 0's object was never written to the store — everything the read needs except the
	// bytes themselves.
	h.putFrameWithMissingObject(t, tx.ID, 0, true, 4, 4)
	h.putFrame(t, tx.ID, 1, false, solidPNG(t, 4, 4, color.RGBA{A: 255}))
	h.putFrame(t, tx.ID, 2, false, solidPNG(t, 4, 4, color.RGBA{A: 255}))
	h.putFrame(t, tx.ID, 3, false, solidPNG(t, 4, 4, color.RGBA{A: 255}))

	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/transfers/"+tx.ID.String()+"/frames/archive", nil))
	require.NotEqual(t, http.StatusOK, response.Code,
		"a first-frame read failure must not look like a successful, empty-bodied zip")
	require.Equal(t, http.StatusInternalServerError, response.Code)
}
