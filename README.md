# Optical Transport Platform

Move large files across an air gap with a monitor and a camera. The sender renders a file as encoded
frames on a display; the receiver photographs them, decodes them, reassembles the file, and verifies
it against the hash the sender declared. Acknowledgements travel out of band through shared storage —
never optically — which is what makes the transfer lossless rather than hopeful.

**[See a transfer, end to end, with screenshots →](docs/DEMONSTRATION.md)**

---

## What it does

A caller posts one request to the sender: a file, and a URL the result should go to.

```bash
curl -X POST http://localhost:8080/api/v1/transfers \
    -F "file=@quarterly-report.tar" \
    -F "callback_url=https://intake.example.com/received"
```

Everything after that happens without them:

1. **Compress** — one of five codecs, streamed rather than buffered, because these files do not fit
   in memory.
2. **Chunk** — sized so that exactly one chunk fills exactly one frame, derived from the encoder's
   capacity at the configured grid rather than configured separately.
3. **Error-code** — Reed-Solomon, RaptorQ (RFC 6330), or a sparse-graph LDPC code, in blocks.
4. **Render** — an adaptive optical grid: four corner fiducials, a binary header band, a modulated
   payload, a binary footer carrying the payload's CRC32 and SHA-256.
5. **Display** — each frame shown until the receiver says its chunk arrived.
6. **Capture and decode** — grayscale, per-block Otsu binarisation, connected-component fiducial
   detection, four-point homography, per-cell median sampling, demodulation. No OpenCV.
7. **Acknowledge** — a signed record per chunk, written to shared storage as soon as the chunk has
   passed its checksums.
8. **Merge and verify** — reassemble, decompress, and compare against the manifest's SHA-256.
9. **Deliver** — the receiver posts the verified file to the callback URL.
10. **Report** — a signed result record tells the sender what happened, including whether the
    delivery succeeded.

## Why there is no packet loss

One rule, in [`sender/internal/scheduler`](sender/internal/scheduler/scheduler.go): **a chunk is
displayed repeatedly until a signed acknowledgement for it arrives.**

Nothing else can promise delivery. A frame lost to a tear, a hand across the lens, or a refresh
caught mid-scan produces no report at all — only silence — so the sender cannot wait for bad news. It
waits for good news instead, and does not move on without it.

Two consequences, both deliberate:

- **An acknowledged chunk is never displayed again.** That is what makes the display converge on what
  is still missing rather than cycling through what has already arrived.
- **The display never idles.** With a window waiting on acknowledgements, it repeats the oldest
  *outstanding* frame, because a camera pointed at a blank screen learns nothing.

Error correction is an optimisation on top of that, not the guarantee: parity turns a lost chunk into
one the receiver rebuilds without a round trip. The guarantee is the acknowledgement.

The claim is tested eleven ways in [`e2e`](e2e/loopback_test.go), including one frame in three
dropped with error correction switched off entirely — recovery there is retransmission and nothing
else. Every test asserts the merged bytes are identical to what was uploaded, that no chunk is
outstanding, and that the delivered body hashes to the same value.

## Where to watch it

Three pages, all served from the same origin as the API on each side.

| URL | What it shows |
|---|---|
| `http://<sender>/display` | **The frames, live.** What a camera is looking at, one frame at a time, as the display advances. |
| `http://<sender>/display?camera=1` | The same page with nothing else on it: full screen, black surround, no toolbar. **This is what the camera should be pointed at.** |
| `http://<sender>/settings` | **Frame rate and geometry.** The panel's refresh rate is measured in the browser; the frame rate applies immediately, geometry only when nothing is in flight. |
| `http://<sender>/transfers/<id>` | A transfer's chunk map, refreshing as acknowledgements arrive, **the file as it was sent**, **every frame it rendered as an image**, and **Pause / Stop**. |
| `http://<receiver>/` | Live capture: frames captured, decoded, and unreadable, with the decode rate. |
| `http://<receiver>/transmissions/<id>` | What has arrived, what is still missing, **the file itself** — an image drawn, a video played, an archive offered as a download — **both hashes side by side**, and **where it was delivered and whether it got there**. |
| `http://<receiver>/settings` | The decoder's thresholds, and **which camera to capture from**. |

Two things about the display page are not cosmetic, because getting either wrong breaks decoding rather
than merely looking untidy. **Scaling is restricted to whole multiples** — a frame is a grid of square
cells and the decoder takes the median of each cell's pixels, so fractional scaling blends cell
boundaries into their neighbours. And **smoothing is off**: the browser's default filter is a blur, and
blur is exactly what the operating envelope budgets for the lens.

