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

### Using the browser's camera — the path that asks permission

**Receiver → Settings → Use this browser's camera.** Press Start and the browser asks *"Allow this site to use
your camera?"*, the operating system shows its own indicator, and the page holds the camera for as long as it is
open — which is also what makes the light stay on rather than flicker.

The frames are posted to `POST /api/v1/capture/frames` and enter the same pipeline as everything else: persist,
decode, acknowledge, merge, verify, deliver. Nothing downstream knows which source produced an image, which is
the point of the `Source` interface.

Why this exists alongside the direct camera source: **a server process cannot ask.** Opening `/dev/video0`
produces no dialog and no operating-system indicator, because the permission was granted once, by whoever passed
the device into the container. A browser can ask, and can do it from any machine that reaches the receiver rather
than only the one the receiver runs on — no device passthrough, no compose overlay.

What it trades away is throughput: encoding each frame in a canvas and posting it will not keep up with reading
V4L2 buffers directly. So this is the path for setting a camera up and watching it work, and the direct source is
the path for moving fifty megabytes. Neither pretends to be the other.

Verified end to end by posting real rendered frames the way the page does: every one accepted, decoded, and
recorded with its chunk number and a fiducial match of 1.00.

### Starting and stopping the camera

**Start camera** and **Stop camera**, on the receiver's Settings page, rather than a Save button.

That was a real fault and not a cosmetic one. "Save capture settings" did two unrelated things depending on
what you had touched: choosing a device configured its *mode* and left the source alone, so an operator saved,
saw a success, and watched a camera that never lit up. Starting a camera and changing its mode are different
acts and now have different buttons.

The page reports what is happening rather than what was asked for: **The camera is running — its light is on**,
or **No camera is running — frames are being read from `file`**. Verified against the kernel: after Start the
container holds one file descriptor on `/dev/video0`; after Stop it holds none. The device is genuinely opened
and genuinely released, which is what the light follows.

Stopping switches back to reading frames from a directory, so the file-backed path is always there to fall
back to — no camera needed to develop against.

### Selecting a camera starts it

The capture source is swapped while the receiver runs, so choosing a camera means the camera opens — not that a
preference is filed for the next restart. That was the first attempt, and the reasoning behind it (the capture
loop holds its source open) turned out to be an argument for doing the swap carefully rather than for not doing
it: an operator clicking a camera and being told to restart the service is being asked to do the work the
service should do.

**The order of a swap depends on what is open**, and this is the part that has to be right. A directory can be
opened twice, so the new source is opened first and a failure leaves the receiver reading from whatever it had.
A camera cannot: attempting it fails with `device or resource busy` every time — which it did, when
re-selecting the camera that was already running. So an exclusive source is closed first, and if the
replacement then will not open, the previous configuration is reopened to put things back.

### Watching frames arrive

**Receiver → Live capture** shows the newest captures as thumbnails, newest first, refreshing on the interval
you choose. Each is the stored image — the bytes the decoder was actually given, not a re-render — labelled with
the chunk it carried, coloured by kind, and carrying its fiducial, timing, contrast and bit-error figures in a
tooltip.

The counters above it answer "is it working"; this answers **"is it working now"**. A count that has stopped
moving looks exactly like one moving slowly, and that difference is the whole question when a camera has just
been aimed. Failures appear alongside successes, deliberately: a panel showing only what decoded would look
healthy while a camera drifted out of focus.

### Capturing from a real camera

`OTP_RECEIVER_CAPTURE_SOURCE=camera` opens one and streams from it, through Video4Linux's memory-mapped
buffer interface — **no OpenCV and no cgo**, for the same reason the decoder is written directly. MJPEG
frames decode with the standard library; YUYV is converted in full colour rather than to grey, because
these encodings put information in hue and reducing to luma would throw away three of the four bits a
`color16` cell carries.

Measured on the machine this was written on: `/dev/video0` opened at **1920×1080 MJPG**, five frames
captured in half a second, and the mode read back from the driver rather than assumed — a driver handed a
format it cannot provide substitutes one and reports what it actually set, and a receiver that believed
otherwise would fail to resolve the grid and blame the lens.

**A camera that is waiting stays quiet.** This is the difference between a camera and a directory: the file
channel goes quiet between transmissions and says so, whereas a camera pointed at a dark screen keeps
producing images of a dark screen thirty times a second. Every one of those would otherwise be stored and
recorded as an unreadable capture — thousands of rows saying nothing but "not started yet", burying the
failures that mean something.

So a frame with nothing in it is reported as *no frame*, which is what it honestly is. The test is a
property every frame this protocol renders has and almost nothing else does: **a lot of pure black and a lot
of pure white at once**, guaranteed by the quiet zone, the four fiducials and the always-binary header and
footer bands. A dark room has the black and none of the white; a blank white screen the reverse; a
photograph of a desk has neither. It is a gate rather than a decision — anything that passes still goes
through the real decoder, which rejects it on its checksums if it was a false positive.

