package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // a model server may answer in JPEG; decoding must not depend on which
	"image/png"
	"io"
	"net/http"
	"time"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Sidecar is an engine backed by an out-of-process model server.
//
// The contract is deliberately the narrowest thing that can help: same size in, same size out,
// geometry untouched. The service deblurs, denoises, undoes compression artefacts — whatever its
// weights have learned — and hands back an image the *existing* decoder then reads at the original
// scale.
//
// It does not rectify, and this is the load-bearing decision. Warping to a canonical raster before
// decoding resamples twice, and the second resample spends the very budget that decides colour
// accuracy: how many camera pixels are averaged per palette decision. Measured on this project, 12 px
// per cell decodes every frame and 5.9 never does. An intermediate raster can only subtract from that,
// so the sidecar is forbidden from touching geometry and the decoder keeps sampling original pixels
// through its own homography.
//
// After enhancement the frame is re-decoded, and if it still fails, the inner engine gets a turn at
// the enhanced image — an enhancement that fixes most cells but not all is exactly what a candidate
// search finishes off.
type Sidecar struct {
	url    string
	client *http.Client

	// inner is tried on the enhanced image after a plain re-decode fails. Never nil; use Null to
	// disable it.
	inner Engine

	// version is what /v1/health reported at construction, so every recovery is attributable to a
	// specific set of weights. "unknown" when the service did not say.
	version string
}

// SidecarOptions configures the client.
type SidecarOptions struct {
	// URL is the service's base address, such as http://localhost:9800.
	URL string

	// Timeout bounds one enhance call. A model server that has stalled must not stall the decode
	// path with it.
	Timeout time.Duration

	// Inner is the engine tried on the enhanced image. Nil means Null.
	Inner Engine
}

// NewSidecar returns a sidecar engine and the version its health endpoint reported.
//
// Probing at construction rather than lazily is deliberate: a receiver configured for a sidecar that
// is not there should say so at startup, in the spirit of reporting honestly what a binary can do,
// rather than discovering it one failed frame at a time with nothing in the log that names the cause.
func NewSidecar(ctx context.Context, opts SidecarOptions) (*Sidecar, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("engine: a sidecar needs a URL")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	inner := opts.Inner
	if inner == nil {
		inner = NewNull()
	}

	s := &Sidecar{
		url:     opts.URL,
		client:  &http.Client{Timeout: timeout},
		inner:   inner,
		version: "unknown",
	}
	if v, err := s.health(ctx); err != nil {
		return nil, fmt.Errorf("engine: the sidecar at %s did not answer: %w", opts.URL, err)
	} else if v != "" {
		s.version = v
	}
	return s, nil
}

func (s *Sidecar) Name() string    { return "sidecar" }
func (s *Sidecar) Version() string { return s.version }

// healthResponse is what GET /v1/health returns.
type healthResponse struct {
	ModelVersion string `json:"model_version"`
	Device       string `json:"device"`
}

func (s *Sidecar) health(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/v1/health", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("health returned %s", resp.Status)
	}
	var body healthResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return "", err
	}
	return body.ModelVersion, nil
}

// enhanceMeta is the metadata the service returns alongside the image, in the X-Otp-Model-Version and
// X-Otp-Stage headers. Kept in headers rather than a JSON envelope so the body stays a plain image and
// neither side has to base64 a megabyte.
const (
	headerModelVersion = "X-Otp-Model-Version"
	headerStage        = "X-Otp-Stage"
)

// Recover enhances the capture, re-decodes it, and falls through to the inner engine.
func (s *Sidecar) Recover(ctx context.Context, req Request) (*Result, error) {
	started := time.Now()

	// A badly clipped frame is refused before the network call. Clipping is unrecoverable — nothing
	// reconstructs a channel that saturated — so sending one to a GPU spends a round trip to learn
	// what the histogram already said.
	if req.Clipped > 0.5 {
		return nil, ErrNotRecovered
	}

	enhanced, version, err := s.enhance(ctx, req.Image)
	if err != nil {
		return nil, ErrNotRecovered
	}
	if version == "" {
		version = s.version
	}

	// Re-decode from scratch. The enhanced image is the same size and geometry, but the fiducials may
	// now be findable where they were not — which is the whole point for a frame that failed at
	// no_quad — so the geometry is searched again rather than reused.
	opts := protocol.LocateOptions{}
	if req.Geometry != nil {
		opts.ExpectedLayout = &req.Geometry.Layout
	}

	if frame, err := encoding.Decode(enhanced, opts); err == nil {
		return &Result{
			Frame: frame,
			Report: Report{
				Engine: s.Name(), Version: version, Stage: "enhance",
				Elapsed: time.Since(started),
			},
		}, nil
	}

	// Enhancement was not enough on its own. If it produced a geometry, let the inner engine finish
	// the job on the enhanced pixels.
	g, err := protocol.Locate(enhanced, opts)
	if err != nil {
		return nil, ErrNotRecovered
	}
	inner := req
	inner.Image = enhanced
	inner.Geometry = g
	inner.Bucket = classifyAfterEnhance()

	res, err := s.inner.Recover(ctx, inner)
	if err != nil {
		return nil, ErrNotRecovered
	}
	res.Report.Engine = s.Name() + "+" + res.Report.Engine
	res.Report.Version = version + "+" + res.Report.Version
	res.Report.Stage = "enhance+" + res.Report.Stage
	res.Report.Elapsed = time.Since(started)
	return res, nil
}

// classifyAfterEnhance reports what the inner engine should treat this as.
//
// Enhancement changed the pixels, so the original bucket may no longer describe the frame: a capture
// that failed at no_quad and now locates is, as far as the inner engine is concerned, a frame whose
// geometry is good and whose payload is unproven. Passing the stale bucket through would make the
// inner engine refuse the very frames enhancement just rescued.
func classifyAfterEnhance() classify.Bucket { return classify.BucketPayloadCRC }

// enhance posts the capture and returns the service's image and the model version it reported.
//
// PNG on the wire, not JPEG. The payload is read as colour against palette entries a fixed distance
// apart, and JPEG's chroma subsampling averages colour across neighbouring cells — which is precisely
// the information being measured. Spending bandwidth to avoid re-compressing what the model just
// cleaned up is the cheaper mistake.
func (s *Sidecar) enhance(ctx context.Context, img image.Image) (image.Image, string, error) {
	var body bytes.Buffer
	if err := png.Encode(&body, img); err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/v1/enhance", &body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "image/png")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("enhance returned %s", resp.Status)
	}

	// Bounded read. A model server is a separate process that can misbehave, and an unbounded decode
	// of whatever it sends is a way for it to exhaust the receiver's memory.
	const maxImageBytes = 64 << 20
	out, _, err := image.Decode(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil {
		return nil, "", err
	}

	// Same size in, same size out is a contract, so it is checked rather than trusted. A service that
	// returned a rescaled image would silently break every geometry the decoder then computed, and the
	// symptom would be frames that fail for no visible reason.
	if !out.Bounds().Eq(img.Bounds()) {
		return nil, "", fmt.Errorf("enhance returned %v, expected %v", out.Bounds(), img.Bounds())
	}
	return out, resp.Header.Get(headerModelVersion), nil
}
