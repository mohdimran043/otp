# Optical Transport Platform

Move large files across an air gap with a monitor and a camera. The sender draws a file as encoded frames
on a display; the receiver photographs them, rebuilds the file, and checks it against a hash the sender
declared before anything was drawn.

![How it works](docs/screenshots/00-overview-diagram.png)

**Acknowledgements travel out of band, never optically.** That is the whole design. Light goes one way, and
a frame lost to a flicker produces no error — only silence — so the sender never waits for bad news. It
waits for good news over a separate channel, and does not move on without it.

A chunk is displayed repeatedly until a signed acknowledgement for it arrives. Nothing else promises
delivery; error correction is an optimisation on top, not the guarantee.

**[See a transfer end to end, with screenshots →](docs/DEMONSTRATION.md)** ·
**[Technical overview (PDF) →](docs/optical-transport-overview.pdf)**

---

## Quick start

Nothing to install but Docker.

```bash
cd sender   && cp .env.example .env   # set OTP_SENDER_ACK_SECRET
docker compose up -d                  # UI on :8080

cd ../receiver && cp .env.example .env  # the same ack secret
docker compose up -d                    # UI on :8081
```

Send a file:

```bash
curl -X POST http://localhost:8080/api/v1/transfers \
    -F "file=@quarterly-report.tar" \
    -F "callback_url=https://intake.example.com/received"
```

Both sides need one directory in common — an NFS mount, a SAN, or a synced share. Frames go one way
through it; acknowledgements come back the other.

The form asks for a file and nothing else. Everything the curl example cannot express — the callback,
encoding, compression, error correction, encryption, and the geometry — is behind **Advanced**, closed by
default, because the defaults are the measured ones and most transfers should not touch them.

Inside it, **grid and cell size are both per transfer**: presets from 128×128 to 1024×1024, cells of 1 to 8
pixels, and either can be left on **"Auto — fit my screen,"** computed in your browser from the panel it is
actually open on. Fixing one auto-sizes the other; leaving both on Auto searches every combination that fits
and prefers the larger cell, then the larger grid — a bigger cell is worth more to a camera than a bigger
grid, because it is the blur budget that fails first. The deployment-wide values in `.env` remain the
default for any transfer that does not choose.

## Transfer speed

Throughput is **bytes per frame × frames per second**. Bigger grids carry more per frame; the panel sets
how many frames a second. The grid is chosen once per transfer, in the New Transfer form; the tables below
describe what each grid carries, whichever transfer picks it.

**Capacity per frame, measured** — `color16` is four bits per cell, `color8` is three:

| Grid | Frame size<br>at 4 px cells | `color8` | `color16` | Best for |
|---|---|---|---|---|
| **256×256** | 1 040 px | 22 669 B | **30 226 B** | 1080p panel, any camera |
| **384×384** | 1 552 px | 52 716 B | **70 288 B** | 4K panel — **the measured sweet spot** |
| **512×512** | 2 064 px | 94 860 B | **126 480 B** | 4K panel + 4K camera, for the largest files |

**What that means in throughput**, `color16`:

| Grid | 25 fps | 40 fps | 60 fps | 1 GB takes |
|---|---|---|---|---|
| 256×256 | 738 KB/s | 1.15 MB/s | 1.73 MB/s | 10 min at 60 fps |
| **384×384** | **1.68 MB/s** | 2.68 MB/s | 4.02 MB/s | 4 min at 60 fps |
| **512×512** | 3.02 MB/s | 4.82 MB/s | **7.24 MB/s** | **2.4 min at 60 fps** |

**Measured end to end**, 8 MB of incompressible data, verified and delivered:

| Configuration | Offered | **Achieved** | Losses |
|---|---|---|---|
| 256×256 @ 8 px, 40 fps | 1.15 MB/s | 730 KiB/s | none |
| **384×384 @ 5 px, 25 fps** | 1.68 MB/s | **1.45 MB/s** | none |

