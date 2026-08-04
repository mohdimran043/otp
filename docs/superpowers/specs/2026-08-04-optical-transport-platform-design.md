# Optical Transport Platform — Design

**Date:** 2026-08-04
**Status:** Approved
**Source:** `docs/Optical Transport Platform.pdf`, `docs/Optical Transport Platform Marketing Website.pdf`

## Purpose

Transfer very large files across an air-gapped optical channel: the sender renders encoded
frames on a monitor, the receiver captures them with a camera. Acknowledgements travel
out-of-band through shared storage, never optically. Sender and receiver are fully
independent, separately deployable applications that share only a versioned protocol
definition.

## Scope decisions

Four decisions were settled before design:

1. **Virtual optical channel is the primary path**, with real hardware behind build-tagged
   adapters. The build environment has no camera, no display, and no OpenCV. A file-based
   channel plus a degradation simulator makes the entire pipeline testable; `OpenGLSink` and
   `GoCVCameraSource` implement the same interfaces for real deployments.
2. **Full API surface**, every endpoint the source documents list, backed by real jobs and
   real database writes. No stub handlers.
3. **FEC:** `none` and `reed-solomon` fully implemented. `raptorq` and `ldpc` are registered
   in the plugin registry returning `ErrNotImplemented` and documented as future work — the
   interface is proven, so adding them later is a drop-in.
4. **Marketing site ships after the platform works**, so its generated API reference and
   protocol documentation describe real code.

## Repository layout

The repository root *is* `optical-platform`. Three Go modules, four deployable units.

```
OpticalTransportProtocol/
├── shared/            Go module. Protocol only: no DB, no HTTP, no runtime.
├── sender/            Go module. Dockerfile, compose, Makefile, migrations, web/, .env
├── receiver/          Go module. Dockerfile, compose, Makefile, migrations, web/, .env
├── marketing-site/    Next.js. Dockerfile, compose, nginx.conf, .env
├── docs/              Source PDFs, generated protocol + API reference, benchmarks
└── AGENTS.md  README.md  Makefile
```

`sender` and `receiver` both `require` the shared module with a development `replace`
directive; Docker builds vendor `shared/` at image build time. Neither application imports
the other, and the root `Makefile` is orchestration only — never a runtime dependency.

## Optical protocol

### Frame layout

A frame is a raster image divided into cells of `cell_px` pixels.

```
┌─────────────────────────────────────────────────┐
│ ▓▓ finder    HEADER BAND (binary-modulated)  ▓▓ │  magic·version·flags·encoder·depth
│ ▓▓           SESSION · FRAME · TOTALS        ▓▓ │  txid·session·frame#·chunk#·len·ts
├─────────────────────────────────────────────────┤
│                                                 │
│         ADAPTIVE OPTICAL GRID (payload)         │  modulation per encoder
│                                                 │
├─────────────────────────────────────────────────┤
│ ▓▓  CRC32 │ SHA-256 │ RESERVED │ FOOTER     ▓▓  │  binary-modulated
└─────────────────────────────────────────────────┘
```

Neither QR nor Aztec. Structure:

