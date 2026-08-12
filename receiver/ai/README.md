# receiver/ai — frame recovery

A retry for frames the decoder rejects. Not a replacement for the decoder, and not an image enhancer.

Measured 2026-08-12 on the development machine (Linux 6.8, Go 1.26.1). Every number below came from a
run of the tests in this tree; none is an estimate.

## What this layer is

The decoder already does everything a generic computer-vision front end would: fiducial detection with
denoise and illumination-flattening fallbacks, an eight-way orientation search, a descriptor read with
nine scale trials, homography fitting, per-corner threshold calibration, a lens-distortion model, and a
per-frame photometric fit (a plane plus a radial term per RGB channel, fitted from the fiducials and
timing columns). See `shared/protocol/locate.go` and `shared/encoding/photometry.go`.

What it does *not* do is keep the confidence of its per-cell decisions. `Palette.Value` returns the
nearest entry and discards the distance to the runner-up. This layer retains that, ranks payload cells
by it, and retries the payload with the least confident cells corrected — verified against the footer's
CRC32 **and** SHA-256, so a candidate that passes is the frame rather than a guess.

```
capture → decodeFrame → verifies?  ─ yes ─→ done, recovery never called
                            │
                            no, and geometry locked
                            ↓
                     rank cells by margin, take k=12
                     try 2^k corrections in cost order
                     each checked against the footer
                            ↓
                     first that verifies is the frame
```

- Nothing in `shared/` changed behaviour. Three additive exports (`Palette.ValueWithMargin`,
  `encoding.SoftRead`, `(*SoftReading).Verify`); the golden vectors pass unchanged.
- Geometry is never touched. This layer cannot make a frame worse than it already was.
- It runs only on failures, so a healthy channel pays nothing.

## Failure buckets

`classify.Of` names the stage a frame died at. This matters more than the recovery counters, because
the buckets are what tell an operator what to *do*, and the two common cases call for opposite actions.

| Bucket | Meaning | Recoverable here |
|---|---|---|
| `decoded` | read cleanly | n/a |
| `no_quad` | fewer than four fiducials found | no — no geometry to search at |
| `degenerate_geometry` | fiducials collinear or coincident | no |
| `descriptor_crc` | fiducials found, grid dimensions unreadable | yes |
| `header_crc` | header band failed after majority voting | yes |
| `footer_crc` | footer band failed | **no** — the footer is the oracle a correction is checked against |
| `payload_crc` | geometry and both bands read, payload did not | **yes — the target** |
| `below_floors` | decoded but under the confidence floors | yes |
| `unsupported_version` | newer protocol | no |
| `other` | unclassified; a rising count means this table lags the decoder | no |

`classify.Clipped` reports the fraction of pixels saturated in at least one channel. Nothing recovers a
255, and clipping correlates monotonically with how early a frame fails, so a corpus dominated by
clipping is asking for exposure control rather than for software.

## It works, at every grid

`TestRecoverAcrossEveryGrid` plants three cells just past the decision boundary — moved along a cube
*edge*, which matters, see below — and requires byte-exact recovery. Every grid in the sender's
dropdown, cell size pinned per grid the way the sender's own chooser pins it:

| grid | px/cell | payload | flips | candidates | elapsed |
|---|---|---|---|---|---|
| 80 | 8 | 877 B | 3 | 7 | 2.2 ms |
| 96 | 8 | 1.9 KB | 3 | 7 | 4.1 ms |
| 128 | 8 | 4.5 KB | 3 | 7 | 7.1 ms |
| 192 | 4 | 12.2 KB | 3 | 7 | 9.6 ms |
| 256 | 4 | 22.7 KB | 3 | 7 | 17.5 ms |
| 384 | 2 | 52.7 KB | 3 | 7 | 20.8 ms |
| 512 | 2 | 94.9 KB | 3 | 7 | 31.2 ms |
| 1024 | 1 | 386 KB | 3 | 7 | 126 ms |

Seven candidates at every grid — the cost ordering finds a three-cell correction almost immediately,
because planted cells have margins near zero while everything else sits at the full palette separation.
The time scales with payload size, not with difficulty, because each candidate hashes the whole
payload.