**Prefer fewer, larger frames.** Twice the throughput at less than two-thirds the frame rate — every frame
costs the same fixed overhead whatever it carries, so larger frames amortise it.

### One transfer, all the arithmetic

An 8.6 MB file at `512×512` / `color8` / `zstd` / Reed–Solomon 32+8 — the defaults, on the largest grid that
a 4K panel and a 4K camera can carry:

| | | |
|---|---|---|
| The file | **8 612 637 B** | as uploaded |
| Compressed | **6 232 607 B** | zstd, 72.4% of the original — 27.6% never has to cross |
| Per frame | **94 860 B** | 512×512 at three bits a cell, from the table above |
| Data chunks | **66** | `⌈6 232 607 ÷ 94 860⌉` — 65 full, and a last one of 66 707 B |
| FEC blocks | **3** | `⌈66 ÷ 32⌉` — coding runs per block, not over the whole file |
| Parity chunks | **24** | 3 blocks × 8 parity each |
| **Frames displayed** | **91** | 90 chunks + 1 manifest |

The parity figure is the one that surprises people: 24 for 66, not the 16 or 17 that 32+8 suggests. Coding
is per block, and the last block holds only the 2 shards that were left over — yet it still gets a full 8
parity shards, 400% redundancy on the tail. That is not waste to engineer away. Shards near the end of a
file are the ones a transfer that gets cut short loses, and a short final block is exactly the case where
losing two chunks would otherwise be unrecoverable.

At 25 fps one pass over all 91 frames takes 3.6 seconds. A pass is not the transfer, though — each chunk
keeps being redisplayed until its acknowledgement arrives, so what the panel actually spends its time on is
whatever the receiver has not yet confirmed.

### The ceiling: the receiver, not the display

Decoding costs **85–115 KB/s per CPU core**, and that barely changes with geometry. Your core count sets
the limit:

```
frames per second  =  cores × 115 000 ÷ bytes per frame
```

| Cores | Sustainable | Reality |
|---|---|---|
| 4 | 460 KB/s | A laptop |
| 19 | **2.2 MB/s** | The 20-core workstation everything here was measured on |
| 64 | 7.2 MB/s | No ordinary machine — **use several channels instead** |

**7 MB/s is a multi-channel target.** Four screen–camera pairs at 1.8 MB/s each, carrying disjoint ranges
of chunks of one file. The foundations exist — chunks are addressed by number, the database claims work
atomically, acknowledgements are per chunk — but the range-claiming display loop is **designed and not yet
built** ([overview](docs/optical-transport-overview.pdf), §8).

## Hardware you need

| | Up to ~1.5 MB/s | For 7 MB/s |
|---|---|---|
| Grid | 384×384 @ 5 px | 512×512 @ 4 px |
| Panel | One 4K, 30 Hz | Four 4K, 60 Hz |
| **Camera** | **1080p is enough**<br><span title="388 cells across a 1080px sensor">2.78 sensor px per cell</span> | **4K required**<br>1080p gives only 2.09 px per cell — no margin for blur |
| Camera rate | ≥ 2× the display | ≥ 60 fps, MJPG, global shutter preferred |
| Receiver | 1 machine, ~19 cores | 4 machines, ~19 cores each |
| Blur budget | 1.0 px | **0.8 px** — the tightest constraint in the whole setup |

## Where to watch it

| URL | What it shows |
|---|---|
| `<sender>/display?camera=1` | **Point the camera here.** Full screen, black surround, nothing else |
| `<sender>/settings` | Frame rate, the deployment's default geometry, saved encryption keys, and the channel toggle. The panel's refresh rate is measured in your browser |
| `<sender>/transfers/<id>` | Chunk map, the file as sent, every frame as an image, Start / Pause / Stop / Delete, Download frames |
| `<receiver>/` | Live capture — frames arriving as thumbnails, labelled by chunk |
| `<receiver>/camera` | **Start the camera here.** Opening this page is what asks the browser for permission |
| `<receiver>/transmissions/<id>` | The file itself, both hashes side by side, where it was delivered, Delete |
| `<receiver>/settings` | Decoder statistics, the decryption keyring, the callback allowlist |