The page follows the display by long-poll (`GET /api/v1/display/next?after=<sequence>`) rather than on
an interval, with the frame inlined in the same response. At thirty frames a second there are 33
milliseconds between frames, so a second request to fetch the image would spend a meaningful share of
them; base64 costs a third more bytes over what is either loopback or a cable.

### Auditing a frame after the fact

Every rendered frame is kept — the row in Postgres and the PNG in object storage — and served at
`GET /api/v1/transfers/{id}/frames/{n}/image`, addressed by the frame number written into the frame's
own header band. That is the number the receiver reports when a decode fails, so a complaint about
"frame 214" can be answered by looking at frame 214.

It settles a question that no counter can. A chunk that took four attempts to get through was either
rendered wrongly or rendered correctly and photographed badly, and those have different fixes. The
stored image is the same bytes that went to the channel, not a re-render that might differ.

### Stopping a transfer

**Pause** and **Stop** live on a transfer's own page. Until they existed the only way to stop a
transmission was to restart the sender, which is a poor answer for a system built around transfers that
take hours.

A stop is a status change on the row, not a signal to a goroutine: the display loop re-reads the status on
every frame, so it takes effect within one frame interval, and neither the API handler nor the display loop
has to know the other exists. It also survives a restart of either.

The two differ in what they keep. **Pause** stops the display and keeps every acknowledgement, so resuming
shows only what is still outstanding. **Stop** ends the transfer; the receiver is never told, because there
is nothing to tell it — it simply stops seeing frames, which from its side is the same event as the sender
being switched off.

### Comparing the two ends by hand

The hashes agreeing is the proof, and a better proof than any inspection by eye — a single flipped bit
changes the whole digest, which nothing a person could notice would do. But a proof is not the same as being
convinced, so the receiver puts both digests next to each other character for character, offers the received
bytes for download so they can be diffed with whatever tool you trust, and links straight to the same
transfer on the sender when its interface is reachable.

That link is off by default. Set `OTP_RECEIVER_PEER_SENDER_UI_URL` to enable it — both sides address a
transfer by the same transmission id, so the sender's page for it is at `/transfers/<id>`. On a genuinely
air-gapped installation the sender's interface will not be reachable from the receiver, and a link that
cannot work is worse than none.

The receiver also shows **where the file went and whether it arrived**: the callback URL as it came across in
the manifest, the HTTP status, the attempt count, and the error if there was one. It is the only side that
can say — the URL crossed the optical channel, and the delivery was made from there.

### Keeping up: the receiver decodes concurrently

Decoding is what the receiver spends its time on — 236 ms for a frame on a 256×256 grid — and it is
per-frame work that shares nothing, so it runs on **one worker per core less one** by default
(`OTP_RECEIVER_CAPTURE_DECODE_WORKERS` to override). Everything *after* the decode stays strictly serial:
chunk rows, acknowledgements, and the merge that fires when the last chunk lands are shared state, and
keeping the applier single-threaded means none of it needed a lock.

**A display faster than the receiver transfers no more bytes.** Frames queue on the channel instead, and
the display prunes its own backlog once it is deep enough, so the surplus costs a render and a write and
delivers nothing. **Receiver → Settings** reports the deepest queue it has seen: one means it kept up, and
a large number is the direct answer to "is my frame rate too high".

### Choosing a camera

The receiver enumerates capture devices through Video4Linux directly — `VIDIOC_QUERYCAP`,
`ENUM_FMT`, `ENUM_FRAMESIZES`, `ENUM_FRAMEINTERVALS` — and offers them with their real modes.

Reading `/sys/class/video4linux/*/name` would have been easier and wrong: most webcams register two
nodes with identical names, one of which is a metadata device that produces no images at all. On the
machine this was written on, `/dev/video0` and `/dev/video1` both report "Integrated Camera"; only the
first can capture. A settings page that offered both would produce a receiver that captures nothing and
reports it as an optical fault.

**The controls are there whether or not a camera is.** That was not true at first: the panel rendered a
device list, so with an empty list it rendered an explanation and nothing to act on — useless in development,
which is exactly where it is needed. A device path can now be typed and the server takes it on trust when it
has no camera to check against, because refusing a mode the camera says it cannot do is only defensible when
the camera can be asked. A typo is still refused: `video0` is neither a path under `/dev` nor a camera index.