Two things were required to hold across that range, and both fail quietly rather than loudly:

- **Selection is a bounded heap, not a sort.** Grid 1024 has about a million payload cells. Sorting
  them to read off twelve costs ~100 ms, several times the search it sets up.
- **The bound is wall-clock, not candidate count.** Exhausting 4096 candidates costs 49 ms at grid 80
  and would cost seconds at grid 1024. An operator raising the grid should not silently lengthen every
  failed decode by two orders of magnitude.

The budget covers the whole call, including the sampling pass — bounding only the loop measured 46 ms
against a 20 ms budget at grid 512. But it is not a strict ceiling: sampling is 118 ms of a 121 ms
recovery at grid 1024, and refusing after paying that would be the worst of both outcomes, so a floor
of 64 candidates always runs once the read is done. A 1 ms budget still recovers a single-cell error at
grid 512.

## The finding that matters: on synthetic degradation, recovery buys nothing

`TestSweepDecodeRate`, 20 frames per point, colour8 depth 3, cell size pinned per grid:

```
grid   px/cel profile      bytes   off    on  delta badcells  failed at
80     8      pristine       877    20    20     +0        -   -
80     8      clean          877    20    20     +0        -   -
80     8      typical        877    20    20     +0        -   -
80     8      harsh          877    20    20     +0        -   -
96     8      pristine      1903    20    20     +0        -   -
96     8      clean         1903    20    20     +0        -   -
96     8      typical       1903    20    20     +0        -   -
96     8      harsh         1903    20    20     +0        -   -
128    8      pristine      4536    20    20     +0        -   -
128    8      clean         4536    20    20     +0        -   -
128    8      typical       4536    20    20     +0        -   -
128    8      harsh         4536    20    20     +0        -   -
192    4      pristine     12183    20    20     +0        -   -
192    4      clean        12183    20    20     +0        -   -
192    4      typical      12183     0     0     +0      845   payload_crc x20
192    4      harsh        12183     0     0     +0        -   no_quad x20
256    4      pristine     22669    20    20     +0        -   -
256    4      clean        22669    20    20     +0        -   -
256    4      typical      22669     0     0     +0     1721   payload_crc x20
256    4      harsh        22669     0     0     +0        -   no_quad x20
384    2      pristine     52716    20    20     +0        -   -
384    2      clean        52716     0     0     +0        -   no_quad x20
384    2      typical      52716     0     0     +0        -   no_quad x20
384    2      harsh        52716     0     0     +0        -   no_quad x20
512    2      pristine     94860    20    20     +0        -   -
512    2      clean        94860     0     0     +0        -   no_quad x20
512    2      typical      94860     0     0     +0        -   no_quad x20
512    2      harsh        94860     0     0     +0        -   no_quad x20
1024   1      pristine    386316    20    20     +0        -   -
1024   1      clean       386316     0     0     +0        -   no_quad x20
1024   1      typical     386316     0     0     +0        -   no_quad x20
1024   1      harsh       386316     0     0     +0        -   no_quad x20
```

**Delta is +0 everywhere.** The reason is in the last two columns, and it is not that recovery was
never reached:

1. **At 8 px cells nothing ever fails.** Not even `harsh`. The sampling window is a quarter of a cell
   wide about the cell centre, blur does not reach it, and the photometric fit snaps levels back — the
   worst margin on a whole `harsh` frame is still the full palette separation of 86.
2. **At 2 px and below the fiducials go before the payload does.** Every failure is `no_quad`: there is
   no geometry, so nothing that operates at a geometry can help.
3. **At 4 px with `typical` there is a real `payload_crc` regime — and 845 wrong cells in it.** 1721 at
   grid 256. That is a symbol error rate of **2.6% and 2.8%**. The search corrects twelve.

So the regime this layer targets — a handful of ambiguous cells in an otherwise good frame — does not
occur under synthetic degradation at any locatable geometry. Degradation here is bimodal: zero errors,
or hundreds.


## The real corpus: recovery earns its place, and cell size dominates everything

Measured 2026-08-12 against five real browser-capture sessions, 35 frames each, sampled **uniformly**
(every Nth frame) so the decode rates are the sessions' own. Replayed through the same
`encoding.Decode` and the same engine the receiver runs.