Two details on the display page are load-bearing rather than cosmetic: **scaling is whole multiples only**
and **smoothing is off**. The decoder takes the median of each cell's pixels, so fractional scaling blends
cells into their neighbours, and the browser's default filter is a blur — which is exactly what the optical
budget reserves for the lens.

## The channel: a directory, or actual light

By default the sender writes every rendered frame into the shared directory as a PNG, which is what lets a
receiver reading that directory work with no camera at all. It is the right default for development and for
the offline round trip below — but in a deployment where the channel really is optical, it is a second,
invisible path carrying the same bytes, and the air gap it was meant to prove is not being tested.

**Settings → Transfer channel** turns it off. On *camera only*, frames are still rendered, still counted,
still displayed — and written nowhere. The only way to receive them is to point a camera at the screen.

```
OTP_SENDER_DISPLAY_SINK=file    # write PNGs into DISPLAY_DIR
OTP_SENDER_DISPLAY_SINK=none    # render and display only; nothing on disk
```

Like the grid, the sink is read once at startup, so it **takes effect on the next restart** rather than live,
and the change is refused outright while any transfer is in flight.

Either **Settings → Transfer channel** or `.env` works: a change made in the UI is written down and laid back
over the configuration on the next start, so it survives the restart it needs. Stored settings win over `.env`
for the fields the settings page manages, which is what makes "I changed it in the UI" mean anything — the
corollary being that editing such a field in `.env` no longer wins once the UI has set it.

### Start the camera before the transfer

**This ordering is not advice, it is a requirement**, and getting it wrong looks like a hang rather than a
mistake. The manifest — the frame carrying the filename, the size and the hash — is displayed early and then,
once every chunk has been acknowledged, the display parks and never shows it again. A camera that starts
watching after that window decodes every chunk perfectly, acknowledges them all, and still cannot assemble
anything, because it never learned what it was assembling. The sender sits at `transmitting` with everything
acknowledged, indefinitely.

So: open the receiver's Camera page and press **Start** first, confirm frames are being decoded, and only then
create the transfer. Recovering from the wrong order means cancelling and sending again.

## Cameras

Three ways to capture, all feeding the same pipeline:

- **`browser`** — the page holds the camera via `getUserMedia`. This is the one that **asks permission** and
  shows the operating system's indicator, and it works from a phone. Needs HTTPS: browsers will not give a
  camera to an insecure page, and `localhost` is the only exception.
- **`camera`** — the receiver opens a V4L2 device directly. No OpenCV, no cgo. Fastest, and needs the device
  passed into the container.
- **`file`** — reads a directory the sender's display writes into. No camera at all, for development.

A camera configures itself: the lowest-numbered device that actually declares video capture, in its largest
mode. It keeps watching, so one plugged in later is noticed — but it never overrides a choice you made.

**The camera has its own page**, `<receiver>/camera`, and opening it is what asks for permission. That is
deliberate: a browser only prompts when `getUserMedia` is called, and it will not tell you a camera's name
until permission has been granted once — so a page that never asks shows an unlabelled device list and no
prompt. Choosing the camera used to live in Settings, where the only thing that asked was a Start button
most operators never reached. Granting access to hardware deserves the page it is on. Capture itself still
waits for **Start**; opening the page asks, it does not begin.

The indicator distinguishes *selected* from *actually running*. Selecting `camera` opens the device
immediately — it is exclusive, so being selected is being open. Selecting `browser` only means the receiver
will accept frames if a page posts them, so it is reported as streaming when a page has posted within the
last two seconds, and not merely because it is the chosen source.

**The page tells you what it is decoding, out loud.** An operator aiming a camera at a monitor cannot watch the
monitor they are aiming at, so the Camera page beeps: a low note when the manifest arrives, one higher beep for
each chunk decoded, and a rising chime when the merged file verifies. Beeps mark *new* chunks rather than
decoded frames — the display repeats a chunk until it is acknowledged, so a beep per frame would be a
continuous drone meaning only "the camera is pointed at something". Silence means the display has nothing left
to give. Alongside them, frames decoded and the decode rate are shown rather than sounded; a decode rate
falling is a lens drifting out of focus, visible long before a chunk stops arriving. The speaker icon silences
it.