- **Four corner finders**, 7×7 concentric squares, one carrying an orientation notch.
  Four corners (rather than QR's three) supply the point correspondences for a full
  homography, which is what makes real perspective correction possible.
- **Timing cells** alternate along the top and left edges between finders, letting the
  decoder recover cell pitch under arbitrary scaling.
- **Header and footer bands are always binary-modulated** with their own CRC16, regardless
  of payload modulation. This is the decision that makes the protocol adaptive: a decoder
  that knows nothing about an incoming frame can always read the header, learn the grid
  geometry, bit depth, and encoder id, and only then demodulate the payload.

### Header fields

Magic `OTP1`, protocol version, header length, flags, encoder id, bit depth, compression id,
FEC id, cell pixels, grid width, grid height, transmission id (UUID), session id, frame
number, total frames, chunk number, total chunks, payload length, timestamp, reserved bytes,
header CRC. Footer carries payload CRC32, payload SHA-256, reserved, and a footer magic.
Reserved fields and the explicit version field carry forward compatibility.

### Manifest frame

Frame 0 sets `FlagManifest` and carries filename, original size, compressed size, original
SHA-256, chunk count, chunk size, compression id, and FEC parameters. It is re-emitted every
N frames so a receiver can join a stream in progress.

### Decode pipeline (pure Go)

Grayscale conversion → per-block Otsu binarization → connected-component finder detection →
corner ordering via the orientation notch → four-point DLT homography → radial distortion
inverse map from the active calibration profile → per-cell k×k median sampling →
demodulation. No OpenCV dependency, and every stage is independently unit-testable.

### Plugins

All encoders implement `Encode`, `Decode`, `Validate`, `EstimateCapacity`.

| Encoders | Compression | FEC |
|---|---|---|
| `binary` — 1 bit/cell | `none` | `none` |
| `grayscale` — 2–3 bits/cell | `gzip` | `reed-solomon` (erasure-coded) |
| `color8` — 3 bits/cell | `lz4` | `raptorq` — registered, `ErrNotImplemented` |
| `color16` — 4 bits/cell | `zstd` | `ldpc` — registered, `ErrNotImplemented` |
| `rolling` — band-interleaved with band parity | `brotli` | |

Chunk payload size is derived from encoder capacity at the configured grid, so one chunk maps
to exactly one frame.

## Data flow

### Sender

`upload → save_file → compress → chunk → fec_encode → optical_encode → frame_generate →
save_frames → notify UI → display_session → transmit → ack_monitor → retransmit → finalize`

Uploaded files are never transmitted directly. Every transition is a persisted job row.

### Receiver

`capture → persist_frames → decode → crc_verify → store_chunk → ack_emit → merge →
sha_verify → callback → cleanup`

Captured frames are always written to disk before decoding — never decoded from memory. That
persistence is what makes `reprocess` possible: replaying a stored session offline under a
different decoder profile.

### Acknowledgement channel

Acknowledgements are not optical. The receiver writes `acks/<transmission_id>/<seq>.json`
atomically (temp file plus rename) to shared storage, signed with HMAC-SHA256. Each record
carries transmission id, frame number, chunk number, CRC status, timestamp, and retry count.
The sender watches the directory with fsnotify and a polling fallback, updates the database,
and feeds the scheduler.

### Scheduler

Priority queue over HIGH / NORMAL / LOW plus a sliding window of unacknowledged chunks with
configurable window size. Missing chunks always escalate to HIGH. The sender never pauses: if
the window is saturated and nothing needs retransmission, it re-displays the oldest
unacknowledged frame as keep-alive, so the display always shows the next available frame.

### Optical channel abstraction

`FrameSink` and `FrameSource` interfaces. The default `FileSink` → `FileCameraSource` pair
operates over a shared volume, with a degradation simulator applying gaussian blur, sensor
noise, perspective warp, JPEG artifacts, and rolling-shutter tearing — so the decoder is
proven against realistic input rather than pixel-perfect PNGs. `OpenGLSink` and
`GoCVCameraSource` sit behind `-tags opengl` and `-tags gocv`, selected by configuration.

## Job engine

A Postgres-backed worker pool, one per application:

- Claiming via `FOR UPDATE SKIP LOCKED`, safe across replicas.
- Typed handlers registered by job type.
- DAG dependencies: a job becomes runnable when all `depends_on` jobs complete.
- Retry with backoff and a maximum attempt count.
- Pause, resume, and cancel honored through context cancellation and a control channel.
- Progress percentage and message, per-job log lines, full history.
- Configurable concurrency.

Every operation in both pipelines runs as a job.

## Cross-cutting concerns

**Persistence.** pgx connection pool with versioned SQL migrations. Sender tables: `files`,
`chunks`, `encoded_frames`, `compression_profiles`, `encoding_profiles`, `transmissions`,
`display_sessions`, `callbacks`, `jobs`, `statistics`, `protocol_versions`. Receiver tables:
`capture_sessions`, `captured_frames`, `decoded_chunks`, `missing_chunks`, `merged_files`,
`callbacks`, `jobs`, `camera_profiles`, `decoder_profiles`, `statistics`,
`protocol_versions`. Both add `job_logs`, `users`, and `audit_logs`.

**Security.** JWT HS256 bearer tokens, roles admin / operator / viewer, RBAC middleware per
route, audit rows on every mutation. AES-256-GCM for optional payload encryption, SHA-256
integrity, HMAC-signed acknowledgements, TLS support in-process and at the proxy.

**Observability.** Zap structured logging with request ids. Prometheus `/metrics` exposing
real counters and histograms — frames rendered and decoded, bit error rate, acknowledgement
latency, job durations, HTTP latency. OpenTelemetry OTLP tracing, no-op when unconfigured.
`/health`, `/health/ready`, `/health/live`.

**Configuration.** YAML with environment overrides and fsnotify hot reload over a safe
subset: log level, worker concurrency, scheduler window, FPS, brightness, gamma.

## User interfaces

**Sender and receiver:** Vite, React, TypeScript, Material UI, Zustand, React Query. Sender
covers dashboard, upload, transmission queue, compression and encoding selection, grid
configuration, preview window, transmission speed, ETA, dropped frames, retry and
acknowledgement counts, statistics, history, logs. Receiver covers live camera, frame rate,
decode quality, perspective detection, frame preview, bit error rate, chunk and merge
progress, missing chunks, decode failures, stored RAW frames, session replay, statistics,
history, logs.

**Marketing site:** Next.js App Router, Tailwind, Framer Motion, react-three-fiber hero,
MDX documentation portal with a build-time search index, and a functional React dashboard
demo — not screenshots. Light and dark themes, WCAG AA, keyboard navigation. SEO through the
metadata API, JSON-LD, Open Graph, Twitter cards, sitemap, and robots. Analytics providers
(Google Analytics, Plausible, Microsoft Clarity) configurable behind a cookie consent gate.
Static export and standalone Docker builds both supported.

Its API reference and protocol specification are generated from Go source: OpenAPI 3.1 from
the route registry, protocol MDX from the protocol definitions.

## Verification strategy

1. **Unit** — protocol round-trips, all five encoders, all five compressors, Reed-Solomon
   erasure recovery, homography math, CRC and HMAC.
2. **Adversarial** — encode, degrade, decode; assert recovery and measure bit error rate.
3. **API integration** — real Postgres in Docker against a real Echo server, exercising every
   registered route with status, body, and database side-effect assertions. A
   router-coverage test fails the build if any registered route lacks a test, so "every API
   is tested" is enforced rather than asserted.
4. **End-to-end loopback** — sender and receiver over a shared volume via compose: upload a
   multi-megabyte file, run the virtual optical channel, assert the merged SHA-256 matches
   the original and that the callback reaches a test sink.
5. **Benchmarks** — measured throughput, per-encoder capacity, compression ratio, and frame
   rate written to `docs/BENCHMARKS.md`, which feeds the marketing site's performance charts
   with real numbers.
6. **Frontend** — Vitest, typecheck, and production build for all three sites.

`make test-all` at the repository root runs everything.

## Build order

| Phase | Deliverable |
|---|---|
| P0 | Shared protocol module, golden vectors, unit and adversarial tests |
| P1 | Sender backend, all APIs, integration tests |
| P2 | Sender React UI |
| P3 | Receiver backend, all APIs, integration tests |
| P4 | Receiver React UI |
| P5 | Dockerization, end-to-end loopback, benchmarks |
| P6 | Marketing website |
| P7 | Root README, AGENTS.md, CI examples, deployment guides |

Each phase completes before the next begins.

## Out of scope

- RaptorQ and LDPC codec implementations (interfaces registered, marked not implemented).
- RabbitMQ or any external broker; the internal Go job queue is sufficient.
- MinIO object storage; the filesystem-backed store implements the same interface, so MinIO
  is a later drop-in.
- Real camera and display hardware validation, which requires physical equipment.
