# Configuring for speed, and what 1 MB/s actually requires

Every number here is measured by a test in this repository, not estimated. Reproduce the lot with:

```bash
make bench
```

**The short answer — measured end to end, not calculated.** 1.459 MB/s, with 8 MB of incompressible
payload crossing the channel in 5.48 seconds, 120 of 120 chunks, zero retransmissions, verified against the
sender's hash and delivered to a callback that recomputed it independently.

The configuration that did it:

| Setting | Value |
|---|---|
| Grid | **384×384 cells at 5 px** — a 1 940 px frame, fits a 4K panel |
| Encoding | `color16` — 70 288 bytes a frame |
| Frame rate | **25 fps** — offering 1.676 MB/s |
| Receiver | 19 decode workers on 20 cores |

**Prefer fewer, larger frames over more, smaller ones.** That is the single most useful thing on this page.
The same hardware measured **730 KiB/s** at 256×256 and 40 fps, and **1 494 KiB/s** at 384×384 and 25 fps —
twice the throughput at *less* than two thirds the frame rate. The receiver's limit turns out to be frames
per second rather than bytes per second, because each frame costs a directory scan, a file read, a PNG
decode, a database row and an acknowledgement whatever its size. Making frames bigger amortises all of that.

---

## What sets the rate

```
transfer rate  =  payload per frame  ×  frames per second  ÷  compression ratio
```

Three independent terms, each with a different constraint behind it:

| Term | Set by | Limited by |
|---|---|---|
| Payload per frame | the grid and the encoding | the panel's pixels, and the camera's ability to resolve a cell |
| Frames per second | the display | the panel's refresh rate, and the receiver's decode speed |
| Compression ratio | the codec and the data | what the data actually is |

## Payload per frame, measured

From `shared/encoding`. **Capacity depends only on the grid, not the cell size** — 256×256 carries the
same bytes at 4 px cells as at 8 px. Cell size buys the camera's ability to read it, and costs screen
area.

**Both the grid and the cell size are chosen per transfer**, not once for a whole deployment. The New
Transfer form offers the presets below — grids to 1024×1024, cells of 1, 2, 3, 4, 6 or 8 px — and "Auto —
fit my screen" for either, computed in the browser from the panel it is open on. The values in `.env` are
the default for any transfer that does not choose. The tables in this document describe what a grid carries
and costs, whichever transfer picks it.

Because capacity depends on the grid alone, the two choices are close to independent: **the grid decides
how much a frame carries, the cell size decides whether the camera can read it.** Fixing one and leaving the
other on Auto sizes the other to the screen. Leaving both on Auto searches every combination that fits and
prefers the larger cell first, then the larger grid — the blur budget is what fails first, so a pixel of
cell is worth more than a row of grid.

**Encryption subtracts 28 bytes from every frame's payload capacity** — a 12-byte nonce and a 16-byte
authentication tag (`protocol.EncryptionOverhead`), because a chunk is sized to fit in exactly one frame
and the ciphertext still has to fit after the nonce and tag are added. An encrypted transfer's usable
capacity at a given grid and encoding is the figures below minus 28 bytes; the sender derives chunk size
from this automatically, so it is invisible operationally, but it is why an encrypted transfer at, say,
256×256 `color16` chunks at 30 198 bytes rather than 30 226.

| Grid | Frame at 4 px | Frame at 8 px | `color8` | `color16` |
|---|---|---|---|---|
| 128×128 | 528 px | 1 056 px | 4 536 B | 6 048 B |
| 192×192 | 784 px | 1 568 px | 12 183 B | 16 245 B |
| **256×256** | 1 040 px | **2 080 px** | 22 669 B | **30 226 B** |
| 384×384 | 1 552 px | 3 104 px | 52 716 B | 70 288 B |
| 512×512 | 2 064 px | 4 128 px | 94 860 B | 126 480 B |

A frame is `cell × (grid + 2 × quiet_zone)` pixels square, with the default two-cell quiet zone.

Frame rate needed for 1 MB/s (1 048 576 B/s), by geometry and encoding:

| Grid | `color8` | `color16` |
|---|---|---|
| 128×128 | 231 fps | 173 fps |
| 192×192 | 86 fps | 65 fps |
| **256×256** | 46 fps | **35 fps** |
| 384×384 | 20 fps | 15 fps |
| 512×512 | 11 fps | 8 fps |

**256×256 at 8 px is the sweet spot for a 4K panel.** It renders 2 080 px square, which fits inside
2 160 px of vertical space with room for mounting error, and 35 fps is a quarter of a 144 Hz panel's
capability. Going larger — 384×384 at 8 px is 3 104 px — needs more vertical pixels than 4K has.

## Compression, measured

Percentage of the original, at level 9, from `shared/compress`:

| Codec | Text | Structured | Sparse | Incompressible |
|---|---|---|---|---|
| `none` | 100% | 100% | 100% | 100% |
| `gzip` | 0.7% | 48.4% | 0.7% | 100.1% |
| `lz4` | 0.8% | 75.0% | 1.2% | 100.1% |
| **`zstd`** | **0.3%** | **37.9%** | **0.5%** | **100.0%** |
| `brotli` | 0.2% | 35.0% | 0.4% | 100.0% |

**Use `zstd` at level 9.** Brotli wins by three percentage points on structured data and costs several
times the CPU for it. On a transfer that is otherwise limited by decode speed, spending CPU to save 3%
of the bytes is the wrong trade. Keep `brotli` for archival transfers where wall-clock does not matter.

Note the last column. `gzip` and `lz4` make already-compressed data **larger** — 100.1% — so a transfer
of a zip, a JPEG, or an MP4 gains nothing from compression and loses a little. The rates in this document
are payload bytes and therefore what you get on incompressible input; anything that compresses moves
faster than the table says.

## Error correction: how much parity you actually get

`data_shards: 32` / `parity_shards: 8` reads like 25% overhead. It is a floor, not the figure, and the gap
comes from coding being **per block** rather than over the whole file:

```
blocks         = ⌈data chunks ÷ data_shards⌉
parity chunks  = blocks × parity_shards
```

Every block gets a full `parity_shards`, including the last one — which holds only the remainder. So the
overhead depends on how the chunk count divides:

| Data chunks | Blocks | Parity | Overhead | Why |
|---|---|---|---|---|
| 64 | 2 | 16 | 25.0% | divides exactly — the nominal figure |
| **66** | **3** | **24** | **36.4%** | a 2-shard tail block still gets 8 parity |
| 96 | 3 | 24 | 25.0% | exact again |
| 97 | 4 | 32 | 33.0% | a 1-shard tail block gets 8 parity — 800% on that block |

**This is worth understanding, not engineering around.** The tail block is where a transfer that gets cut
short loses its chunks, and a short final block is exactly the case where losing two would otherwise be
unrecoverable. But if you are sizing storage or a time budget, use the formula rather than the ratio: a file
landing just past a block boundary costs noticeably more than the nominal 25%.

Reducing `parity_shards` is the cheap lever on a clean channel — the shared-directory case loses nothing at
all, so parity there is pure cost. Raising `data_shards` widens the blocks and so reduces the tail penalty,
at the price of a codec that needs more of each block present to reconstruct anything.

## The ceiling nobody can configure around

From `shared/decode_rate_test.go`, one core, this hardware:

| Geometry | Pristine | Through typical optics | Frames/s/core | Throughput/core |
|---|---|---|---|---|
| 96×96 @6px | 21.8 ms | 22.3 ms | 44.9 | 86 KB/s |
| 128×128 @8px | 55.6 ms | 67.2 ms | 14.9 | 67 KB/s |
| **256×256 @8px** | 236.8 ms | 265.7 ms | **3.8** | **85 KB/s** (`color8`) |

Decoding costs roughly the same per byte at every geometry: **85 to 96 KB/s per core.** At `color16` on
256×256 that is about 115 KB/s per core.

**So 1 MB/s requires about nine cores decoding concurrently.** Frames are decoded concurrently — one
worker per core less one, or `OTP_RECEIVER_CAPTURE_DECODE_WORKERS` — which is what makes those cores
usable. It parallelises almost perfectly, because a frame is decoded from its own pixels and shares
nothing; only the work *after* the decode is serial, and that is milliseconds of database writes against
hundreds of milliseconds of decoding.

**The arithmetic that decides your frame rate:**

```
frames per second  =  decode workers × 115 000 ÷ bytes per frame
```

At 256×256 `color16` — 30 226 bytes a frame:

| Cores free for decoding | Sustainable rate | Frame rate to set |
|---|---|---|
| 1 | 115 KB/s | 4 fps |
| 4 | 460 KB/s | 15 fps |
| **9** | **1.0 MB/s** | **35 fps** |
| 19 | 2.2 MB/s | 72 fps |

**Measured against that table**, on 20 cores with 19 workers:

| Geometry | Frame | Offered | Measured | Frames/s |
|---|---|---|---|---|
| 256×256 @8px `color16` | 30 226 B | 1.153 MB/s at 40 fps | 730 KiB/s | 30 |
| **384×384 @5px `color16`** | **70 288 B** | 1.676 MB/s at 25 fps | **1 494 KiB/s** | 124 |

Both runs decoded every frame they captured — zero failures, zero retransmissions. The difference is
entirely per-frame overhead.

