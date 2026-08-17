# The channel

How frames actually cross the gap: a shared directory, a display and a camera, or paper. Split out of the
README, which now carries the overview and links here.

## The channel: a directory, or actual light

By default the channel is light. The sender renders frames, displays them, and writes nothing to disk; the
receiver takes what a browser's camera posts. The only way across is to point that camera at the screen,
which is the point of the system — and the reason it is the default is that anything else lets a deployment
carry a file end to end without the optical path ever having been exercised.

The sender can instead write every rendered frame into the shared directory as a PNG, which a receiver reading
that directory picks up with no camera at all. That is the development channel and the offline round trip
below: fast, deterministic, and a second invisible path carrying the same bytes, so the air gap it was meant
to prove is not being tested while it is on.

**Settings → Transfer channel** switches between them.

```
OTP_SENDER_DISPLAY_SINK=none    # default: render and display only; nothing on disk
OTP_SENDER_DISPLAY_SINK=file    # write PNGs into DISPLAY_DIR

OTP_RECEIVER_CAPTURE_SOURCE=browser   # default: frames posted by a page holding the camera
OTP_RECEIVER_CAPTURE_SOURCE=camera    # a Video4Linux device, which a container needs passed in
OTP_RECEIVER_CAPTURE_SOURCE=file      # read the PNGs the sender's file sink wrote
```

The two go together: a sender on `none` and a receiver on `file` will sit there transferring nothing, because
nothing is being written for it to read.

Like the grid, the sink is read once at startup, so it **takes effect on the next restart** rather than live,
and the change is refused outright while any transfer is in flight.

Either **Settings → Transfer channel** or `.env` works: a change made in the UI is written down and laid back
over the configuration on the next start, so it survives the restart it needs. Stored settings win over `.env`
for the fields the settings page manages, which is what makes "I changed it in the UI" mean anything — the
corollary being that editing such a field in `.env` no longer wins once the UI has set it.

### Sending from a phone

A phone makes a perfectly good display — it is a bright, dense panel you can hold wherever the camera is — but
two things about it are not obvious, and both look like "it doesn't work" rather than like a setting.

**The geometry has to fit the phone's CSS viewport, which is about 390 px whichever way you hold it.** The frame
is square, so the limiting dimension is the short one, and rotating to landscape does not help. The display page
scales by whole multiples only and **crops rather than shrinks** a frame too large for the window — correct, since
shrinking blends neighbouring cells and the decoder takes the median of each cell — so an oversized frame means
the camera photographs a fragment and decodes nothing at all.

| Geometry | Frame | On a phone |
|---|---|---|
| 128×128 @ 8 px | 1056 px | **cropped** — camera sees a fragment |
| 128×128 @ 3 px | 396 px | marginal, and cropped on a 390 px viewport |
| **128×128 @ 2 px** | **264 px** | **fits, with margin** |

Two pixels a cell sounds far too small against the measured monitor configurations, and on a monitor it would
be. On a phone it is not: at a device pixel ratio of 3 those 264 CSS px are 792 *physical* pixels, so each cell
is about six real pixels of screen. What the camera resolves is physical pixels, not CSS ones.

**Fill at least half the camera's view with the screen.** This is the one that actually decides it, and the
threshold is sharper than it looks. Measured on a 1920×1080 webcam, square-on and in focus, against a 128×128
grid:

| Frame spans | Camera px per cell | Result |
|---|---|---|
| 28% of the short side | 2.3 | reaches the decoder, unreadable |
| 37% | 3.0 | unreadable |
| **46%** | **3.8** | decodes, every frame |
| 56% and up | 4.5+ | decodes, every frame |

So half the frame height is the floor and about two thirds is where you want to be, because everything real
costs margin: an angle, a smudge on the lens, glare off the screen, autofocus drifting. Square-on and sharp is
worth more than close.

There is a second, separate guard that can bite in a dark room. The receiver drops frames that do not look like
a frame at all — protection against storing thousands of images of a blank screen — by asking for at least a
twelfth of the image to be dark *and* a twelfth to be bright. A normally lit room passes this without
thinking about it, because walls and windows supply the bright end. A small screen in a dark room does not, and
the symptom is distinctive: frames are posted and "held", the decode count stays at zero, and nothing appears
under Decode failures either, because the frames never reached the decoder to fail.

### Start the camera before the transfer

**This ordering is not advice, it is a requirement for small transfers**, and getting it wrong looks like a
hang rather than a mistake.

The manifest — the frame carrying the filename, the size and the hash — is displayed first and then re-emitted
every 64 frames, specifically so a receiver arriving mid-stream can join. That is sound, but it races against
how fast the chunks get acknowledged. At 10 fps the next manifest is 6.4 seconds away, while a five-chunk
transfer has every chunk acknowledged inside about two. Once nothing is outstanding the display parks — the
keep-alive path re-shows the oldest *unacknowledged* chunk, and there isn't one — so the manifest is never
re-emitted. A camera that caught the chunks but missed the manifest has decoded everything, acknowledged
everything, and still cannot assemble a file, because it never learned what it was assembling. The sender sits
at `transmitting` with everything acknowledged, indefinitely.

The smaller the transfer, the worse the odds: the manifest interval is fixed while the time to acknowledge
everything shrinks with the chunk count.

**This no longer hangs.** Once the last chunk is acknowledged the manifest is the only thing the receiver can
still be missing, so it is what stays on screen while the sender waits to be told the file arrived — bounded by
the acknowledgement timeout, so a sender displaying to an empty room does not run forever, and ended
immediately by the receiver reporting the merge. A manifest that turns up long after the last chunk now
completes the transfer rather than being ignored. Verified with a camera made to drop every manifest frame for
45 seconds: the transfer finished with the manifest arriving 43.8 seconds after the last chunk.

Starting the camera first is still the better habit — it is quicker, since the manifest is the first frame
shown — but getting the order wrong now costs seconds rather than the whole transfer.

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

## Printing the frames

Alongside the frame archive, a transfer's frames can be downloaded as one PDF, a frame to a page:

```bash
curl -O -J localhost:1000/api/v1/transfers/<id>/frames/printable
```

or **Print as PDF** on the transfer page. The archive is for a machine — the receiver's import endpoint
replays it into the same pipeline a camera feeds. This is for a person: print the sheets, hold them up,
point a camera. It is the cheapest way to exercise the optical path with no display at all, and the only way
to make a capture reproducible, since a sheet of paper is the same frame every time where a panel's
brightness, refresh and viewing angle are not.

Each page is captioned with its sheet number, frame number and what it carries, because a stack of printed
QR codes is otherwise indistinguishable and the order matters for reading them back. The sheet number and the
frame number differ — the manifest re-emissions are dropped, since a person reads the sheets in order and
needs the manifest once — so a caption gives both.

Images are placed with interpolation off. It is the one setting that decides whether a printed frame is
readable: a reader that smooths the image as it scales spreads every cell into its neighbours, which is right
for a photograph and ruins a QR code.

Bounded at 500 sheets. A PDF records where every object begins in a table at the end, so it cannot be
streamed and is built whole in memory; past that the archive is the thing to reach for.

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