## Encrypting a transfer

An optical channel is a broadcast, not a wire. Anything with line of sight to the monitor receives every
frame the display draws, and the protocol is documented, so a second camera in the room decodes a file
exactly as well as the receiver does. An air gap keeps a transfer off the network; it does nothing about who
else is looking at the screen. Encryption is what makes the channel confidential rather than merely
inconvenient to intercept.

Each transfer chooses for itself, in the New Transfer form or the equivalent API fields: **none** by
default, **AES-256-GCM**, or **ChaCha20-Poly1305**. The cipher ID travels in every frame's header, so the
receiver knows what it is looking at without being told out of band — but the payload itself is unreadable
without the key.

The key's path is deliberately manual. It is generated in the sender's form (or typed in by hand), and it is
the operator's job to carry it to the receiver's **Settings** page — over a phone call, a password manager,
a second air-gapped channel, anything that is not the optical one. No API returns key material once it is
stored, and the key itself never crosses the light.

**The honest caveat.** A manifest's filename, content hashes, and callback URL are not part of the encrypted
payload and stay readable to anyone watching the display — the receiver needs them before it has the key, to
know what it is assembling, to verify it afterwards, and to know where to deliver it. And encryption protects
the optical channel specifically: the sender's own database holds the uploaded file in plaintext regardless,
because it has to, to render the frames in the first place. Encrypting a transfer answers "who else can see
the screen," not "who can reach the sender."

## Moving frames without light

Every rendered frame can leave the sender as a file instead of a photograph, and come back into the receiver
the same way — for a transfer that has to cross on a USB stick, or a one-off where standing up a camera
isn't worth it.

**Download frames**, on the transfer's own page, packages every frame — the manifest and all chunk and
parity frames — as a zip. A transfer that fits in one chunk is packaged instead as a single composite PNG,
the manifest stacked above the data frame, because there is no second frame to zip.

**Import frames**, on the receiver's Transmissions page, takes that zip or PNG back in. It is not a shortcut
around the pipeline: imported frames are acknowledged, merged and verified exactly as frames captured by a
camera are, through the same code path. An encrypted archive needs its key loaded into the receiver's
keyring first — importing before that produces the same acknowledged-but-unreadable failure a camera would
produce pointed at the same encrypted screen.

### Preparing a transfer without sending it

A transfer normally begins displaying the moment it is ready. Turning off **"Start displaying immediately"**
in the New Transfer form (or `autostart=false` on the API) stops it at `ready` instead: the file is
compressed, chunked, coded and rendered, and then nothing is drawn until someone says so.

That is what makes the round trip above practical, and it is useful on its own. Preparing at 09:00 and
displaying at 02:00 needs no scheduler. A transfer can be prepared, its frames downloaded, and the camera
never involved. And a large file's expensive part — the rendering — happens while you are watching, not
while the panel is unattended.

```bash
curl -X POST http://localhost:8080/api/v1/transfers -F "file=@report.tar" -F "autostart=false"
curl -X POST http://localhost:8080/api/v1/transfers/<id>/start   # when you are ready
```

**Start** appears on the transfer's page for exactly as long as it is `ready`. Starting anything else is
refused — a transfer already displaying, or finished, has nothing to start.

## Deleting what you sent

Cancelling a transfer stops it. It does not reclaim anything — the chunks, the coded shards and every
rendered frame are still there, and a sender that has run for a month holds every frame of every transfer
anyone ever started. **Delete** is the one that reclaims, on the transfer's page or the row in the list:

```bash
curl -X DELETE http://localhost:8080/api/v1/transfers/<id>        # sender
curl -X DELETE http://localhost:8081/api/v1/transmissions/<id>    # receiver
```

