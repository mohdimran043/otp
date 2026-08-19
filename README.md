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

Inside it, **grid and cell size are both per transfer**: presets from 64×64 to 512×512, cells of 1 to 8
pixels, and either can be left on **"Auto — fit my screen,"** computed in your browser from the space the
Display page will actually give a frame — the panel less the browser's own furniture, less the room the page
keeps for its caption, divided between the lanes.

Only grids that screen can show are offered. One that cannot be displayed at any offered cell size renders a
frame the Display page pins at 1× and hangs off the edge, since it scales by whole numbers and will not
resample a cell across a fractional boundary to make it fit.

Fixing the grid sizes the cell for it, and *not* by taking the largest that fits — that is usually the
smaller picture as well as the slower one. An 80-cell grid at 8 px renders 672 and is shown at 672 on a 1080
panel, because twice 672 does not fit; at 4 px it renders 336 and is shown at 1008. Same picture to a camera,
a quarter of the pixels to draw and compress. Leaving both on Auto takes the largest grid the encoding can
still be read at, then sizes its cell the same way. The deployment-wide values in `.env` remain the
default for any transfer that does not choose.

## What it looks like

<table>
<tr>
<td width="50%"><img src="docs/screenshots/01-sender-send-form.png" alt="The send form"><br><sub><b>Send</b> — a file and a callback, everything else defaulted to a measured setting.</sub></td>
<td width="50%"><img src="docs/screenshots/13-sender-display-live.png" alt="A frame on the display"><br><sub><b>Display</b> — one frame at a time, scaled by a whole number so no cell is resampled.</sub></td>
</tr>
<tr>
<td><img src="docs/screenshots/09-receiver-live-capture.png" alt="Frames arriving"><br><sub><b>Receiving</b> — frames as they arrive, each one clickable for what happened to it.</sub></td>
<td><img src="docs/screenshots/24-receiver-live-camera-waiting.png" alt="Aiming a camera"><br><sub><b>Aiming</b> — the aiming display grades the shot and says which way to move.</sub></td>
</tr>
<tr>
<td><img src="docs/screenshots/03-optical-frame-color8.png" alt="A colour frame"><br><sub><b>A frame</b> — fiducials, timing columns, header and footer bands, colour payload.</sub></td>
<td><img src="docs/screenshots/05-camera-capture-degraded.png" alt="A degraded capture"><br><sub><b>A real capture</b> — blurred, off-square, unevenly lit, and still readable.</sub></td>
</tr>
</table>

More in [a transfer, end to end](docs/DEMONSTRATION.md).

## What it does, and where the detail lives

The README stopped being readable somewhere past six hundred lines, so the long-form explanation moved into
documents beside it. Each one is a topic an operator actually goes looking for, rather than a chapter of a
manual nobody reads front to back.

| | |
|---|---|
| **[The channel](docs/channel.md)** | The three ways frames cross the gap — a shared directory, a display and a camera, or paper — and how to aim a camera at one. |
| **[Transfer speed](docs/performance.md)** | What this moves in practice, and the one setting that decides it. |
| **[Production camera setup](docs/production-camera.md)** | Specifying real hardware, and the arithmetic that says whether a camera can read a geometry before you buy it. |
| **[Print and scan](docs/PRINT-AND-SCAN.md)** | What one A4 sheet holds, measured — and which geometries survive being printed and read back. |
| **[Security](docs/security.md)** | The four encryption modes, including certificates, and what an air gap does not buy you. |
| **[Running it](docs/operations.md)** | Storage, retention, deletion, long runs, and the tests. |
| **[Deploying it](deploy/README.md)** | The proxy, TLS, and the admin consoles. |
| **[Optimal configuration](docs/OPTIMAL-CONFIG.md)** | The settings that reach 1 MB/s, and what they ask of the panel and the camera. |


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

## How it is put together

```
shared/     Protocol only: frame format, encodings, compression, error correction. No DB, no HTTP.
sender/     Go + React. Compresses, chunks, codes, renders, displays, watches for acknowledgements.
receiver/   Go + React. Captures, decodes, acknowledges, merges, verifies, delivers.
```

