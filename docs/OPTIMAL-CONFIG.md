# Configuring for speed, and what 1 MB/s actually requires

Every number here is measured by a test in this repository, not estimated. Reproduce the lot with:

```bash
make bench
```

**The short answer.** 1 MB/s needs `color16` on a **256×256 grid at 8 px cells**, displayed at
**35 frames a second**, on a 4K panel, and a receiver with **at least nine cores free for decoding**.

That last requirement is the one that is easy to miss and impossible to configure around. Decoding costs
85–115 KB/s per core, measured, so the receiver's core count sets a hard ceiling that no display setting
improves. Frames are now decoded concurrently — one worker per core less one by default — so the cores are
used; what they cannot be is more numerous than they are.

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
  grid_width: 256
  grid_height: 256
  cell_pixels: 8          # 2080 px square — needs a 4K panel
  quiet_zone: 2
  fec:
    codec: reed-solomon
    data_shards: 32
    parity_shards: 8      # 25% — generous for a clean channel, cheap to reduce

display:
  sink: file              # or opengl on a machine with a panel
  fps: 35                 # see the ceiling above before raising this
  window_size: 64
```

Or as environment variables:

```bash
OTP_SENDER_OPTICAL_ENCODER=color16
OTP_SENDER_OPTICAL_COMPRESSION=zstd
OTP_SENDER_OPTICAL_LEVEL=9
OTP_SENDER_OPTICAL_GRID_WIDTH=256
OTP_SENDER_OPTICAL_GRID_HEIGHT=256
OTP_SENDER_OPTICAL_CELL_PIXELS=8
OTP_SENDER_DISPLAY_FPS=35
```

The frame rate can also be changed at any moment from **Settings** in the sender UI, including
mid-transfer, which is when you want it. The geometry cannot: it is written into every frame header and
the chunk size is derived from it, so the UI refuses a geometry change while anything is in flight.

### Receiver

```bash
OTP_RECEIVER_CAPTURE_SOURCE=gocv          # a camera; "file" reads a directory instead
OTP_RECEIVER_CAPTURE_DECODE_WORKERS=0     # 0 = one per core less one, which is what you want
OTP_RECEIVER_DECODER_CELL_PIXELS_HINT=8
OTP_RECEIVER_DECODER_MIN_FINDER_SCORE=0.75
OTP_RECEIVER_CALLBACK_ALLOWED_HOSTS=intake.example.com
```

Choose the camera from **Settings** in the receiver UI. With nothing configured it takes the
lowest-numbered device that actually declares video capture, in its largest mode.

### What the hardware has to be

| | Requirement | Why |
|---|---|---|
| Panel | 4K, ≥ 2 160 px vertical | The frame is 2 080 px square |
| Panel refresh | ≥ 40 Hz | 35 fps with margin; 144 Hz is ample |
| Camera resolution | ≥ 2× the frame's pixels on the sensor | A cell must land on several sensor pixels to be sampled reliably |
| Camera frame rate | ≥ 2× the display rate | Frames must not be missed between exposures; the sender re-shows an unacknowledged frame, so this costs throughput rather than correctness |
| Camera format | MJPG rather than raw | The same size is often 30 fps compressed and 5 fps uncompressed — the bus cannot carry raw frames faster |
| Receiver CPU | ~9 cores for 1 MB/s | 115 KB/s per core, measured; frames decode concurrently, so cores are what set the rate |
| Optics | Blur under a fifth of a cell | 8 px cells allow about 1.6 px of blur; 4 px cells allow 0.8 px |

## Why not just turn everything up

Three settings look free and are not.

**Frame rate above the receiver's decode speed** produces duplicate captures, not throughput. Each one
costs a write and a decode attempt, so the measured effect of overshooting is negative.

**Cell size below 8 px** halves the blur budget. `256×256 @4px` renders 1 040 px and carries exactly the
same bytes as `@8px` — the only thing a smaller cell saves is screen area, and the only thing it costs is
the margin the camera needs. Use 4 px cells when frames travel over HTTP and no camera is involved;
use 8 px when there is a lens.

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