```
session    frames   decoded  recovered  clipped  marginal geometry
17bd0315       35   31( 89%)          0    0.167      0.0% 128x128@8px (31/35)
43a3f881       35    7( 20%)          3    0.066      5.4% 80x80@8px   (17/35)
657df846       35    0(  0%)          0    0.024      0.0% none resolved
af0d182a       35    4( 11%)          7    0.000      2.5% 96x96@8px   (30/35)
f6ee85f3       35    1(  3%)          0    0.000      6.6% 128x128@1px (31/35)

per session, by geometry:
  17bd0315   128x128@8px   31 frames   31 decoded (100%)   0 recovered  finder 1.000  marginal   0 of 12096
  43a3f881   80x80@8px     17 frames    7 decoded ( 41%)   3 recovered  finder 1.000  marginal 119 of  2202
  af0d182a   96x96@8px     30 frames    4 decoded ( 13%)   7 recovered  finder 0.991  marginal 129 of  5076
  f6ee85f3   128x128@1px   31 frames    1 decoded (  3%)   0 recovered  finder 0.996  marginal 743 of 11316
  657df846   -             35 frames  no geometry resolved on any frame
```

**Recovery works on real captures.** On `af0d182a` it took 4 decoded frames to 11 — 11% to 31%, nearly
tripling the session's yield — and on `43a3f881` 7 to 10. Across all 175 frames, 43 decoded first pass
and 53 after recovery: **+5.7 percentage points**, entirely from frames that would otherwise have been
retransmitted or lost.

**The operating window is narrow and it is set by marginal-cell density.** At 2.5% marginal
(`af0d182a`) the search rescued 7 of 26 `payload_crc` frames. At 6.6% (`f6ee85f3`) it rescued 0 of 28.
Somewhere between those lies the edge, and the practical reading is that recovery pays while ambiguity
stays under roughly 3% of the payload and stops paying above about 5%.

**Cell size dominates, and grid size barely matters.** The best real result is `128x128@8px`: **31 of 31
frames decoded, zero marginal cells, 4536 payload bytes per frame.** That is five times the payload of
80x80@8px at a *better* decode rate. Against it, `128x128@1px` — the same grid, one pixel a cell —
managed 1 frame in 31. Eight pixels a cell is the whole difference; the grid is nearly free.

`657df846` (4569 frames in the full session, none decoded) resolved no geometry on any sampled frame:
35 of 35 `no_quad`. Nothing in this layer addresses that, and nothing should — a camera that never sees
four fiducials is not aimed at the screen.

### A correction to an earlier reading, and why it happened

An earlier run of this same harness reported recovery rescuing **0 of 150** real failures and concluded
the layer was not earning its place. That run sampled 150 `payload_crc` failures from one session — and
picked them from `128x128@1px`, the worst geometry in the corpus, where ambiguity runs at 8%. A
stratified sample from the worst configuration is not a measurement of the layer; it is a measurement of
that configuration. The uniform multi-session sample above is the honest one.

### A correction to the clipping metric

`classify.Clipped` originally counted pixels saturated in *any* channel, on the argument that a red cell
clipped in red has lost what distinguished it from white. That argument is wrong here, and the wrong
version looked more principled than the right one. Colour8 places every symbol at a corner of the RGB
cube, so seven symbols in eight saturate a channel **by design** — the metric reported 0.628 for the
capture that decoded all 31 of its frames. Anything thresholding on it would have misfired; the sidecar
refuses above 0.5, so it would have declined every colour frame ever captured. It now requires all three
channels, which reads 0.167 on that same session — close to the 0.125 expected from white being one
corner of eight.

### Reproducing it

```bash
# one session
OTP_CORPUS_DIR=/path/to/captures OTP_CORPUS_GRID=128 OTP_CORPUS_CELL=8 \
  go test ./ai/corpus/ -run TestCorpusReplay -v -count=1 -timeout 900s

# several, one subdirectory per session
OTP_CORPUS_SESSIONS=/path/to/sessions \
  go test ./ai/corpus/ -run TestCorpusSessions -v -count=1 -timeout 2400s
```