Both sides delete objects first and rows last, deliberately. Objects gone with the rows still present is a
state a retry repairs, because the rows still name what needs cleaning up; rows gone with objects still
present is a leak nothing will ever find again, because nothing left in the database points at them.

**The sender refuses while a transfer is in flight** — `preparing`, `transmitting` or `paused` all return
409 `cancel it first`, because deleting the frames out from under the display loop is not something an
operator means to do by accident. The receiver has no such state and so no such guard: a transmission there
is either finished, or one whose frames are still arriving, and deleting that only means the next chunk to
decode starts a fresh row from nothing.

### The 24-hour sweep

A transfer that never finished is worse than a large one — nothing in the sender ever revisits it, and the
pipeline moves forward or stops rather than cleaning up after itself. So an hourly sweep deletes anything
older than a day that never reached `completed`, by exactly the code path a manual delete uses:

```
OTP_SENDER_RETENTION_INTERVAL=1h
OTP_SENDER_RETENTION_MAX_AGE=24h
```

`completed` is the one status it will never touch, however old. Everything else is fair game, and **that
includes a transfer still transmitting** — "never completed" is the whole test, on the reasoning that a
transfer stuck mid-flight for a day has abandoned just as much storage as one that failed outright. Note the
asymmetry with the paragraph above: the sweep will reap a `transmitting` transfer that a manual DELETE would
refuse. At the measured 1.45 MB/s that needs a transfer over ~125 GB to matter, but if you legitimately send
files that take longer than a day, raise `MAX_AGE` past the longest transfer you expect.

## Running 24/7

Streaming frames continuously does not wear the camera out. A sensor reads out
electronically — there is no shutter mechanism actuating per frame, no moving part
that accumulates cycles the way a DSLR's mirror does. A webcam or machine-vision
camera pointed at a monitor for a year performs the same read-out on its last frame
as on its first. What actually deserves attention in a permanent installation:

- **Heat.** A sensor streaming at 60 fps runs warm, and warm sensors are noisier.
  Give the camera airflow and keep it out of direct sunlight; noise shows up in the
  decode quality figures long before frames fail.
- **Autofocus.** Turn it off. Focus hunting is the only mechanical motion in the
  system, it is pointless — the target never moves — and every hunt is a stretch of
  blurred, undecodable frames. Fixed-focus or locked-focus lenses are the right tool.
- **The panel, not the camera.** The frames change constantly, so the grid area
  cannot burn in — but the static black surround on an OLED can retain. On an LED
  panel there is nothing to worry about; on an OLED, let the display page's surround
  stay pure black (it does) and prefer LED for permanent duty.
- **The receiver keeps up or tells you.** Decode statistics are per session; a slow
  decline in finder scores is a lens drifting or dust accumulating — the platform's
  earliest warning, visible on the receiver's front page.

## Object storage

Both sides store uploads, chunks, rendered frames and merged files through one small interface, and either
can be backed by a local directory or by S3-compatible object storage. Filesystem is the default and needs
nothing:

```
OTP_SENDER_STORAGE_BACKEND=filesystem     # or minio
OTP_RECEIVER_STORAGE_BACKEND=filesystem   # or minio
```

**Each stack runs its own MinIO.** Not one shared instance — the two applications share a protocol and a
directory and nothing else, and giving them one object store would be a dependency the design does not have.
The sender's lives in the `sender-minio` volume with the `otp-sender` bucket, the receiver's in
`receiver-minio` with `otp-receiver`, and neither can see the other's.

Both compose files already run MinIO and create the bucket **whether or not the backend is ever switched**,
so turning it on is one variable and a restart rather than new infrastructure. Nothing publishes MinIO's
ports, so the console is not reachable from the host by default; look at a bucket from inside the stack.
`minio-init` is a one-shot that has already exited by the time you want to look, so this runs a fresh
throwaway copy of it rather than `exec`ing into the dead one:

```bash
docker compose run --rm --entrypoint sh minio-init -c 'mc alias set local http://minio:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null && mc ls --recursive local/otp-sender/'
```

