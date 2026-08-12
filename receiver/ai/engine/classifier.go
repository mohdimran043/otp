package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/opticaltransport/otp/receiver/ai/classify"
	"github.com/opticaltransport/otp/shared/cellpatch"
	"github.com/opticaltransport/otp/shared/encoding"
)

// Classifier reads a frame's cells with a learned model instead of by nearest-neighbour palette matching.
//
// This is the machine-learning engine, and it is aimed at the failure the measurements actually show. On
// real captures, failures concentrate in payload_crc at a finder score near 1.000: the frame was found, the
// geometry is right, both fixed bands read, and individual cell decisions were wrong. That is a per-cell
// classification problem, so a per-cell classifier is the model that addresses it.
//
// What it does differently from the deterministic engine is not "try harder" — it is to use information the
// decoder never looks at. The decoder averages a window at a cell's centre and matches the result against
// eight palette entries. At four pixels a cell the neighbours bleed into that centre, so the colour there is
// a mixture whose composition depends on what surrounds the cell, and no distance metric on a single number
// can represent that. The model sees a patch spanning one and a half cells and can.
//
// The geometry stays in Go. Patches are sampled here, through the decoder's own homography and lens model,
// and only features cross the network. Shipping the image so the service could redo geometry would be
// slower and worse: the correct homography already exists on this side.
//
// Verification is unchanged and non-negotiable. Whatever the model proposes is unpacked to a payload and
// checked against the footer's CRC32 and SHA-256, so a returned frame is confirmed rather than believed. A
// confident model that is wrong produces a refusal, not corrupt data.
type Classifier struct {
	url    string
	client *http.Client

	version string

	// maxCells and maxCandidates bound the search over the model's own uncertainty, the same way the
	// deterministic engine bounds its search over palette margins.
	maxCells      int
	maxCandidates int
}

// ClassifierOptions configures the client.
type ClassifierOptions struct {
	URL           string
	Timeout       time.Duration
	MaxCells      int
	MaxCandidates int
}

// NewClassifier probes the service and returns an engine bound to it.
func NewClassifier(ctx context.Context, opts ClassifierOptions) (*Classifier, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("engine: a classifier needs a URL")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	c := &Classifier{
		url:           opts.URL,
		client:        &http.Client{Timeout: timeout},
		version:       "unknown",
		maxCells:      orDefault(opts.MaxCells, 12),
		maxCandidates: orDefault(opts.MaxCandidates, 4096),
	}

	v, recordBytes, err := c.health(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: the classifier at %s did not answer: %w", opts.URL, err)
	}
	// The record layout is a contract between two languages and two repositories' worth of assumptions.
	// Checking it at startup turns a silent misinterpretation of every patch — which would look like a
	// model that simply performs badly — into a refusal to start with the reason stated.
	if recordBytes != cellpatch.RecordBytes {
		return nil, fmt.Errorf("engine: the classifier expects %d-byte records, this build samples %d",
			recordBytes, cellpatch.RecordBytes)
	}
	if v != "" {
		c.version = v
	}
	return c, nil
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func (c *Classifier) Name() string    { return "classifier" }
func (c *Classifier) Version() string { return c.version }

func (c *Classifier) health(ctx context.Context) (version string, recordBytes int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/v1/health", nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("health returned %s", resp.Status)
	}

	var body struct {
		ModelVersion string `json:"model_version"`
		RecordBytes  int    `json:"record_bytes"`
	}
	if err := decodeJSON(resp.Body, &body); err != nil {
		return "", 0, err
	}
	return body.ModelVersion, body.RecordBytes, nil
}