Captures come from the receiver's own object store, which persists every frame before deciding whether
it could read it. Pull a session with `mc cp` against the `otp-receiver` bucket under
`captures/<session>/`. Sample **uniformly**; a stratified sample gives a decode rate that is an artefact
of the stratification. Stored objects are keyed `.png` but hold whatever the camera posted — a browser
posts JPEG — so decode by content, not by extension.


## Machine learning: a cell classifier, measured against the deterministic engine

Added 2026-08-12. A small CNN reads each payload cell from a patch instead of matching one averaged
sample against eight palette entries.

**Why a classifier and not an enhancer.** Real failures concentrate in `payload_crc` at a finder score
near 1.000 — the frame was found, geometry is right, both fixed bands read, individual cell decisions
were wrong. That is per-cell classification. An enhancement network would attack `no_quad`, which at one
or two pixels a cell is missing *samples*, and no network invents samples never captured.

**What the model sees.** A 9x9 patch spanning 1.5 cells, sampled *through the decoder's homography* in
cell coordinates — so it is invariant to scale, rotation and perspective, and one model covers every
geometry. Plus this frame's measured black and white per channel, concatenated at the head rather than
applied to the input, so the model can learn its own mapping including the gamma a linear correction
cannot represent. The decoder's own centre sample is fed forward directly so the model starts from at
least the baseline's information.

The point of the patch is context. The decoder averages a window at the cell centre; at four pixels a
cell the neighbours bleed into it, so the colour there is a mixture whose composition depends on the
surroundings. A distance metric on one number cannot represent that. A patch can.

**Feature extraction lives in `shared/cellpatch`**, imported by both the sender's dataset exporter and the
receiver's inference path. Training features and production features are then the same code to the last
bilinear weight — two implementations would drift silently, and the symptom would be a model that scored
well in training and performed worse than the rule it replaced.

**Training.** 1,031,400 labelled cells from 156 frames across 8 geometries and 12 optical profiles.
Labels are proved, not assumed: taken from the pristine render and verified against its own footer, so a
mislabelled set is refused rather than trained on. Split by **frame**, never by cell — cells from one
frame share its exposure and blur, so a cell split scores the model on frames it has already seen.
26,552 parameters, 14 s on an RTX 4090.

```
baseline (decoder's rule) held-out accuracy  99.985%   (32 errors in 215,424 cells)
model held-out accuracy                     100.000%
cells fixed / broken                         32 / 0
```

Read that with care. The baseline is already at 99.985% on synthetic data, so this comparison proves only
that the model does not regress. **The synthetic set cannot demonstrate value** — the same bimodality the
decode sweep found means these profiles either leave cells clean or destroy the fiducials. The real
corpus is where the claim is settled.

### On real captures, the model roughly doubles what recovery achieves

175 real phone-camera frames, all three engines over identical frames in one run:

```
engine            decoded  recovered        total       rate   recover ms
go                     43         10           53      30.3%         1192
classifier             43         21           64      36.6%          804
go+classifier          43         24           67      38.3%         1124
```

Every recovery in every engine came from `payload_crc`, which is the bucket the design predicted.

- No recovery at all: **24.6%**
- Deterministic search: **30.3%** (+5.7 points)
- Learned classifier: **36.6%** (+12.0 points) — **2.1x the deterministic engine's recoveries**
- Both, cheapest first: **38.3%** (+13.7 points)

The chain beats either alone — 24 recoveries against 21 and 10 — so the two engines recover *different*
frames and running both is worth more than picking one. The model was trained purely on synthetic
degradation and generalises to real phone-camera JPEG, which is the result that justifies the approach.

Cost is real: about 0.8-1.2 s per recovered frame, dominated by sampling and classifying twelve thousand
cells per frame plus the round trip. That is affordable only because recovery runs on failures and off the
capture hot path.

### Running it