Observed on the deployed stack with the camera open and nothing displayed: 53 seconds, zero frames
captured, zero failures recorded. Start a transfer and it begins.

### Choosing a camera

The receiver enumerates capture devices through Video4Linux directly — `VIDIOC_QUERYCAP`,
`ENUM_FMT`, `ENUM_FRAMESIZES`, `ENUM_FRAMEINTERVALS` — and offers them with their real modes.

Reading `/sys/class/video4linux/*/name` would have been easier and wrong: most webcams register two
nodes with identical names, one of which is a metadata device that produces no images at all. On the
machine this was written on, `/dev/video0` and `/dev/video1` both report "Integrated Camera"; only the
first can capture. A settings page that offered both would produce a receiver that captures nothing and
reports it as an optical fault.

**It configures itself.** At startup the receiver takes the lowest-numbered device that actually declares
video capture, in its largest mode — verified against real hardware, where it found the built-in camera and
chose 1920×1080 at 30 fps in MJPG without being asked. It keeps looking, too, because a camera is not a fixed
part of a machine: the one an operator wants is usually plugged in after boot, and needing a restart to notice
a USB device reads as broken.

What it will not do is override a working choice. An operator who selected the second camera keeps it when a
third appears, because their decision is better evidence than the order the kernel enumerated the devices in.
If the chosen camera is *unplugged*, another is substituted and the receiver says so loudly — capturing from a
camera nobody chose is exactly the surprise that must not be sprung quietly.

**Clicking a device configures it.** Choosing a camera is the whole of the intent; picking between eighteen
modes afterwards is answering a question the receiver can answer better. So naming a device with no mode is
treated as a request to be configured rather than an incomplete request, and the server fills in the largest
frame that camera offers.

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

## The public addresses

Both sides, tunnelled from this machine:

| | URL | Password |
|---|---|---|
| **Sender** | **https://episode-northern-absence-carl.trycloudflare.com** | **none** |
| **Receiver** | **https://cinnamon-dandruff-deprecate.ngrok-free.dev** | `NGROK_BASIC_AUTH` in `receiver/.env` |

**Two providers, because a free ngrok account has exactly one endpoint.** Both sides define an ngrok tunnel and
only one can be online at a time — the second fails with `ERR_NGROK_334`, "the endpoint is already online". So the
receiver keeps ngrok, where a password can be set, and the sender uses a Cloudflare quick tunnel, which needs no
account at all.

**The sender's address has no password, and that is the one thing to be careful about.** A quick tunnel has
nowhere to put one and the sender's API does not authenticate itself, so anyone who finds the address can upload a
file, change the frame geometry, or cancel a transfer. Stop it when you are not testing:

```bash
cd sender && docker compose stop cloudflared
```

Neither address is stable. Both die when the containers stop, and both providers hand out a different hostname
next time — a free ngrok endpoint is persistent per account but the quick tunnel is not.

## Reaching it from a phone, or anywhere else

Each side can publish itself over HTTPS through an ngrok tunnel defined in its own compose file. It is behind a
profile, because a service that publishes itself the moment somebody runs `docker compose up` is not one anybody
should ship.

```bash
# Once, per side: put your token in sender/.env and receiver/.env
#   NGROK_AUTHTOKEN=...            from https://dashboard.ngrok.com/get-started/your-authtoken
#   NGROK_BASIC_AUTH=user:password
cd sender   && docker compose --profile public up -d
cd receiver && docker compose --profile public up -d

# The public addresses, read from each agent's own inspector
curl -s localhost:4040/api/tunnels | python3 -c 'import json,sys;print(json.load(sys.stdin)["tunnels"][0]["public_url"])'
curl -s localhost:4041/api/tunnels | python3 -c 'import json,sys;print(json.load(sys.stdin)["tunnels"][0]["public_url"])'
```

**HTTPS is not a nicety here.** A browser will not hand a camera to an insecure page, and `localhost` is the only
other context it treats as secure — which does not help a phone. So a tunnel is the shortest route to capturing
with a phone's camera at all.

**Both tunnels have basic auth on by default, deliberately.** Neither application authenticates its own API yet,
so a public address without a password lets anyone who finds it upload files, change the frame geometry, cancel
transfers, start a camera and read what has arrived. One flag is a poor substitute for real authentication and a
great deal better than none. Remove `--basic-auth` from the compose file only if the address is genuinely meant
to be open.

## On a phone

Both interfaces are built for it, and the receiver is the one that matters: a phone is usually the easiest camera
to aim at a display.

- **The rear camera is preferred by default on a phone**, because the point is to photograph a screen and the
  front camera points at whoever is holding it. A laptop has no rear camera and ignores the preference.
- **Start and Stop are full-width** at phone size, and the navigation tabs scroll rather than overflowing into
  nothing.
- **The camera keeps running while you move around the interface.** It is held outside the page components, so
  going from Settings to Live capture to watch frames land does not release it — which it used to, at exactly the
  moment you would want it not to. It stops when you press Stop or close the tab, and at no other time.

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