The two applications **do not import each other**. They share a protocol, a directory, and nothing else —
which is what lets either be restarted, upgraded or replaced while the other keeps running. Each exposes a
`harness` package so an out-of-tree test can drive a whole side without reaching into its internals.

## What this implements, and where it comes from

Almost none of this is novel. The frame layout, the codes, the demodulation and the cryptography are
published work, and the tables below say which paper or specification each part came from and which file
holds it. Three kinds of entry, kept apart because the distinction matters when something misbehaves:

- **Specification** — the document was implemented here, and a test checks this code against its published values.
- **Paper** — the technique was taken from the work and written here; the wire format is this project's own.
- **Dependency** — the algorithm belongs to a library, and the citation says what that library implements.

### Erasure coding — `shared/fec/`

| Work | Here |
|---|---|
| [**RFC 6330**](https://www.rfc-editor.org/rfc/rfc6330.html) — *RaptorQ Forward Error Correction Scheme for Object Delivery*. Luby, Shokrollahi, Watson, Stockhammer & Minder, 2011 | **Specification.** [`raptorq.go`](shared/fec/raptorq.go) and [`raptorq_tables.go`](shared/fec/raptorq_tables.go): the systematic indices of Table 2, the §5.5 random tables, the §5.3.5.2 degree distribution, the §5.3.3.3 extended source block. One deliberate departure — the RFC's inactivation schedule is replaced by Gaussian elimination over GF(256), which is exactly why a block is capped at 1024 shards rather than the specification's 56403. |
| [**RFC 6330 §5.7**](https://www.rfc-editor.org/rfc/rfc6330.html#section-5.7) — GF(256) arithmetic | **Specification.** [`gf256.go`](shared/fec/gf256.go). The `OCT_EXP`/`OCT_LOG` tables are generated from the primitive polynomial `0x11D` rather than transcribed — a computed table cannot be mistyped — and `TestGF256MatchesRFC` checks them against the published values. |
| [Luby, *LT Codes*, FOCS 2002](https://doi.org/10.1109/SFCS.2002.1181950) | **Paper.** The fountain property RaptorQ inherits and `TestRaptorQRecoversFromRepairSymbolsAlone` asserts: repair symbols are unlimited, any K + 2 of them rebuild the block, and no particular symbol is ever needed. On a link where the sender is still displaying while the receiver is still reporting, that is the difference between negotiating which parity to send and simply sending more. |
| [Shokrollahi, *Raptor Codes*, IEEE Trans. Inf. Theory 52(6), 2006](https://doi.org/10.1109/TIT.2006.874390) | **Paper.** The pre-code-plus-LT construction that RFC 6330 standardises, including the dense GF(256) HDPC rows that give the code its `256^-e` failure tail. |
| [**RFC 5170**](https://www.rfc-editor.org/rfc/rfc5170.html) — *LDPC Staircase and Triangle FEC Schemes*. Roca, Neumann & Furodet, 2008 | **Paper.** [`ldpc.go`](shared/fec/ldpc.go) takes the staircase parity structure — parity shard *i* constrained together with *i−1*, so encoding is one sequential pass rather than a matrix multiply — but not the RFC's wire format. Column weight is 5 rather than the textbook 3, measured: at 32 source and 32 parity shards, 3 checks per shard recovers 50% of the loss patterns that leave 35 shards, where 5 recovers 76%. |
| [Gallager, *Low-Density Parity-Check Codes*, IRE Trans. Inf. Theory 8(1), 1962](https://doi.org/10.1109/TIT.1962.1057683) | **Paper.** The sparse-graph code and its iterative decoder. On an erasure channel belief propagation collapses to a single rule — a check with exactly one unknown resolves it — which is `ldpcPropagate`, followed by an exact solve for whatever propagation could not reach. |
| [Reed & Solomon, *Polynomial Codes Over Certain Finite Fields*, J. SIAM 8(2), 1960](https://doi.org/10.1137/0108018) | **Dependency.** [`simple.go`](shared/fec/simple.go) over [`klauspost/reedsolomon`](https://github.com/klauspost/reedsolomon). `TestReedSolomonIsOptimal` checks the MDS property that is the whole reason to choose it: *any* k of the n shards suffice, with no margin. |
| [Rizzo, *Effective Erasure Codes for Reliable Computer Communication Protocols*, ACM SIGCOMM CCR 27(2), 1997](https://doi.org/10.1145/263876.263881) | **Dependency.** The systematic-Vandermonde software construction the library descends from, and the reason the field caps a Reed–Solomon block at 256 shards while LDPC and RaptorQ have no such ceiling. |

### Reading a frame the decoder rejected — `receiver/ai/`

| Work | Here |
|---|---|
| [Chase, *A Class of Algorithms for Decoding Block Codes with Channel Measurement Information*, IEEE Trans. Inf. Theory 18(1), 1972](https://doi.org/10.1109/TIT.1972.1054746) | **Paper.** [`soft/recover.go`](receiver/ai/soft/recover.go) is Chase-II on the symbol layer: keep the distance from each sampled cell to its runner-up palette entry ([`Palette.ValueWithMargin`](shared/encoding/palette.go)), rank the payload by it, take the twelve least reliable and enumerate the 2¹² test patterns. What makes it safe rather than merely plausible is the acceptance test — the footer carries **both** a CRC32 and a SHA-256, so a false accept needs a simultaneous collision in both. |
| [Duffy, Li & Médard, *Capacity-Achieving Guessing Random Additive Noise Decoding*, IEEE Trans. Inf. Theory 65(7), 2019](https://doi.org/10.1109/TIT.2019.2896110) | **Paper.** The other half of that search. Candidates are dequeued from a heap in increasing cost order rather than enumerated blindly, which is GRAND's guess-the-noise-in-likelihood-order with the footer's checksums standing in for codebook membership. `Options.MaxCandidates` and `Options.Budget` are where the query budget is set — 4096 and 50 ms by default. |
| [Ramsey, *Realization of Optimum Interleavers*, IEEE Trans. Inf. Theory 16(3), 1970](https://doi.org/10.1109/TIT.1970.1054443) · [Forney, *Burst-Correcting Codes for the Classic Bursty Channel*, IEEE Trans. Comm. 19(5), 1971](https://doi.org/10.1109/TCOM.1971.1090705) | **Paper.** Why the `rolling` encoding exists. A rolling-shutter tear is a burst, and row-major placement is the worst possible arrangement for one; [`rolling.go`](shared/encoding/rolling.go) interleaves consecutive payload bits across bands, so the same tear removes every *N*th bit instead of a contiguous run — the loss pattern erasure coding handles best rather than worst. |

### The screen-to-camera channel

| Work | Here |
|---|---|
| [**ISO/IEC 18004**](https://www.iso.org/standard/83389.html) — QR code symbology | **Paper**, in that the layout borrows from it without being compatible with it: the 7×7 ring-inside-ring fiducial ([`finder.go`](shared/protocol/finder.go)), alternating timing columns, the quiet zone, and a header written several times over and resolved by majority vote. One thing is deliberately *not* taken — all four corners carry the identical pattern, because a distinctive corner is the first distinction blur destroys. Orientation is instead resolved by trying all eight hypotheses and letting a CRC settle it. |
| [Perli, Ahmed & Katabi, *PixNet: Interference-Free Wireless Links Using LCD-Camera Pairs*, MobiCom 2010](https://doi.org/10.1145/1859995.1860012) | **Context.** The display-camera pair treated as a communication channel with characteristic distortions — perspective and blur — rather than as a barcode scanned slowly. This platform answers both, but with a homography and a per-frame photometric fit where PixNet generalises OFDM. |
| [Hao, Zhou & Xing, *COBRA: Color Barcode Streaming for Smartphone Systems*, MobiSys 2012](https://doi.org/10.1145/2307636.2307645) | **Paper.** Colour barcodes streamed rather than scanned, and — taken directly — a palette laid out for what blur does to it. [`palette.go`](shared/encoding/palette.go) maximises the *minimum* distance between any two entries, since the closest pair is where a noisy sensor first confuses symbols and every other pair is irrelevant until it does. It is why `color16` separates hue from luminance instead of using the cube corners at two brightnesses, which collide. |
| [Hu, Gu & Pu, *LightSync: Unsynchronized Visual Communication over Screen-Camera Links*, MobiCom 2013](https://doi.org/10.1145/2500423.2500437) | **Context.** The rolling-shutter and screen-camera synchronisation problem, named and measured. The answer here is to not synchronise at all: acknowledgement is out of band, so a frame is simply redisplayed until it is acknowledged, and the tear is handled by interleaving instead. |
| [Hu, Mao, Huang, Xue, She, Bian & Shen, *Strata: Layered Coding for Scalable Visual Communication*, MobiCom 2014](https://doi.org/10.1145/2639108.2639132) | **Context.** Graceful degradation as distance and angle worsen. Handled here at the other end — [`shared/readable`](shared/readable/readable.go) computes the pixels per cell a capture can ever produce and refuses a geometry outright, since a floor is what a refusal should rest on. |
| [Tran, Jayatilaka, Ashok & Misra, *DeepLight: Robust & Unobtrusive Real-time Screen-Camera Communication*, IPSN 2021](https://doi.org/10.1145/3412382.3458269) | **Context.** A learned decoder for a screen-camera link. [`receiver/ai/`](receiver/ai/README.md) does something narrower on purpose: a small CNN over a 9×9 patch spanning one and a half cells, classifying one symbol at a time. At four pixels a cell the neighbours bleed into the centre, so the sampled colour is a mixture — which a learned function can undo and a distance metric on a single sample cannot, however good the palette. |

### Locating and demodulating a frame — `shared/protocol/`, `shared/encoding/`

| Work | Here |
|---|---|
| [Hartley & Zisserman, *Multiple View Geometry in Computer Vision*, 2nd ed., CUP 2004 — §4.1, the DLT](https://www.robots.ox.ac.uk/~vgg/hzbook/) | **Paper.** Four point correspondences determine a projective transform up to scale; [`HomographyFromQuad`](shared/protocol/geometry.go) fixes the scale by setting `h[8] = 1`, which leaves eight unknowns in the eight equations the four correspondences contribute. |
| [Brown, *Decentering Distortion of Lenses*, Photogrammetric Engineering 32(3), 1966](https://www.semanticscholar.org/paper/Decentering-distortion-of-lenses-Brown/2ef001c656378a1c5cf80488b35684742220d3f9) | **Paper.** [`protocol.Distortion`](shared/protocol/geometry.go) is Brown–Conrady exactly: two radial coefficients K1, K2 and two tangential P1, P2, normalised against half the image diagonal so both stay near unity magnitude. Applied *forward* — grid cell through the homography, then displaced by the lens model — because undistorting the whole image would cost a full resample per frame and gain nothing. |
| [Zhang, *A Flexible New Technique for Camera Calibration*, IEEE TPAMI 22(11), 2000](https://doi.org/10.1109/34.888718) | **Context.** Where a deployment's K1…P2 come from, and why the zero value is the right default for a file replay or a screen capture. |
| [Bradley & Roth, *Adaptive Thresholding using the Integral Image*, J. Graphics Tools 12(2), 2007](https://doi.org/10.1080/2151237X.2007.10129236) | **Paper.** [`Binarize`](shared/protocol/bitmap.go) is block-adaptive mean thresholding: 16×16 tiles, each tile's own mean as its threshold, smoothed over a 3×3 neighbourhood, with a contrast gate so a flat tile does not turn sensor noise into speckle. A single global threshold fails on a photographed display, which is unevenly lit by construction. |
| [Rosenfeld & Pfaltz, *Sequential Operations in Digital Picture Processing*, JACM 13(4), 1966](https://doi.org/10.1145/321356.321357) | **Paper.** [`LabelComponents`](shared/protocol/bitmap.go), eight-connected. It is what makes fiducial detection structural rather than statistical: a finder is a bright ring whose bounding-box centre is *also* bright but belongs to a different component. Only a ring inside a ring does that, so ordinary bright blobs are rejected without tuning a threshold. |
| [Rec. ITU-R BT.601](https://www.itu.int/rec/R-REC-BT.601) | **Specification.** The luma weights 0.299 / 0.587 / 0.114, used twice — flattening a capture to a [`GrayMap`](shared/protocol/bitmap.go), and weighting palette distance in [`Palette.Value`](shared/encoding/palette.go), because a sensor resolves green far better than blue and an unweighted distance throws that discriminating power away. |

### Cryptography

| Work | Here |
|---|---|
| [**FIPS 197**](https://doi.org/10.6028/NIST.FIPS.197-upd1) — AES · [**NIST SP 800-38D**](https://doi.org/10.6028/NIST.SP.800-38D) — GCM ([McGrew & Viega, INDOCRYPT 2004](https://doi.org/10.1007/978-3-540-30556-9_27)) | **Dependency.** AES-256-GCM in [`crypt.go`](shared/protocol/crypt.go), via the Go standard library. |
| [**RFC 8439**](https://www.rfc-editor.org/rfc/rfc8439.html) — *ChaCha20 and Poly1305 for IETF Protocols* ([ChaCha](https://cr.yp.to/chacha/chacha-20080128.pdf), [Poly1305](https://cr.yp.to/mac/poly1305-20050329.pdf), Bernstein) | **Dependency.** The second cipher, via `golang.org/x/crypto/chacha20poly1305`. The cipher ID travels in every frame header, so a receiver is never configured to match a sender. |
| [**SEC 1 v2.0**](https://www.secg.org/sec1-v2.pdf) — ECIES · [**NIST SP 800-56A Rev. 3**](https://doi.org/10.6028/NIST.SP.800-56Ar3) — ECDH | **Paper.** [`certcrypt.go`](shared/protocol/certcrypt.go) is an ECIES-shaped key wrap over P-256: `ECDH(sender priv, receiver pub)` → HKDF-SHA256 salted with the transmission id → AES-256-GCM sealing a per-transfer key. Confidentiality and authenticity are the same ECDH result read two ways, not a signature bolted on. P-256 rather than RSA for the frame budget: the wrapped key rides in *every* frame so that a frame stays independent, and 60 bytes a frame is affordable where an RSA-2048 block is not. |
| [**RFC 5869**](https://www.rfc-editor.org/rfc/rfc5869.html) — HKDF ([Krawczyk, CRYPTO 2010](https://eprint.iacr.org/2010/264)) | **Dependency.** The extract-and-expand step above, via `golang.org/x/crypto/hkdf`. |
| [**RFC 2104**](https://www.rfc-editor.org/rfc/rfc2104.html) — HMAC ([Bellare, Canetti & Krawczyk, CRYPTO 1996](https://doi.org/10.1007/3-540-68697-5_1)) | **Dependency.** Every acknowledgement is HMAC-SHA256 signed ([`ack.go`](shared/protocol/ack.go)). The ack directory is the one input the sender takes from outside itself: anything able to write it could otherwise report a chunk that never arrived, truncating the transfer, or report that everything failed, making it retransmit for ever. |
| [**FIPS 180-4**](https://doi.org/10.6028/NIST.FIPS.180-4) — SHA-256 | **Dependency.** The transfer's ultimate success criterion, and half of the recovery search's acceptance test. |

### Compression — `shared/compress/`

All four are **dependencies**, selected per transfer and named in the manifest so the receiver runs the
right one in reverse: [**RFC 1951** DEFLATE](https://www.rfc-editor.org/rfc/rfc1951.html)
([Ziv & Lempel 1977](https://doi.org/10.1109/TIT.1977.1055714),
[Huffman 1952](https://doi.org/10.1109/JRPROC.1952.273898)) ·
[**RFC 8878** Zstandard](https://www.rfc-editor.org/rfc/rfc8878.html)
([Duda, *Asymmetric Numeral Systems*, 2013](https://arxiv.org/abs/1311.2540)) ·
[**RFC 7932** Brotli](https://www.rfc-editor.org/rfc/rfc7932.html) · LZ4. Decompression is bounded by the
manifest's declared size, because every one of them can express a small input that expands without limit.

### Unidirectional delivery

| Work | Here |
|---|---|
| [**RFC 6726**](https://www.rfc-editor.org/rfc/rfc6726.html) — *FLUTE: File Delivery over Unidirectional Transport*, 2012 · [**RFC 5775**](https://www.rfc-editor.org/rfc/rfc5775.html) — *Asynchronous Layered Coding* | **Context, and the closest standardised analogue.** The manifest in [`manifest.go`](shared/protocol/manifest.go) is re-emitted periodically rather than sent once, which is FLUTE's FDT on a carousel: a receiver whose camera came online mid-transmission joins the stream instead of waiting for the next one. For a file that takes an hour to display, that is the difference between a working installation and an unusable one. |
| [**RFC 5052**](https://www.rfc-editor.org/rfc/rfc5052.html) — *FEC Building Block* · [**RFC 3453**](https://www.rfc-editor.org/rfc/rfc3453.html) — *The Use of FEC in Reliable Multicast* | **Context.** The separation these keep between a codec and the transport carrying its shards is the one [`fec.Blocking`](shared/fec/blocking.go) enforces: a frame header numbers chunks across the whole transmission, a codec numbers shards within a block, and the translation between them is written once and used from both sides. |

**Where this departs from all of it.** Every scheme above delivers over a channel that is either
bidirectional or accepted as lossy — FLUTE in particular is defined to have no feedback at all. This one is
neither. Light goes one way and acknowledgements come back over a shared directory, so a chunk is
redisplayed until a *signed* acknowledgement for it arrives, and error correction is an optimisation on top
rather than the guarantee. That inversion — ARQ over an out-of-band return path, FEC as the thing that
makes it cheaper — is what the rest of the design follows from, and it is not any of these papers'.

### Not from anywhere

Four things here have no citation because they are this project's own, and it seems worth saying which:

- **The per-frame photometric fit** ([`photometry.go`](shared/encoding/photometry.go)) — a plane plus a radial term per RGB channel, fitted from the fiducials and timing columns, which are the cells whose values are known before the payload is read. It needs no calibration target because every frame already carries one. What it cannot undo is gamma: two reference levels fix a line, and a power curve needs a third point to be observable.
- **The four-corner descriptor block** ([`descriptor.go`](shared/protocol/descriptor.go)) — a receiver cannot read the header until it knows the grid width, and the header band's position depends on it. Stating the geometry outright in a small CRC-checked block beside each fiducial breaks that circle, rather than extrapolating a seven-cell fiducial across a grid hundreds of cells wide.
- **Lane tiling** ([`lanes.go`](shared/protocol/lanes.go)) — several independent frames at once. It buys no pixels per cell (measured: 0.92–0.96 of a single grid, slightly *worse*); what it buys is independence, so a reflection or a passing hand costs one lane rather than the whole frame. It pairs with RaptorQ specifically, where surviving lanes advance the transfer as much as if the spoiled one had never been sent.
- **The out-of-band signed acknowledgement channel** ([`ack.go`](shared/protocol/ack.go)) — described above, and the reason the two applications share a protocol and a directory and no code path at all.

### Adjacent, and deliberately not implemented

The other literature on moving data across an air gap is the covert one: Guri et al. on optical exfiltration
via [router and switch LEDs](https://arxiv.org/abs/1706.01140) and
[security-camera infrared](https://arxiv.org/abs/1709.05742). Same physics, opposite intent — those channels
are built to go unnoticed and carry bits per second, where this one is built to be pointed at and moves
megabytes per second with a hash at the end saying whether it worked. None of it is implemented here, but
the threat it describes is the one [`docs/security.md`](docs/security.md) names: an optical channel is a
broadcast, so a second camera in the room decodes the file as easily as the intended receiver does.

## Documentation

- [A transfer, end to end](docs/DEMONSTRATION.md) — screenshots of the whole path
- [Technical overview (PDF)](docs/optical-transport-overview.pdf) — twelve pages for a
  non-specialist reader: the setup, speed at each end, what a chunk is, how acknowledgement works, and how
  it would scale out
- [Optimal configuration](docs/OPTIMAL-CONFIG.md) — every measured figure and what to set
