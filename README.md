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

A larger grid scales it linearly. `color16` on a 256×256 grid carries about 18 KiB a frame — roughly
**530 KiB/s at 30 fps, or 1 MiB/s at 60** — which is where a 4K panel and an industrial camera would
actually be run. Whether the receiver can decode at that density is an optical question, and
[`TestOpticalEnvelope`](shared/encoding/adversarial_test.go) maps where each encoding gives out:
`color8` survives the worst channel tested, and the three-bit grey ramp needs a controlled
installation.

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
- **Payload encryption binds a chunk to its position.** With AES-256-GCM enabled, the transmission id
  and chunk numbering are authenticated, so a chunk cannot be replayed into a different slot — a
  corruption no per-frame checksum would catch.
- **Filenames and object keys are validated, not sanitised.** Both arrive from outside and are used
  to write files; sanitising produces a different name that is accepted, which lets two inputs
  collide on one object.
- **Decompression is bounded by the manifest's declared size.** Every codec here can express a small
  input that expands without limit.