```bash
# 1. generate the training set (Go knows the truth)
OTP_DATASET_OUT=/tmp/ds OTP_DATASET_FRAMES=3 go test ./ai/dataset/ -run TestGenerate -v   # in sender/

# 2. train (RTX 4090, ~14s)
cd receiver/ai/service && python3 train.py --data /tmp/ds --out models/symbol-classifier/v1 --epochs 12

# 3. serve
python3 serve.py --weights models/symbol-classifier/v1 --port 9800

# 4. point the receiver at it
OTP_RECEIVER_DECODER_RECOVERY_ENGINE=classifier
OTP_RECEIVER_DECODER_RECOVERY_SIDECAR_URL=http://172.17.0.1:9800   # docker bridge gateway

# 5. compare engines on real captures
OTP_CORPUS_SESSIONS=/dir OTP_CLASSIFIER_URL=http://localhost:9800 \
  go test ./ai/corpus/ -run TestCorpusEngines -v -timeout 3600s
```

The service runs on the host rather than in a container because this machine's Docker has no NVIDIA
runtime configured. Nothing in the contract depends on that: it is an HTTP endpoint either way.

## What that implies, stated plainly

Recovery is correct, cheap, and on real captures it earns a measurable +5.7 points. It is also narrow:
it pays while payload ambiguity is under roughly 3% and stops paying above about 5%. Keep it on; it
costs nothing when frames decode.

But it is second-order next to the configuration finding:

- **Eight pixels a cell, and the grid is nearly free.** `128x128@8px` decoded 31 of 31 with zero marginal
  cells and 4536 bytes a frame. `128x128@1px` decoded 1 of 31. The sender still offers 1- and 2-pixel
  cells, and `COLOUR_GRID_CEILING = 96` bounds the wrong axis — it caps the grid for the Auto chooser
  when what needs bounding is cell size. Fixing that is worth far more than any further recovery work
  and costs nothing.
- **The learned classifier is justified; a GPU *enhancement* network still is not.** The classifier
  doubles recovery on real frames because it attacks per-cell reads, which is where real failures are.
  Enhancement would attack `no_quad`, which at one pixel a cell is missing samples rather than missing
  contrast, and no network adds samples that were never captured.
- **In-frame parity is still the way to widen the window.** Recovery corrects twelve cells; parity spent
  within the frame would address the 2-5% band wholesale, where today a frame missing its CRC is lost
  whole however few cells were wrong.
- **`ai/corpus` keeps this measurable.** Re-run it after any change claiming to improve recovery. A claim
  without a number from it is not a claim.

## Configuration

```
decoder:
  recovery:
    enabled: true          # OTP_RECEIVER_DECODER_RECOVERY_ENABLED
    max_cells: 12          # OTP_RECEIVER_DECODER_RECOVERY_MAX_CELLS      (search space is 2^n, capped at 20)
    max_candidates: 4096   # OTP_RECEIVER_DECODER_RECOVERY_MAX_CANDIDATES
    budget: 50ms           # OTP_RECEIVER_DECODER_RECOVERY_BUDGET         (0 disables the time bound)
```

All four are reloadable, like the confidence floors beside them: they are what an operator adjusts
while watching a marginal camera, and needing a restart to try a wider search defeats the point.

Reported on `GET /api/v1/session` under `recovery`, and shown on the camera page as recovered-of-attempted
plus the dominant failure stage and the action it implies. The object is omitted rather than zeroed when
nothing can report it — "nothing is asking" and "nothing was recoverable" must not look alike.

## A trap worth knowing

Correcting toward the **second-nearest** palette entry is sound because a small margin means the sample
sits near a Voronoi boundary, and the two cells sharing a boundary are by definition the two nearest.

This is easy to get wrong when building a test. Nudging white 55% of the way toward blue lands at
(115,115,255), whose nearest two entries are blue at 108 and *magenta* at 117, with the original white
only third at 132 — the line crosses the cube interior and passes nearer magenta than white. A correct
search returns magenta there, and is right to. Plants must move along a cube **edge**: black to red
passes through (127,0,0), nearest two exactly black and red at 69 each, closest other corner at 207.

## Running it

```bash
cd receiver && go test ./ai/... -count=1                      # unit tests, ~1s
cd receiver && go test ./ai/bench/ -run TestSweepDecodeRate -v -count=1 -timeout 3600s   # the sweep, ~145s
cd receiver && go test ./ai/soft/ -run TestRecoverAcrossEveryGrid -v -count=1            # per-grid costs
cd shared   && go test ./... -count=1                         # proves the core is unchanged
```

The sweep skips under `-short`.
