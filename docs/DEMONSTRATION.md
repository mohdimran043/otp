# A transfer, end to end

Everything below was captured from the two applications running in Docker on one host, on
2026-08-06. Both are the images `sender/Dockerfile` and `receiver/Dockerfile` build, behind their own
nginx, each with its own Postgres, sharing one volume. Reproduce it with:

```bash
cd demo && docker compose up -d --build
```

The receiver in this demonstration has a **simulated camera** in front of its file source
(`OTP_RECEIVER_CAPTURE_SIMULATE=typical`): every frame is blurred, tilted off-axis, vignetted,
given sensor noise, and JPEG-compressed before the decoder sees it. Without that, a file-to-file
run would only show that the decoder can read the encoder's own output, which is the easiest case
there is.

---

## 1 · Submitting a file

The whole platform is driven by one request: a file, and a URL the result should go to. Everything
after this happens without the caller.

![The sender's upload form](screenshots/01-sender-send-form.png)

The encoding, compression, and error-correction lists come from the server rather than being
hard-coded in the page, so a build that added an encoding offers it and one that did not cannot be
asked for it. The note at the bottom is the frame geometry in force — it needs a restart to change,
because it is written into every frame header.

The callback URL is worth reading carefully: **the receiver** posts the file there, once it has
reassembled and verified it. The sender never does. The URL travels across the optical channel in
the manifest, because the sender is where a caller supplies it and the receiver is the side that
ends up holding a finished file.

---

## 2 · What actually crosses the gap

The file is compressed, cut into chunks sized so that exactly one chunk fills exactly one frame,
error-coded, and rendered. This is a real frame from the transfer below — 96×96 cells at 6 pixels
each, `color8` encoding, three bits per cell:

![A rendered optical frame](screenshots/03-optical-frame-color8.png)

Four corner fiducials, a binary-modulated header band across the top, the colour payload in the
middle, a binary footer band carrying the payload's CRC32 and SHA-256, and timing columns down each
side. The header and footer are always plain black and white whatever the payload modulation is —
that is what lets a receiver read a frame it knows nothing about, learn the geometry and encoding
from it, and only then demodulate the payload.

Frame 0 is the manifest, which is why it looks different: it carries the filename, sizes, the
original SHA-256, the chunk geometry, the error-correction parameters, and the callback URL.

![The manifest frame](screenshots/04-optical-frame-manifest.png)

---

## 3 · What the camera sees

The same frame after the simulated optics — this is the image the decoder is actually given:

![The frame as captured through simulated optics](screenshots/05-camera-capture-degraded.png)

It is 696×696 rather than 600×600 because the frame now occupies part of a larger field of view, as
it would on a sensor. It is rotated a few degrees, softened, noisy, darker at the corners, and
carries compression artefacts. Every frame in this transfer decoded anyway: `frames_failed` was
zero.

That is what the four corner fiducials are for. Four points rather than QR's three supply the
correspondences for a full homography, so a genuinely off-axis view can be corrected rather than
merely tolerated.

---

## 4 · The sender, transferring

![The sender's dashboard](screenshots/06-sender-dashboard.png)

![A completed transfer](screenshots/07-sender-transfer-complete.png)

Reading the completed transfer above: 51.7 KiB of audio, 28 chunks, 39 frames rendered (28 chunks +
7 parity shards + 3 manifest re-emissions), **zero retransmissions**, and the receiver's own verdict
at the top — merged, hashed, and matched.

"305 displays in total" for 39 frames is not waste. The display never idles: while a window is
waiting on acknowledgements it repeats the oldest *outstanding* frame, because a camera pointed at a
blank screen learns nothing. What it never does is repeat a frame whose chunk has already been
acknowledged, which is what makes the transfer converge rather than cycle.

The chunk map is the picture worth having during a transfer: filled means acknowledged, outlined
means still outstanding, and a dot marks a parity shard.

---

## 5 · The receiver

![The receiver's live capture](screenshots/09-receiver-live-capture.png)

The decode rate is the figure that matters most here. It falls before frames start failing outright,
which makes it the earliest warning that a camera needs attention — sooner than a failure count,
which only moves once things are already going wrong.

---

## 6 · The file, arrived

![The received image](screenshots/10-receiver-image-received.png)

The two hashes are the point of the whole exercise: what the receiver computed over the reassembled
file, and what the sender declared in the manifest. They match, so this is the file that was sent —
not a file that looks like it.

The image is rendered rather than merely reported, because a hash proves a transfer worked while an
image lets an operator recognise the thing they sent. Nothing unverified is ever rendered or served:
a merged file that failed its hash check is kept as evidence and refused on the download endpoint.

Audio arrives the same way and is playable in place:

![The received audio](screenshots/11-receiver-audio-received.png)

![The receiver's transmission list](screenshots/12-receiver-transmissions.png)

---

## 7 · Delivery to the callback

Once verified, the receiver posts the file to the URL that came across in the manifest. The endpoint
in this demonstration recomputes the hash for itself rather than trusting the header:

```json
{
  "transmission_id": "44320e09-235f-4fb4-9bf7-ece317417060",
  "filename": "colour-test.png",
  "bytes": 20915,
  "declared_sha256": "e7988de79d701069fff4409997c2383fa5d530e7f7e2f58bb69fcb56a6361c96",
  "actual_sha256": "e7988de79d701069fff4409997c2383fa5d530e7f7e2f58bb69fcb56a6361c96",
  "match": true
}
```

The receiver then writes a signed result record back to the shared volume, which is how the sender —
which has no other view of the far side — learns that the transfer worked and that the file was
handed on:

```json
{
  "verified": true,
  "sha256": "4236da46021c81817d128a712f9da13392eaf2830c5d6423fb7883f688dfb1da",
  "chunks_received": 28,
  "chunks_recovered": 0,
  "frames_captured": 34,
  "frames_failed": 0,
  "callback_delivered": true,
  "callback_status": 200
}
```

A receiver will not deliver to any host it is told to. The URL crossed the air gap from outside the
receiver's trust boundary, so delivery is gated by an operator-held allowlist
(`OTP_RECEIVER_CALLBACK_ALLOWED_HOSTS`) and redirects are not followed — a redirect would be the far
side choosing a second destination after the first had been checked. With no allowlist configured,
nothing is delivered anywhere; files still arrive, are verified, and can be downloaded from the UI.

---

## Measured on this run

| | colour-test.png | chime.wav |
|---|---|---|
| Original size | 4 920 B | 52 962 B |
| Compressed | 2 971 B (60%) | 52 962 B (100%, already compact) |
| Chunks | 2 | 28 |
| Frames rendered | 7 | 39 |
| Retransmissions | 0 | 0 |
| Chunks rebuilt from parity | 0 | 0 |
| Frames unreadable | 0 | 0 |
| Verified against sender's hash | yes | yes |
| Callback delivered | yes (HTTP 200) | yes (HTTP 200) |
| Throughput | 6.5 KB/s | 6.55 KiB/s |

The throughput figure is limited by the demonstration rather than by the protocol — see the
[README](../README.md#transfer-speed) for what sets it and what the same code does at other
settings.

## Three defects this demonstration found

Worth recording, because they are the reason a run like this is not optional.

**A verified transfer was reported as failed.** The retry ceiling counted every display of a chunk,
including keep-alive repeats. At thirty frames a second against a two-second acknowledgement poll, a
chunk gets repeated some sixty times before news of its arrival gets back — so the ceiling tripped
on a transfer that was working perfectly. Keep-alives are now excluded from the count: they are the
display filling time, not the sender trying again.

**Progress read 136%.** Parity shards are acknowledged like any other chunk, and the acknowledged
count included them while the total counted only source chunks. Parity is scaffolding that never
appears in the file, so it no longer counts toward how much of the file has arrived.

**A completed transfer went on reporting an outstanding chunk.** Clearing the outstanding list
passed an empty array to SQL, and an empty Go slice arrives as NULL — `chunk_number <> ALL(NULL)` is
NULL rather than true, so the delete matched nothing. Nothing-outstanding is now its own case.