**The capture source is chosen there too** — `file` to read a directory, `gocv` to open a camera — which is
the first decision and the one that decides whether the rest matters, since a camera selected while the
source is `file` is recorded and never opened. It applies at the next start, because the capture loop holds
its source open and swapping it underneath would tear down a session mid-frame.

With nothing configured, the default is the lowest-numbered device that actually declares video
capture, in its **largest** mode, fastest breaking the tie. Resolution comes first deliberately: cells
the camera cannot resolve do not decode at all, whereas a slow camera only makes the sender wait — which
the acknowledgement rule already makes safe.

A mode is validated against the device before it is applied, because a V4L2 driver handed a resolution
it does not support substitutes one rather than failing. A receiver that asked for 1920×1080 and was
quietly given 640×480 would fail to resolve the grid and blame the lens.

## Transfer speed

Throughput is `chunk size × frames per second`, and the chunk size is what one frame carries. Two
things set it: how dense the modulation is, and how big the grid is.

Measured payload capacity per frame, from
[`shared/encoding`](shared/encoding/encoding_test.go) on a 96×96 grid:

| Encoding | Bits per cell | Payload per frame | At 10 fps | At 30 fps | At 60 fps |
|---|---|---|---|---|---|
| `binary` | 1 | 634 B | 6.2 KiB/s | 18.6 KiB/s | 37 KiB/s |
| `grayscale` (2-bit) | 2 | 1 269 B | 12.4 KiB/s | 37 KiB/s | 74 KiB/s |
| `color8` | 3 | 1 903 B | 18.6 KiB/s | 56 KiB/s | 112 KiB/s |
| `color16` | 4 | 2 538 B | 24.8 KiB/s | 74 KiB/s | 149 KiB/s |

Those are payload bytes, before compression. A file that compresses two to one moves at twice the
figure shown; one that is already compressed moves at the figure shown.

A larger grid scales it linearly, and **capacity depends on the grid alone, not the cell size**:
`color16` on a 256×256 grid carries **30 226 bytes a frame** whether the cells are 4 px or 8 px — 864
KiB/s at 30 fps, 1.7 MiB/s at 60. Cell size buys the camera's ability to resolve a cell and costs screen
area, nothing else. That geometry at 8 px cells is 2 080 px square, which is what a 4K panel is for.

Whether the receiver can decode at that density is an optical question, and
[`TestOpticalEnvelope`](shared/encoding/adversarial_test.go) maps where each encoding gives out:
`color8` survives the worst channel tested, and the three-bit grey ramp needs a controlled
installation.

**[Which settings to use, and what 1 MB/s actually requires →](docs/OPTIMAL-CONFIG.md)** — measured
capacity per geometry, compression ratios per codec, and the ceiling that no configuration gets around:
decoding costs 85–115 KB/s per core, so 1 MB/s needs about nine cores decoding at once and the receiver
currently decodes one frame at a time.

**What was measured end to end**, both applications in Docker with a simulated camera in the path:

| Run | Payload | Time | Rate | Retransmissions | Verified |
|---|---|---|---|---|---|
| Image, 30 fps configured | 4 920 B | 0.16 s | 6.5 KB/s | 0 | yes |
| Audio, 30 fps configured | 52 962 B | 7.9 s | 6.55 KiB/s | 0 | yes |
| In-process loopback, 200 fps | 65 536 B | 0.88 s | 73 KiB/s | 0 | yes |
| In-process loopback, repeated ×5 | 24 576 B each | ~0.33 s | 70–78 KiB/s | 0 | yes |

The Docker figures sit below the table above, and the reason is worth being straight about: the
bottleneck is the receiver's capture loop, not the channel. Each captured frame is written to disk,
degraded by the camera simulation, decoded, and recorded in Postgres before the next is read — about
three to four frames a second on this hardware — so the display at 30 fps is far ahead of it, and the
extra frames become keep-alive repeats. A real deployment is limited by the camera's frame rate and
the receiver's core count, and the fix is the ordinary one: decode captured frames concurrently. The
in-process loopback figures show what the same code does when capture is not the constraint.

Reproduce any of it:

```bash
make bench
```

## Running it

Nothing needs installing but Docker. Each side is its own compose file, because each side is what
gets installed on its own machine:

