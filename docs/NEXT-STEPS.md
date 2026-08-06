# Agreed next work, in order

Written at the end of the session that measured throughput, so the next one can start immediately.
The order is not arbitrary: steps 1 and 2 are prerequisites for the 50 MB test being meaningful at
all, and doing 3 before them would make the result worse rather than better.

## 1 · Fix the acknowledgement path — before anything else

Measured: 1 MB of incompressible payload took 292 s (3.4 KB/s) against a channel that allows
57 KB/s at the same settings. The receiver captured 5972 frames to receive 526 chunks and wrote an
acknowledgement file for every one, duplicates included; the shared volume ended with 8957 files
totalling 35.5 MB, thirty-five times the size of the file. The sender re-lists and re-verifies that
directory on a two-second poll. The 84 retransmissions in a run with no injected loss are the same
fault from the other side: the sender timing out on acknowledgements already written but not yet read.

Three changes, in this order:

1. **Do not write an acknowledgement for a duplicate chunk.** It tells the sender nothing it does not
   already know. 5972 files becomes 526.
2. **One append-only log per transmission** instead of a file per acknowledgement, so a scan is one
   read rather than thousands of opens. Keep the signature per record so a truncated tail is
   discarded rather than trusted.
3. **Remember applied records by offset**, so a scan reads only what is new.

Expected result: the measured rate should approach the channel bound (136 KB/s at the shipped
default), i.e. 100 MB in twelve to fifteen minutes, before any concurrency work.

## 2 · Pull frames over HTTP instead of a shared volume

The receiver should fetch frames from the sender rather than read a directory. It removes the shared
mount from the loop entirely, which is what makes a two-machine test possible over a network cable.

- **Sender:** `GET /api/v1/transfers/{id}/frames/{n}/image` already exists in spirit — add a
  long-polling `GET /api/v1/display/next?after={sequence}` that returns the frame currently on the
  display with its sequence number, so a receiver can follow the display without polling blindly.
- **Receiver:** a `Source` implementation named `http` alongside `file` and `gocv`. Everything
  downstream is unchanged, which is the point of that interface — persist, decode, acknowledge, merge,
  verify, deliver are identical whatever produced the image.
- Acknowledgements can then travel back over HTTP too (`POST /api/v1/acks`), signed with the same
  secret, which removes the shared volume from both directions.

**A consequence worth exploiting:** with frames fetched rather than photographed there are no optics,
so the operating envelope stops constraining cell size and modulation. `color16` at small cells is
free in this mode, which roughly doubles bytes per frame over `color8`.

## 3 · Defaults: densest encoding, best compression, panel-rate display

Only after 1 and 2. Proposed defaults for both sides:

| Setting | Value | Why |
|---|---|---|
| Encoding | `color16` | Four bits per cell, the densest available. Safe over HTTP; over real optics it needs a controlled installation, so the config page should say so. |
| Compression | `zstd` at level 9 | Measured 37.9% on structured data against brotli's 35.0% — brotli wins by three points and costs several times the CPU. On a 50 MB file that trade is not worth it. Keep `brotli` selectable for archival transfers where time does not matter. |
| Error correction | `reed-solomon`, 32 + 8 | Optimal at this block size; 25% parity for a lossless channel is generous and cheap to reduce. |
| Grid | 256×256 at 8 px | 22 669 bytes a frame. Needs a 4K panel; the config page should refuse it when the display cannot show it. |
| Frame rate | measured from the panel | See below. |

**Refresh rate.** The server has no display to interrogate in a container, so measure it where it can
be measured:

- The sender's config page times `requestAnimationFrame` intervals and reports the rate — 144 Hz
  shows as 6.94 ms — then offers to apply it.
- When a real display is attached, `xrandr --current` (or the OpenGL sink's own query) gives the
  authoritative figure at startup, and configuration overrides it.
- The applied rate must stay below what the receiver can decode, or every extra frame becomes a
  duplicate. After step 1 that ceiling is roughly 115–173 KB/s per core divided by the frame size;
  the config page should show both numbers side by side so the choice is informed rather than
  aspirational.

## 4 · Configuration handshake: do not transmit until the receiver has agreed

Requested explicitly, and the mechanism should mirror how everything else crosses the gap.

1. The sender publishes a **configuration announcement**: the settings that affect decoding — encoding,
   bit depth, grid, cell size, quiet zone, compression id, FEC parameters, encryption on or off —
   plus a hash over them, signed with the acknowledgement secret.
2. The receiver applies what it can, and answers with a **configuration acknowledgement** carrying the
   hash of the settings it actually applied.
3. The sender compares hashes. Equal: it may display. Unequal or absent: it stays in a new
   `awaiting-receiver` state and shows why, rather than transmitting frames the receiver will not
   read.
4. A settings change while a transmission is in flight is refused, not queued — the geometry is
   written into every frame header and the chunk size was derived from it.

The hash matters more than the field list: it means the two ends agree on the *whole* configuration
rather than on each field separately, and a version skew that adds a field is caught rather than
silently ignored.

## 5 · The 50 MB test, and its document

Once 1 to 4 are in place: transfer a 50 MB file over the HTTP transport, both sides in Docker, and
record in `docs/50MB-TRANSFER.md`:

- every configuration value in force on both sides, and the agreed configuration hash
- wall-clock time, throughput, chunk and frame counts, retransmissions, chunks rebuilt from parity
- the URLs to watch it live, with screenshots taken during the transfer rather than after
- the sender's declared SHA-256, the receiver's computed SHA-256, and the callback endpoint's
  independently recomputed SHA-256 — three figures that must be identical
- what limited the rate, measured rather than assumed

## 6 · Then the two-machine deployment

`sender/docker-compose.yml` and `receiver/docker-compose.yml` are already separate for this. What
changes for real hardware: the receiver's source becomes `gocv`, the sender's sink becomes `opengl`,
both behind build tags, and the frame rate comes from the panel. Everything between them — the
protocol, the pipeline, the acknowledgement contract, the UIs — is unchanged, which is the whole
reason the file-backed and HTTP paths exist.