**One trap specific to testing.** The demonstration stack runs the receiver with
`OTP_RECEIVER_CAPTURE_SIMULATE=typical`, which models a lens and sensor over every pixel before decoding.
That is invaluable for proving the optical tolerances and ruinous for measuring throughput: at a 2 080 px
frame it costs seconds per frame and pins the CPU that should be decoding. The same transfer that runs at
730 KiB/s without it managed **121 KB/s with it**. A real camera does that degradation in the lens, for
free — so leave `SIMULATE` empty when measuring speed, and set it when testing whether the optics hold up.

**Setting the frame rate above that line does not transfer more bytes.** Frames queue on the channel
instead, and the display prunes its own backlog once it is deep enough — so the surplus costs a render and
a write and delivers nothing. The receiver reports the deepest queue it has seen as **Settings → Deepest
backlog**: one means it kept up, and a large number means the frame rate is above what this receiver can
use. Turning it down costs nothing.

---

## The configuration

### Sender

```yaml
optical:
  encoder: color16        # 4 bits per cell, the densest available
  compression: zstd
  level: 9
  grid_width: 384         # measured fastest: fewer, larger frames beat more, smaller ones
  grid_height: 384
  cell_pixels: 5          # 1940 px square — needs a 4K panel
  quiet_zone: 2
  fec:
    codec: reed-solomon
    data_shards: 32
    parity_shards: 8      # 25% — generous for a clean channel, cheap to reduce

display:
  sink: file              # or opengl on a machine with a panel
  fps: 25                 # 1.68 MB/s offered; measured 1.46 MB/s delivered
  window_size: 64
```

Or as environment variables:

```bash
OTP_SENDER_ENCODER=color16
OTP_SENDER_COMPRESSION=zstd
OTP_SENDER_COMPRESSION_LEVEL=9
OTP_SENDER_GRID_WIDTH=384
OTP_SENDER_GRID_HEIGHT=384
OTP_SENDER_CELL_PIXELS=5
OTP_SENDER_DISPLAY_FPS=25
```

There is no `OPTICAL_` in those names even though the YAML nests them under `optical:` — the environment
mapping is flat, and a variable that does not match is not an error, it is simply never read. A misspelt
name leaves the default in place silently, so check `/api/v1/config` rather than assuming it took.

The frame rate can also be changed at any moment from **Settings** in the sender UI, including mid-transfer,
which is when you want it. The geometry above is only the deployment's **default**: every new transfer starts
from it, and the New Transfer form can override both the grid and the cell size per transfer (128×128 up to
1024×1024, cells of 1–8 px, or "Auto — fit my screen" for either).

What nothing can do is change either one once a transfer exists. The grid is written into every frame header
and the chunk size is derived from it, so both are fixed at creation — which is also why the Settings page
refuses to change the deployment default while any transfer is in flight.

### Receiver

```bash
OTP_RECEIVER_CAPTURE_SOURCE=gocv          # a camera; "file" reads a directory instead
OTP_RECEIVER_CAPTURE_DECODE_WORKERS=0     # 0 = one per core less one, which is what you want
OTP_RECEIVER_DECODER_CELL_PIXELS_HINT=8
OTP_RECEIVER_DECODER_MIN_FINDER_SCORE=0.75
OTP_RECEIVER_CALLBACK_ALLOWED_HOSTS=intake.example.com
```

Choose the camera from the **Camera** tab in the receiver UI — its own page, because opening it is what asks
the browser for permission, and a browser will not label a device list until permission has been granted
once. With nothing configured it takes the lowest-numbered device that actually declares video capture, in
its largest mode.

### What the hardware has to be

Two columns, because the answer differs sharply between "1 MB/s on one channel" and "7 MB/s across
several". The camera is where they diverge most.

| | For ~1.5 MB/s, one channel | For 7 MB/s, four channels |
|---|---|---|
| Geometry | 384×384 at 5 px — a 1 940 px frame | 512×512 at 4 px — a 2 064 px frame |
| Panel | One 4K, ≥ 30 Hz | Four 4K, ≥ 60 Hz |
| **Camera** | **1080p is sufficient** — 388 cells across a 1 080 px sensor is 2.8 pixels a cell | **4K required** — 516 cells across 1 080 px is only 2.09 pixels a cell, which leaves no margin at all. 4K gives 4.19 |
| Camera frame rate | ≥ 2× the display rate | ≥ 60 fps, matched to the panel |
| Camera format | MJPG — the same size is often 30 fps compressed against 5 fps raw, because the bus cannot carry raw frames faster | MJPG, and check the sensor actually offers 4K at 60 rather than 4K at 30 |
| Shutter | Rolling is tolerable | Global preferred; a rolling shutter may catch a 60 Hz panel mid-refresh |
| Receiver CPU | ~19 cores | ~19 cores **per channel**, so four machines |
| Optics | Blur under a fifth of a cell — 5 px cells allow 1.0 px | 4 px cells allow **0.8 px**. This is the tightest constraint in the whole configuration |

