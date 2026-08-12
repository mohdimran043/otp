package engine_test

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/receiver/ai/engine"
	"github.com/opticaltransport/otp/receiver/ai/soft"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// The engine interface exists so that swapping an implementation cannot reach the decode path. These
// tests hold it to that: every implementation is exercised through the same interface, and the sidecar
// is driven against a real HTTP server so the wire contract is proven rather than assumed.

// cubeEdgeNeighbours lists, for each colour8 symbol, the symbols one channel away. A plant must move
// along a cube edge; across the interior a third corner intrudes and the runner-up is no longer the
// true symbol. See receiver/ai/soft for the worked example.
var cubeEdgeNeighbours = map[uint32][]uint32{
	0: {1, 2, 3}, 1: {0, 5, 6}, 2: {0, 4, 6}, 3: {0, 4, 5},
	4: {2, 3, 7}, 5: {1, 3, 7}, 6: {1, 2, 7}, 7: {4, 5, 6},
}

// plantedRequest renders a colour8 frame, breaks n payload cells, and returns the Request the pipeline
// would build for it plus the payload it must recover to.
func plantedRequest(t *testing.T, n int) (engine.Request, []byte) {
	t.Helper()

	l, err := protocol.NewLayout(80, 80, 8)
	require.NoError(t, err)

	capacity, err := encoding.Color8.EstimateCapacity(l, 3)
	require.NoError(t, err)
	payload := make([]byte, capacity.PayloadBytes)
	for i := range payload {
		payload[i] = byte(i * 13)
	}

	f := protocol.NewFrame(protocol.Header{TransmissionID: uuid.New()}, payload)
	img, err := encoding.Color8.Encode(f, l, 3)
	require.NoError(t, err)

	cells := l.PayloadCells()
	for i := 0; i < n; i++ {
		cell := cells[(i*len(cells)/n+37)%len(cells)]
		rect := l.CellRect(cell)
		mid := color.RGBAModel.Convert(img.At(rect.Min.X+rect.Dx()/2, rect.Min.Y+rect.Dy()/2)).(color.RGBA)
		neighbours := cubeEdgeNeighbours[encoding.Color8Palette.Value(mid)]
		toward := encoding.Color8Palette.Colors[neighbours[i%len(neighbours)]]
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

	g, err := protocol.Locate(img, protocol.LocateOptions{ExpectedLayout: &l})
	require.NoError(t, err)

	return engine.Request{
		Image:    img,
		Geometry: g,
		Bucket:   classify.BucketPayloadCRC,
	}, payload
}

func goEngine() engine.Engine {
	opts := soft.DefaultOptions()
	opts.Budget = 0
	return engine.NewGo(opts)
}

func TestGoEngineRecoversThroughTheInterface(t *testing.T) {
	req, payload := plantedRequest(t, 3)

	e := goEngine()
	require.Equal(t, "go", e.Name())
	require.NotEmpty(t, e.Version())

	res, err := e.Recover(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, payload, res.Frame.Payload)
	require.Equal(t, "go", res.Report.Engine)
	require.Equal(t, "soft", res.Report.Stage)
	require.Equal(t, 3, res.Report.Flips)
	require.Positive(t, res.Report.Elapsed)
}

// TestGoEngineRefusesWithoutGeometry is the guarantee the pipeline relies on to hand every failure to
// the engine unconditionally: an engine that works at a geometry must decline when there is none,
// rather than the caller having to know which buckets each engine can serve.
func TestGoEngineRefusesWithoutGeometry(t *testing.T) {
	req, _ := plantedRequest(t, 3)
	req.Geometry = nil
	req.Bucket = classify.BucketNoQuad

	_, err := goEngine().Recover(context.Background(), req)
	require.ErrorIs(t, err, engine.ErrNotRecovered)
}

// TestGoEngineRefusesAnUnrecoverableBucket covers the footer case specifically. The footer is the
// oracle a correction is checked against, so a frame without one cannot be searched toward anything.
func TestGoEngineRefusesAnUnrecoverableBucket(t *testing.T) {
	req, _ := plantedRequest(t, 3)
	req.Bucket = classify.BucketFooterCRC

	_, err := goEngine().Recover(context.Background(), req)
	require.ErrorIs(t, err, engine.ErrNotRecovered)
}

func TestNullEngineRecoversNothing(t *testing.T) {
	req, _ := plantedRequest(t, 3)
	e := engine.NewNull()
	require.Equal(t, "none", e.Name())

	_, err := e.Recover(context.Background(), req)
	require.ErrorIs(t, err, engine.ErrNotRecovered)
}

func TestChainStopsAtTheFirstEngineThatSucceeds(t *testing.T) {
	req, payload := plantedRequest(t, 2)

	// Null first, so the chain has to fall through it rather than stopping on the cheapest rung.
	chain := engine.NewChain(engine.NewNull(), goEngine())
	require.Equal(t, "none+go", chain.Name())
	require.Contains(t, chain.Version(), "go=")

	res, err := chain.Recover(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, payload, res.Frame.Payload)
	require.Equal(t, "go", res.Report.Engine, "the report names the engine that succeeded")
}

func TestChainRefusesWhenEveryEngineDoes(t *testing.T) {
	req, _ := plantedRequest(t, 3)
	_, err := engine.NewChain(engine.NewNull(), engine.NewNull()).Recover(context.Background(), req)
	require.ErrorIs(t, err, engine.ErrNotRecovered)
}

func TestEmptyChainIsUsableAndRefuses(t *testing.T) {
	req, _ := plantedRequest(t, 3)
	chain := engine.NewChain()
	require.Equal(t, "none", chain.Name())
	_, err := chain.Recover(context.Background(), req)
	require.ErrorIs(t, err, engine.ErrNotRecovered)
}

// sidecarServer stands in for a model server. enhance decides what it returns, so a test can model an
// identity pass, a service that repairs the frame, or one that misbehaves.
func sidecarServer(t *testing.T, version string, enhance func(image.Image) image.Image) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model_version":"` + version + `","device":"cpu"}`))
	})
	mux.HandleFunc("POST /v1/enhance", func(w http.ResponseWriter, r *http.Request) {
		in, err := png.Decode(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Otp-Model-Version", version)
		w.Header().Set("Content-Type", "image/png")
		_ = png.Encode(w, enhance(in))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// identity is the weightless service: it returns exactly what it was given.
//
// This is the shape the sidecar ships in before any model exists, and it is deliberately testable —
// the whole path is exercised end to end while the model's measured contribution is exactly zero,
// rather than an unfalsifiable claim.
func identity(img image.Image) image.Image { return img }

func TestSidecarReportsTheVersionItsHealthEndpointGave(t *testing.T) {
	srv := sidecarServer(t, "test-weights/v7", identity)

	s, err := engine.NewSidecar(context.Background(), engine.SidecarOptions{URL: srv.URL})
	require.NoError(t, err)
	require.Equal(t, "sidecar", s.Name())
	require.Equal(t, "test-weights/v7", s.Version())
}

func TestSidecarRefusesToOpenWhenTheServiceIsAbsent(t *testing.T) {
	// A port nothing is listening on. Opening must fail rather than yield an engine that silently
	// recovers nothing, which would read as a hard channel instead of a missing service.
	_, err := engine.NewSidecar(context.Background(), engine.SidecarOptions{
		URL:     "http://127.0.0.1:1",
		Timeout: 200 * time.Millisecond,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "did not answer")
}

func TestSidecarNeedsAURL(t *testing.T) {
	_, err := engine.NewSidecar(context.Background(), engine.SidecarOptions{})
	require.Error(t, err)
}

// TestSidecarFallsThroughToItsInnerEngine is the composition that matters: an identity service cannot
// repair anything, so the frame must be recovered by the inner search on the returned pixels, and the
// report must say both were involved.
func TestSidecarFallsThroughToItsInnerEngine(t *testing.T) {
	srv := sidecarServer(t, "identity", identity)
	req, payload := plantedRequest(t, 2)

	s, err := engine.NewSidecar(context.Background(), engine.SidecarOptions{
		URL:     srv.URL,
		Timeout: 10 * time.Second,
		Inner:   goEngine(),
	})
	require.NoError(t, err)

	res, err := s.Recover(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, payload, res.Frame.Payload)
	require.Equal(t, "sidecar+go", res.Report.Engine)
	require.Equal(t, "enhance+soft", res.Report.Stage)
	require.Contains(t, res.Report.Version, "identity")
}

// TestSidecarSucceedsOnEnhanceAloneWhenTheServiceRepairsTheFrame models a model server good enough that
// no search is needed: the repaired image decodes on its own, and the stage says "enhance".
func TestSidecarSucceedsOnEnhanceAloneWhenTheServiceRepairsTheFrame(t *testing.T) {
	req, payload := plantedRequest(t, 2)

	// The "model" here is the pristine render, which is the strongest enhancement possible. It stands
	// for a network that fully restored the cells, and proves the enhance-only path is reachable.
	pristine, _ := plantedRequest(t, 0)
	srv := sidecarServer(t, "oracle/v1", func(image.Image) image.Image { return pristine.Image })

	s, err := engine.NewSidecar(context.Background(), engine.SidecarOptions{
		URL:   srv.URL,
		Inner: engine.NewNull(),
	})
	require.NoError(t, err)

	res, err := s.Recover(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "enhance", res.Report.Stage,
		"a service that fully repaired the frame needs no search behind it")
	require.Equal(t, "sidecar", res.Report.Engine)
	require.Equal(t, 0, res.Report.Flips, "nothing was corrected by search; the image was already right")

	// Byte-exact, because the helper's payload is deterministic and the oracle returned the pristine
	// render of that same payload. Worth asserting rather than skipping: it proves the enhance-only
	// rung returns a frame that passed the footer, not merely that it returned something.
	require.Equal(t, payload, res.Frame.Payload)
}

// TestSidecarRejectsAResizedResponse holds the service to the contract. Returning a rescaled image
// would silently invalidate every geometry the decoder then computed, and the symptom would be frames
// failing for no visible reason.
func TestSidecarRejectsAResizedResponse(t *testing.T) {
	req, _ := plantedRequest(t, 2)
	srv := sidecarServer(t, "bad/v1", func(img image.Image) image.Image {
		b := img.Bounds()
		return image.NewRGBA(image.Rect(0, 0, b.Dx()/2, b.Dy()/2))
	})

	s, err := engine.NewSidecar(context.Background(), engine.SidecarOptions{URL: srv.URL, Inner: goEngine()})
	require.NoError(t, err)

	_, err = s.Recover(context.Background(), req)
	require.ErrorIs(t, err, engine.ErrNotRecovered)
}

// TestSidecarRefusesAClippedFrameWithoutCallingTheService records that the histogram decides before the
// network does. Nothing reconstructs a saturated channel, so spending a round trip on one is worse than
// declining — and on a GPU-backed deployment that round trip is the expensive part.
func TestSidecarRefusesAClippedFrameWithoutCallingTheService(t *testing.T) {
	var called bool
	srv := sidecarServer(t, "v1", func(img image.Image) image.Image {
		called = true
		return img
	})

	req, _ := plantedRequest(t, 2)
	req.Clipped = 0.9

	s, err := engine.NewSidecar(context.Background(), engine.SidecarOptions{URL: srv.URL, Inner: goEngine()})
	require.NoError(t, err)

	_, err = s.Recover(context.Background(), req)
	require.ErrorIs(t, err, engine.ErrNotRecovered)
	require.False(t, called, "a clipped frame must not reach the service")
}

func TestOpenBuildsWhatTheSettingsName(t *testing.T) {
	ctx := context.Background()

	off, err := engine.Open(ctx, engine.Settings{Enabled: false, Engine: "go"})
	require.NoError(t, err)
	require.Equal(t, "none", off.Name(), "disabled recovery is the Null engine, not a nil one")

	goOnly, err := engine.Open(ctx, engine.Settings{Enabled: true, Engine: "go", Search: soft.DefaultOptions()})
	require.NoError(t, err)
	require.Equal(t, "go", goOnly.Name())

	defaulted, err := engine.Open(ctx, engine.Settings{Enabled: true, Search: soft.DefaultOptions()})
	require.NoError(t, err)
	require.Equal(t, "go", defaulted.Name(), "an unnamed engine defaults to the deterministic search")

	_, err = engine.Open(ctx, engine.Settings{Enabled: true, Engine: "magic"})
	require.Error(t, err)
}

// TestOpenPutsTheCheapEngineFirst is the ordering that makes an expensive engine affordable: the
// deterministic search runs on every failure and the model server only on what it could not read.
func TestOpenPutsTheCheapEngineFirst(t *testing.T) {
	srv := sidecarServer(t, "v2", identity)

	e, err := engine.Open(context.Background(), engine.Settings{
		Enabled:        true,
		Engine:         "sidecar",
		SidecarURL:     srv.URL,
		SidecarTimeout: 5 * time.Second,
		Search:         soft.DefaultOptions(),
	})
	require.NoError(t, err)
	require.Equal(t, "go+sidecar", e.Name(), "the deterministic search must come first")
}

func TestOpenFailsRatherThanDowngradingWhenTheSidecarIsAbsent(t *testing.T) {
	_, err := engine.Open(context.Background(), engine.Settings{
		Enabled:        true,
		Engine:         "sidecar",
		SidecarURL:     "http://127.0.0.1:1",
		SidecarTimeout: 200 * time.Millisecond,
		Search:         soft.DefaultOptions(),
	})
	require.Error(t, err, "a named but unreachable sidecar must stop startup, not fall back silently")
}