```bash
# On the machine with the display
cd sender && cp .env.example .env   # set the two secrets
docker compose up -d --build        # UI and API on :8080

# On the machine with the camera
cd receiver && cp .env.example .env  # the same OTP_*_ACK_SECRET
docker compose up -d --build         # UI and API on :8081
```

Both sides need one directory in common — an NFS or SAN mount, or a synced share — which is the only
thing they share besides the protocol. Frames go one way through it; acknowledgements come back the
other.

Both are fronted by nginx: one origin serves the browser app and proxies the API, so there is no
build-time API address, no CORS to arrange, and TLS terminates in front of the Go process rather than
inside it. The API binds to the container's loopback interface, so nginx is the only route to it.

To watch the whole path on one host, [`demo/docker-compose.yml`](demo/docker-compose.yml) runs both
sides plus a callback endpoint.

## Testing

```bash
make test
```

That runs every suite in containers — Go toolchain, both Postgres databases, and MinIO all come from
images, so a clone of this repository plus Docker is enough. `make test-local` does the same on a
host with a toolchain, skipping whatever it cannot reach.

| Suite | What it covers |
|---|---|
| [`shared`](shared) | Protocol records, golden wire vectors, five encodings against simulated optics, five compressors, four error-correcting codes, RFC 6330 conformance |
| [`sender`](sender) | Configuration and reload, migrations, the job engine under concurrency, both object stores, the pipeline end to end |
| [`receiver`](receiver) | Configuration, migrations, object stores |
| [`e2e`](e2e) | Both applications over one shared volume: loss, degradation, encryption, every encoding, callback success and failure |

## How it is put together

```
shared/     Protocol only: frame format, encodings, compression, error correction. No DB, no HTTP.
sender/     Go module + browser app. Compresses, chunks, codes, renders, displays, watches for acks.
receiver/   Go module + browser app. Captures, decodes, acknowledges, merges, verifies, delivers.
e2e/        Runs both together. Imports each side's harness, never their internals.
```

The sender and the receiver **do not import each other**, and nothing inside either may. They share a
protocol, a directory, and nothing else — which is what lets either be restarted, upgraded, or
replaced while the other keeps running. The end-to-end tests import an exported harness from each
side rather than reaching into them, so that boundary stays real.

Both applications hold their own Postgres and their own object storage (filesystem or MinIO, behind
one interface with a shared conformance suite). Every pipeline stage is a job row, because every stage
represents real elapsed time and a process that lost it to a deploy would fail on exactly the files
this exists to move.

## Documentation

- [A transfer, end to end](docs/DEMONSTRATION.md) — screenshots of the whole path, including the
  frames themselves and what the camera sees
- [Optimal configuration](docs/OPTIMAL-CONFIG.md) — which settings reach 1 MB/s, measured, and the
  ceiling that no configuration gets around
- [Design](docs/superpowers/specs/2026-08-04-optical-transport-platform-design.md) — the approved
  specification
- [Prerequisites](PREREQUISITES.md) — what to install for native builds and real hardware

## Security notes

Worth reading before deploying, because several of these are the kind of thing that is only obvious
after it goes wrong.

- **Both secrets have no defaults.** A default signing secret is not a secret, and the
  acknowledgement channel is the one input the sender takes from outside itself: anything able to
  write that directory could otherwise report chunks as delivered when they were not.
- **Callback URLs are allowlisted.** The URL crosses the air gap, so a receiver acting on any URL it
  was handed would be a request-forgery proxy for whoever controls the sender. Redirects are not
  followed.
- **Nothing unverified is delivered or served.** A merged file that fails its hash check is kept as
  evidence and refused.
- **A transferred file is never served as something a browser will execute.** Both ends serve files back
  to a browser, and both consult one allowlist ([`shared/mediatype`](shared/mediatype/mediatype.go)):
  raster images, natively playable media, and plain text may be rendered in place, and everything else
  is an opaque download with `nosniff`. **SVG is excluded deliberately** — it looks like an image and is
  an XML document that may contain `<script>`, so rendering one would be script running on the origin
  that also hosts the operator's controls.
- **Payload encryption binds a chunk to its position.** With AES-256-GCM enabled, the transmission id
  and chunk numbering are authenticated, so a chunk cannot be replayed into a different slot — a
  corruption no per-frame checksum would catch.
- **Filenames and object keys are validated, not sanitised.** Both arrive from outside and are used
  to write files; sanitising produces a different name that is accepted, which lets two inputs
  collide on one object.
- **Decompression is bounded by the manifest's declared size.** Every codec here can express a small
  input that expands without limit.