**The cell-size squeeze is the thing to watch.** Going from 5 px cells to 4 px to fit a larger grid on the
same panel cuts the blur budget from 1.0 px to 0.8 px, at the same time as asking the camera to resolve
more cells. A marginal lens will show up here before it shows up anywhere else, and the symptom is a decode
rate that falls before any frame fails outright — which is why that figure is on the receiver's front page.

## Why not just turn everything up

Three settings look free and are not.

**Frame rate above the receiver's decode speed** produces duplicate captures, not throughput. Each one
costs a write and a decode attempt, so the measured effect of overshooting is negative.

**Cell size below 8 px** halves the blur budget. `256×256 @4px` renders 1 040 px and carries exactly the
same bytes as `@8px` — the only thing a smaller cell saves is screen area, and the only thing it costs is
the margin the camera needs. Use 4 px cells when frames travel over HTTP and no camera is involved;
use 8 px when there is a lens.

The 1-and-2-pixel cells and the 1024×1024 grid exist for that first case, and the honest statement of what
they are is worth being precise about. **`1024×1024 @1px` is byte-exact over a shared directory** — a
transfer round-trips identically, which is what makes it the right choice for the file channel and for the
frame-export path, where a "cell" is a pixel in a PNG nobody photographs. It is **not camera-proven**:
against the simulated optics it decodes 0 times out of 5, and that is the expected result rather than a
defect, because one pixel per cell leaves no blur budget at all to spend. For anything with a lens in it,
stay with the measured configurations above — 384×384 @5px, or 512×512 @4px if the camera is 4K. The UI
offers the small cells without steering you away from them, so the judgement is yours to make.

**`color16` over marginal optics.** Four bits per cell means sixteen levels to tell apart, and
`TestOpticalEnvelope` in `shared/encoding` maps where each encoding gives out: `color8` survives the
worst channel tested, `color16` needs a controlled installation. Over the HTTP transport there are no
optics at all and `color16` is free.

## Reading the rate while it runs

- **Sender → Settings** shows payload per frame and channel rate for the current geometry, computed from
  the encoder rather than estimated, and measures the panel's refresh rate in the browser.
- **Sender → Display** is the frames themselves, and the running total climbing while the picture holds
  still is the display repeating an unacknowledged frame.
- **Receiver → Live capture** shows the decode rate. That is the number to watch when raising the frame
  rate: it falls before frames start failing outright, which makes it the earliest warning that the
  display is ahead of the receiver.
- **Receiver → Settings** shows how many frames are being decoded at once and how many have been skipped.
  The skipped count is the surplus the display produced that nobody could use — the single most direct
  answer to "is my frame rate too high".

## Stopping a transfer

A transfer that is going wrong — the wrong file, or a frame rate that turns out to be far too high — can be
stopped from its own page rather than by restarting the sender. **Pause** keeps every acknowledgement, so
resuming shows only what is still outstanding; **Stop** ends it for good. Either takes effect within one
frame interval, because the display loop re-reads the transfer's status on every frame.

Neither reclaims storage. A stopped transfer still holds every chunk, shard and rendered frame it produced,
and on the geometry above that is roughly the compressed size again in frame PNGs. **Delete** is what
reclaims, and it refuses while a transfer is `preparing`, `transmitting` or `paused` — cancel first. An
hourly sweep also deletes anything older than `OTP_SENDER_RETENTION_MAX_AGE` (24h by default) that never
reached `completed`, which includes a transfer still transmitting; raise it past the longest transfer you
expect to run.

## Where the bytes are stored

Storage is not a throughput constraint at these rates, but it is a capacity one: a transfer costs its
compressed size in chunks plus every rendered frame as a PNG, and nothing removes either until a delete or
the retention sweep does.

Both sides take `filesystem` (a local directory) or `minio` (S3-compatible), chosen per side:

```bash
OTP_SENDER_STORAGE_BACKEND=filesystem     # or minio
OTP_RECEIVER_STORAGE_BACKEND=filesystem   # or minio
```

Filesystem is the faster of the two and the right default for a single-instance deployment — object storage
adds a network round trip per object where a directory adds a syscall. Reach for `minio` when a side runs as
more than one instance, which is the multi-channel case: four receivers decoding disjoint chunk ranges of one
file need somewhere all of them can write.

Each stack runs its **own** MinIO — `otp-sender` and `otp-receiver` buckets in separate volumes, never one
shared instance, because the two applications share a protocol and a directory and nothing else. Both compose
files create their bucket whether or not the backend is switched, so enabling it is one variable and a
restart. The backends are separate namespaces and nothing migrates between them: a side flipped after it has
run will not find what it wrote before.