// Recover classifies every payload cell and searches over the model's least confident decisions.
func (c *Classifier) Recover(ctx context.Context, req Request) (*Result, error) {
	if req.Geometry == nil || !classify.Recoverable(req.Bucket) {
		return nil, ErrNotRecovered
	}

	// The classifier assumes payload cells are visited in row-major order, which is what
	// cellpatch.Sample produces and what every encoding except rolling uses. Rolling interleaves across
	// bands, so its cells would be labelled in the wrong order — a mismatch that would corrupt frames
	// while appearing to correct them, so it is refused outright rather than approximated.
	if req.Geometry.Header.EncoderID == encoding.IDRolling {
		return nil, ErrNotRecovered
	}

	started := time.Now()

	// SoftRead first, for the footer. It is the oracle every proposal is checked against, and a frame
	// whose footer cannot be read is unrecoverable however good the model is — better to learn that here
	// than after a GPU round trip.
	reading, err := encoding.SoftRead(req.Geometry, req.Image)
	if err != nil {
		return nil, ErrNotRecovered
	}

	records, err := cellpatch.Sample(req.Image, req.Geometry)
	if err != nil {
		return nil, ErrNotRecovered
	}
	if len(records) != len(reading.Symbols) {
		return nil, fmt.Errorf("engine: sampled %d cells for a %d-symbol payload region",
			len(records), len(reading.Symbols))
	}

	posteriors, version, err := c.classify(ctx, records)
	if err != nil {
		return nil, ErrNotRecovered
	}
	if version == "" {
		version = c.version
	}

	// The model's own reading, cell by cell.
	symbols := make([]uint32, len(records))
	type ranked struct {
		index  int
		second uint32
		doubt  float64
	}
	doubts := make([]ranked, 0, len(records))
	for i := range records {
		best, second, top, runner := top2(posteriors[i])
		symbols[i] = best
		doubts = append(doubts, ranked{index: i, second: second, doubt: 1 - (top - runner)})
	}

	if frame, err := reading.Verify(symbols); err == nil {
		return &Result{
			Frame: frame,
			Report: Report{
				Engine: c.Name(), Version: version, Stage: "classify",
				Considered: len(records), Candidates: 1, Elapsed: time.Since(started),
			},
		}, nil
	}

	// The model read the frame and it still does not verify, so some cells are wrong. Search over the
	// ones the model was least sure about, using its runner-up — the same structure as the deterministic
	// search, with the model's posterior gap standing in for the palette margin. A posterior is the
	// better uncertainty estimate of the two, because it is calibrated against the actual error
	// distribution rather than against distance in a colour space.
	sort.Slice(doubts, func(a, b int) bool { return doubts[a].doubt > doubts[b].doubt })
	k := c.maxCells
	if k > len(doubts) {
		k = len(doubts)
	}
	set := doubts[:k]

	type candidate struct {
		mask uint32
		cost float64
	}
	masks := make([]candidate, 0, (1<<uint(k))-1)
	for m := uint32(1); m < 1<<uint(k); m++ {
		var cost float64
		for b := 0; b < k; b++ {
			if m&(1<<uint(b)) != 0 {
				// Cheapest first means flipping the cells the model doubted most, so cost is the
				// confidence being overridden.
				cost += 1 - set[b].doubt
			}
		}
		masks = append(masks, candidate{mask: m, cost: cost})
	}
	sort.Slice(masks, func(a, b int) bool { return masks[a].cost < masks[b].cost })

	trial := make([]uint32, len(symbols))
	tried := 0
	for _, cand := range masks {
		if tried >= c.maxCandidates {
			break
		}
		tried++
		copy(trial, symbols)
		for b := 0; b < k; b++ {
			if cand.mask&(1<<uint(b)) != 0 {
				trial[set[b].index] = set[b].second
			}
		}
		if frame, err := reading.Verify(trial); err == nil {
			return &Result{
				Frame: frame,
				Report: Report{
					Engine: c.Name(), Version: version, Stage: "classify+search",
					Flips: popcount(cand.mask), Candidates: tried, Considered: k,
					Elapsed: time.Since(started),
				},
			}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, ErrNotRecovered
}

// classify posts the sampled records and reads back one distribution per cell.
func (c *Classifier) classify(ctx context.Context, records []cellpatch.Record) ([][8]float32, string, error) {
	var body bytes.Buffer
	body.Grow(len(records) * cellpatch.RecordBytes)
	for _, r := range records {
		body.Write(r.MarshalBinary())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/classify", &body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("classify returned %s", resp.Status)
	}

	want := len(records) * 8 * 4
	raw := make([]byte, want)
	if _, err := io.ReadFull(io.LimitReader(resp.Body, int64(want)), raw); err != nil {
		return nil, "", fmt.Errorf("classify returned short posteriors: %w", err)
	}

	out := make([][8]float32, len(records))
	for i := range out {
		for j := 0; j < 8; j++ {
			bits := binary.LittleEndian.Uint32(raw[(i*8+j)*4:])
			out[i][j] = math.Float32frombits(bits)
		}
	}
	return out, resp.Header.Get(headerModelVersion), nil
}

// top2 returns the two most likely symbols and their probabilities.
func top2(p [8]float32) (best, second uint32, top, runner float64) {
	top, runner = -1, -1
	for i, v := range p {
		f := float64(v)
		switch {
		case f > top:
			second, runner = best, top
			best, top = uint32(i), f
		case f > runner:
			second, runner = uint32(i), f
		}
	}
	return best, second, top, runner
}

func popcount(v uint32) int {
	n := 0
	for ; v != 0; v &= v - 1 {
		n++
	}
	return n
}

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, 1<<16)).Decode(v)
}
