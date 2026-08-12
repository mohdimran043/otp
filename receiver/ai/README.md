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


## The real corpus: recovery recovers nothing, and geometry explains everything

Measured 2026-08-12 against 180 stored captures from a real handheld session (`48cb6970`, 1210 frames,
1080x1920 JPEG from a phone camera) — 150 frames the receiver had recorded as `payload_crc` failures plus
30 it had decoded, replayed through the same `encoding.Decode` and the same engine the receiver runs:

```
frames 180   engine go (margin-ordered-subset/1)
decoded first pass  30 (16.7%)
recovered by engine 0 (+0.0 points -> 16.7%)

first-pass buckets:
  decoded                 30
  payload_crc            150

by geometry:
  layout           frames  decoded  recovered    finder marginal/frame
  128x128@1px         154     4(  3%)          0     0.998     1001 of 12096 (8.3%)
  80x80@8px            26    26(100%)          0     0.988       30 of 2340 (1.3%)

mean clipped fraction 0.006
decode 39.6s total, recovery 50.0s total
```

Three things follow, and the third is the one that matters.

**1. Real failures are in recovery's target bucket, and recovery still cannot fix them.** Every failure
was `payload_crc` at a finder score of 0.998 — geometry essentially perfect, both bands read, only the
payload wrong. That is exactly the bucket this layer was built for, and it recovered **0 of 150**. The
reason is the marginal-cell count: these frames carry around a thousand ambiguous cells each, and the
search corrects twelve. The best frame in the sample had 292. Even it was 24 times beyond reach.

**2. The search's operating window is empty here.** At 80x80 every frame decoded on the first pass with
about 30 marginal cells — already more than the twelve the search covers, but read correctly anyway. At
128x128 there were a thousand. So the corpus contains no frames in the band where a bounded search helps:
below it they decode unaided, above it they are out of range. That band may exist at some intermediate
geometry, but nothing in this corpus lands in it.

**3. Geometry explains the whole decode rate.** 100% at 80x80@8px against 3% at 128x128@1px, from the same
camera in the same session. A single pixel per cell on the sender's display cannot survive a camera at any
distance, and that is a configuration decision, not a processing problem. **This is worth 30x more than
any recovery layer, and it costs nothing to change.**

A caveat on what was measured. For real captures there is no ground truth payload, so the figures above
count *marginal* cells — cells read with a margin under a quarter of the palette separation — not cells
read *wrongly*. A marginal cell may still be correct, and at 80x80 all 30 of them were. What can be said
without ground truth is bounded but sufficient: the search exhaustively tried every correction over the
twelve least confident cells of each frame and none verified, so more than twelve cells were wrong, or
the wrong ones were not among the twelve least confident. With a thousand cells ambiguous, it is the
former.

### Reproducing it

```bash
OTP_CORPUS_DIR=/path/to/captures OTP_CORPUS_GRID=80 OTP_CORPUS_CELL=8 \
  go test ./ai/corpus/ -run TestCorpusReplay -v -count=1 -timeout 900s
```

Captures come from the receiver's own object store, which persists every frame before deciding whether it
could read it. Pull a session out with `mc cp` against the `otp-receiver` bucket under
`captures/<session>/`. Stored objects are keyed `.png` but hold whatever the camera posted — a browser
posts JPEG — so decode by content, not by extension.

## What that implies, stated plainly

Recovery is correct and it is cheap, and on the evidence now in hand it is **not earning its place on
this channel**. It is proven to work when a frame has a few wrong cells — byte-exact at every grid from
80 to 1024 — and measured against 150 real failures it recovered none, because real failures carry
hundreds to a thousand ambiguous cells rather than a few.

That is a statement about this corpus, not a proof about all cameras. But it is the corpus this
deployment produces, and it agrees with the synthetic sweep and with the earlier channel measurements:
at 5.9 px/cell the mean distance to the nearest palette entry was 53 against a margin of 86, which is a
whole frame sitting on the decision boundary.

Consequences for what to build next:

- **A GPU enhancement sidecar is not the indicated next step.** It would attack `no_quad` — the
  largest bucket at high density — but `no_quad` at 1–2 px per cell is missing information, not
  missing contrast, and no restoration network adds samples that were never captured.
- **The indicated next step is in-frame error correction.** A 2.6% symbol error rate is squarely in
  reach of parity spent *within* the frame; the existing FEC operates across chunks, so a frame that
  misses its CRC is lost whole no matter how few or many cells were wrong. That is a protocol change
  and out of scope here, but it is where the measured evidence points.
- **The cheapest win available is a configuration change, and it is large.** 80x80@8px decoded 100% of
  its frames in the same session where 128x128@1px decoded 3%. Before any further engineering, stop
  offering one- and two-pixel cells for a camera channel, or bound them the way `COLOUR_GRID_CEILING`
  already bounds colour density for the Auto chooser.
- **The real corpus is now measured, and `ai/corpus` keeps it measurable.** Re-run it after any change
  that claims to improve recovery; a claim without a number from it is not a claim.

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