Versions are pinned, and the two pins do not match on purpose: `minio/minio:RELEASE.2024-11-07T00-52-20Z`
and `minio/mc:RELEASE.2024-11-17T19-35-25Z`. The server and the client are versioned and released
independently, and there is no server release tagged 2024-11-17 — the November release nearest that client
pin is the one above.

**Switching backends does not migrate anything.** The two are separate namespaces, so a sender flipped from
`filesystem` to `minio` will not find the frames it wrote before the switch. Do it on a quiet deployment, or
accept that older transfers lose their objects while their rows remain.

## Testing

```bash
make test
```

Three things are **kept outside this repository**: the containerised test stack that supplies the Go
toolchain, both databases and MinIO; the end-to-end suite that drives both applications against each
other under loss, degradation and every encoding; and the single-host demonstration stack.

`make test` notices what is absent rather than failing on it. Without the container stack it runs the
suite against your host toolchain, and **every test needing Postgres or MinIO skips rather than runs** —
so read the skips. A pass from a bare clone is a narrower claim than a pass with the full stack, and the
unit tests below are what remains true either way.

| Suite | Covers |
|---|---|
| [`shared`](shared) | Protocol, five encodings against simulated optics, five compressors, four error-correcting codes, RFC 6330 conformance |
| [`sender`](sender) | Configuration, migrations, the job engine under concurrency, object stores, the pipeline, deletion and the retention sweep |
| [`receiver`](receiver) | Camera enumeration, capture sources, decoding, object stores, deletion, the capture-source indicator |

## How it is put together

```
shared/     Protocol only: frame format, encodings, compression, error correction. No DB, no HTTP.
sender/     Go + React. Compresses, chunks, codes, renders, displays, watches for acknowledgements.
receiver/   Go + React. Captures, decodes, acknowledges, merges, verifies, delivers.
```

The two applications **do not import each other**. They share a protocol, a directory, and nothing else —
which is what lets either be restarted, upgraded or replaced while the other keeps running. Each exposes a
`harness` package so an out-of-tree test can drive a whole side without reaching into its internals.

## Documentation

- [A transfer, end to end](docs/DEMONSTRATION.md) — screenshots of the whole path
- [Technical overview (PDF)](docs/optical-transport-overview.pdf) — twelve pages for a
  non-specialist reader: the setup, speed at each end, what a chunk is, how acknowledgement works, and how
  it would scale out
- [Optimal configuration](docs/OPTIMAL-CONFIG.md) — every measured figure and what to set

## Security notes

- **Neither API authenticates yet, and both now have destructive endpoints.** Anyone who can reach the
  sender can upload a file, change the geometry, or `DELETE` a transfer and every frame it produced; anyone
  who can reach the receiver can start a camera or `DELETE` a received transmission, including a merged file
  that has not been downloaded yet. There is no undo and no soft delete on either side. This was always the
  case for the geometry; deletion makes the consequence of leaving these open considerably worse. Put
  authentication in front of them before exposing either.
- **Both secrets have no defaults.** A default signing secret is not a secret, and the acknowledgement
  channel is the one input the sender takes from outside itself.
- **Callback URLs are allowlisted, redirects not followed.** The URL crosses the gap from outside the
  receiver's trust boundary; without a list the receiver would be a request-forgery proxy.
- **Nothing unverified is delivered or served.** A merged file failing its hash is kept as evidence and
  refused.
- **A transferred file is never served as something a browser executes.** One allowlist
  ([`shared/mediatype`](shared/mediatype/mediatype.go)) governs both ends. **SVG is excluded** — it looks
  like an image and is an XML document that may carry `<script>`.
- **Decompression is bounded by the manifest's declared size.** Every codec here can express a small input
  that expands without limit.
- **Per-transfer keys never cross the optical channel and are never returned by any API.** A key is
  generated on the sender and carried to the receiver's Settings by the operator, out of band; once stored,
  no endpoint on either side echoes it back — `GET`ting a transfer or a keyring entry shows that a key
  exists, never what it is.
