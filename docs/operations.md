# Running it

Everything about keeping a deployment healthy: storage, retention, deletion, long runs, and the tests
that prove a change did what it claimed.

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
