import { videoConstraints, type CaptureDetail } from './videoConstraints'
// The browser's camera, owned outside React.
//
// It has to live here rather than in a component, and the reason is the bug this fixes: the stream was held by
// the Settings page, so navigating to Live capture unmounted the component and released the camera. Which is the
// worst possible moment to release it — Live capture is the page you go to *because* you just started the camera
// and want to watch frames arrive.
//
// So the MediaStream, the canvas, the posting timer and the counters live in module scope. A component subscribes
// to be re-rendered and unsubscribes when it goes away; the camera is unaffected either way. It stops when Stop
// is pressed, or when the tab is closed, and at no other time.

export interface CameraState {
  running: boolean
  /** The track's own label, which is the camera's name as the browser knows it. */
  label: string
  /** Frames posted, frames that held a decodable grid, and frames that showed nothing. */
  sent: number
  accepted: number
  idle: number
  /** Frames dropped before posting because they were blurrier than what this camera has been managing. */
  blurred: number
  /** Sharpness of the last frame as a fraction of the best seen recently, 0..1. */
  steadiness: number
  error: string | null
  /** The resolution actually granted, which is not always the resolution asked for. */
  width: number
  height: number
}

const initial: CameraState = {
  running: false,
  label: '',
  sent: 0,
  accepted: 0,
  idle: 0,
  blurred: 0,
  steadiness: 0,
  error: null,
  width: 0,
  height: 0,
}

let state: CameraState = initial
let stream: MediaStream | null = null

/**
 * grabber is the element frames are drawn from, owned here and never unmounted.
 *
 * A hidden element of our own rather than the one a component renders, because the component's element goes away
 * when an operator navigates to another page — and drawing from it was how the capture stopped. This one exists
 * for as long as the tab does, so posting continues whichever page is open, or none.
 */
let grabber: HTMLVideoElement | null = null

/** preview is the component's element, when one is mounted. It only shows; it is never drawn from. */
let preview: HTMLVideoElement | null = null
let canvas: HTMLCanvasElement | null = null
let timer: number | null = null
let posting = false

const listeners = new Set<() => void>()

function emit(patch: Partial<CameraState>) {
  state = { ...state, ...patch }
  listeners.forEach((listener) => listener())
}

/** subscribe registers a component to re-render on change, and returns its unsubscribe. */
export function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

/** snapshot is the current state, for useSyncExternalStore. */
export function snapshot(): CameraState {
  return state
}

/**
 * attach hands the component's <video> element the live stream.
 *
 * Called on every mount, because the element is new each time even though the stream is not. This is what lets
 * the preview reappear when an operator navigates back to the page while the camera has been running all along.
 */
export function attach(element: HTMLVideoElement | null) {
  preview = element
  if (element && stream) {
    element.srcObject = stream
    void element.play().catch(() => {
      // Autoplay refused without a gesture on some browsers. Cosmetic only: the posting loop draws from the
      // hidden element, not this one.
    })
  }
}

/** ensureGrabber creates the hidden element that frames are drawn from. */
function ensureGrabber(): HTMLVideoElement {
  if (!grabber) {
    grabber = document.createElement('video')
    grabber.muted = true
    grabber.playsInline = true
    grabber.setAttribute('aria-hidden', 'true')
    grabber.style.position = 'fixed'
    grabber.style.width = '1px'
    grabber.style.height = '1px'
    grabber.style.opacity = '0'
    grabber.style.pointerEvents = 'none'
    document.body.appendChild(grabber)
  }
  return grabber
}

/** postRate is how many frames a second are considered for sending. */
const postRate = 10

/**
 * Blur rejection, for a camera held in a hand.
 *
 * A phone held at arm's length is never still. Most frames are fine and a steady minority are smeared by the
 * movement between the shutter opening and closing, and a smeared frame is worse than no frame at all: it costs
 * an upload, a store, a decode and a row in the failure log, and it can never be read, because motion blur takes
 * a cell's colour and mixes it with its neighbours' before the sensor ever sees it. Nothing downstream recovers
 * that.
 *
 * They are cheap to spot, though. Blur removes high spatial frequencies and almost nothing else, so the total
 * gradient energy of a frame collapses when it is smeared and returns the instant it is not. Comparing that
 * against what this camera has recently managed — rather than any fixed number — is what makes it work across
 * different phones, distances and lighting, none of which can be known in advance.
 *
 * Which is why this now defaults to *off*.
 *
 * The paragraph above was written before the receiver could recover a frame. It is still true that motion blur
 * mixes a cell with its neighbours before the sensor sees it — but "nothing downstream recovers that" is no
 * longer the case. Mixing with neighbours is precisely what the learned cell classifier is trained to undo: it
 * reads a patch spanning one and a half cells rather than one averaged sample, and measured on real captures it
 * recovers twice as many frames as the arithmetic search alone.
 *
 * So a blurred frame is now a candidate, not waste, and the cost of dropping one has inverted. Dropping it
 * saves an upload and a decode; keeping it may be the frame that carries a chunk nothing else delivered. The
 * decision belongs on the server, which can see what recovery achieved, and not in a browser guessing.
 *
 * The measurement stays and the counter stays, because "how steady is this camera" is genuinely useful to show
 * an operator. Only the rejection is gone. Set a floor above zero to bring it back — on a channel with recovery
 * disabled, or a link too slow to carry every frame, it is still the right trade.
 */

/**
 * sharpnessFloor is the fraction of recent best sharpness a frame must reach to be worth posting. Zero posts
 * everything and lets the server decide, which is the default now that the server can recover blur.
 */
let sharpnessFloor = 0

/** setSharpnessFloor adjusts the blur gate at run time. Zero disables it. */
export function setSharpnessFloor(fraction: number): void {
  sharpnessFloor = Number.isFinite(fraction) && fraction > 0 ? fraction : 0
}

/**
 * sharpnessDecay lets the reference fall when the scene genuinely changes, so moving to a new position does not
 * leave the camera permanently comparing against a sharpness it can no longer reach. At ten frames a second this
 * halves the reference in about two seconds.
 */
const sharpnessDecay = 0.97

/** measureWidth is the width the frame is reduced to before measuring. */
const measureWidth = 320

let measureCanvas: HTMLCanvasElement | null = null
let bestSharpness = 0

/**
 * sharpness returns the mean squared gradient of a frame, reduced first for speed.
 *
 * Measured on a small copy rather than the full frame: blur is a low-frequency property and survives the
 * reduction, while the measurement cost falls by two orders of magnitude. Returns 0 when it cannot measure,
 * which the caller must read as "no opinion" rather than "blurred" — refusing to post frames because the
 * measurement failed would be a worse failure than the one it prevents.
 */
function sharpness(source: CanvasImageSource, width: number, height: number): number {
  if (width === 0 || height === 0) return 0
  if (!measureCanvas) measureCanvas = document.createElement('canvas')

  const w = measureWidth
  const h = Math.max(1, Math.round((height / width) * w))
  measureCanvas.width = w
  measureCanvas.height = h

  const ctx = measureCanvas.getContext('2d', { willReadFrequently: true })
  if (!ctx) return 0
  ctx.drawImage(source, 0, 0, w, h)

  let data: Uint8ClampedArray
  try {
    data = ctx.getImageData(0, 0, w, h).data
  } catch {
    // Tainted canvas. Not possible for a same-origin camera stream, but the failure mode if it ever
    // happened would be silently posting nothing, so it is handled rather than assumed away.
    return 0
  }

  // Green alone stands in for luminance here: it carries most of it, and it is the channel a Bayer sensor
  // samples twice as often as the others, so it is both the cheapest and the least noisy choice.
  let total = 0
  for (let y = 1; y < h; y++) {
    for (let x = 1; x < w; x++) {
      const i = (y * w + x) * 4 + 1
      const dx = data[i]! - data[i - 4]!
      const dy = data[i]! - data[i - w * 4]!
      total += dx * dx + dy * dy
    }
  }
  return total / ((w - 1) * (h - 1))
}

async function postOneFrame() {
  // One request at a time. Without this a slow post overlaps the next tick and the backlog grows until the
  // browser is posting frames from several seconds ago.
  if (posting || !stream) return

  const track = stream.getVideoTracks()[0]
  if (!track || track.readyState !== 'live') return

  const source = ensureGrabber()
  if (source.videoWidth === 0) return

  posting = true
  try {
    if (!canvas) canvas = document.createElement('canvas')

    const width = source.videoWidth
    const height = source.videoHeight

    canvas.width = width
    canvas.height = height
    const context = canvas.getContext('2d')
    if (!context) return
    context.drawImage(source, 0, 0, width, height)

    // Drop what hand shake has smeared, before it costs an upload and a decode.
    //
    // The reference is what this camera has managed in the last couple of seconds, not a constant: how sharp a
    // sharp frame is depends on the phone, the distance and the light, and none of those are known here. A
    // measurement of zero means the measurement failed, and is deliberately allowed through — declining to post
    // because the blur check broke would be a worse fault than the blur.
    const sharp = sharpness(canvas, width, height)
    if (sharp > 0) {
      bestSharpness = Math.max(sharp, bestSharpness * sharpnessDecay)
      const steadiness = bestSharpness > 0 ? sharp / bestSharpness : 1
      if (steadiness < sharpnessFloor) {
        emit({ blurred: state.blurred + 1, steadiness })
        return
      }
      emit({ steadiness })
    }

    // JPEG rather than PNG: a PNG of a 1080p frame is megabytes, and the artefacts at this quality are far inside
    // what the decoder tolerates — the optical envelope budgets for a lens.
    const blob = await new Promise<Blob | null>((resolve) => canvas!.toBlob(resolve, 'image/jpeg', 0.92))
    if (!blob) return

    const response = await fetch('/api/v1/capture/frames', {
      method: 'POST',
      headers: { 'Content-Type': blob.type },
      body: blob,
    })
    if (!response.ok) {
      const text = await response.text()
      let message = text
      try {
        const parsed = JSON.parse(text) as { error?: string }
        if (parsed.error) message = parsed.error
      } catch {
        // Keep the raw body.
      }
      emit({ error: message || response.statusText })
      return
    }

    const result = (await response.json()) as { accepted: boolean }
    emit({
      error: null,
      sent: state.sent + 1,
      accepted: state.accepted + (result.accepted ? 1 : 0),
      idle: state.idle + (result.accepted ? 0 : 1),
    })
  } catch (err) {
    emit({ error: err instanceof Error ? err.message : String(err) })
  } finally {
    posting = false
  }
}

/**
 * tameExposure biases the camera away from over-exposing the display.
 *
 * A camera aimed at a monitor sees a small bright rectangle surrounded by a dark bezel, a dark desk and a dim
 * room. Its meter averages all of that, decides the scene is under-exposed, and lifts the whole frame until the
 * one part that mattered is sitting on the sensor's ceiling. The result photographs beautifully and decodes not
 * at all: measured across captured frames, a quarter of every pixel came back at 255, and clipping is not a
 * degradation that a better decoder can undo — the differences between light cells are not compressed, they are
 * discarded before the frame is ever encoded.
 *
 * The correlation is direct. Frames with 5.8% of pixels clipped decoded; at 17.4% the geometry still locked but
 * the payload failed its CRC; at 25.8% the fiducials themselves could no longer be found, because a bright
 * payload flattened to the same white as their outer rings.
 *
 * So the exposure is pulled down before the first frame is posted. Support is patchy and entirely
 * device-dependent — this is one of the least uniformly implemented corners of the media APIs — so every step
 * is optional and failure is silent. A camera that ignores all of it is no worse off than before.
 */
async function tameExposure(track: MediaStreamTrack) {
  // getCapabilities is itself non-standard enough to be missing.
  const caps = (track.getCapabilities?.() ?? {}) as MediaTrackCapabilities & {
    exposureMode?: string[]
    exposureCompensation?: { min: number; max: number; step: number }
  }

  // Preferred: bias the automatic exposure down but leave it automatic, so the camera still tracks a
  // changing scene. Two thirds of the way to the floor is well clear of the clip on the phones this was
  // measured on without driving the dark cells into the noise at the other end.
  const comp = caps.exposureCompensation
  if (comp && typeof comp.min === 'number' && typeof comp.max === 'number') {
    const target = comp.min + (comp.max - comp.min) * 0.33
    try {
      await track.applyConstraints({
        advanced: [{ exposureMode: 'continuous', exposureCompensation: target }],
      } as unknown as MediaTrackConstraints)
      return
    } catch {
      // Fall through: some devices advertise the range and then refuse the value.
    }
  }

  // Failing that, ask for continuous exposure explicitly. It is what most cameras do anyway, but a device
  // left in a manual mode by whichever application had it last will otherwise keep that mode.
  if (caps.exposureMode?.includes('continuous')) {
    try {
      await track.applyConstraints({ advanced: [{ exposureMode: 'continuous' }] } as unknown as MediaTrackConstraints)
    } catch {
      // Nothing further to try, and an un-tamed camera still produces frames.
    }
  }
}

/**
 * start asks the browser for a camera and begins posting frames.
 *
 * `prepare` is called first so the receiver is taking posted frames before any are sent — the other order means
 * the first second of frames is refused.
 */
export async function start(options: {
  deviceId?: string
  rearFacing?: boolean
  detail?: CaptureDetail
  prepare: () => Promise<void>
}) {
  emit({ error: null, sent: 0, accepted: 0, idle: 0, blurred: 0, steadiness: 0 })
  // A new run measures its own sharpness from scratch: the last one may have been at a different distance, in
  // different light, or on a different camera entirely.
  bestSharpness = 0
  try {
    await options.prepare()

    // See videoConstraints: the resolution asked for here decides which grids can be decoded at all, and it
    // used to be pinned to 1080p — under which a 384 grid is unreadable at any framing.
    const constraints: MediaStreamConstraints = {
      video: videoConstraints({
        deviceId: options.deviceId,
        rearFacing: options.rearFacing,
        detail: options.detail,
      }),
      audio: false,
    }

    const media = await navigator.mediaDevices.getUserMedia(constraints)
    stream = media

    // The hidden element gets the stream first, because it is what frames are drawn from and it must be playing
    // before the posting timer starts.
    const source = ensureGrabber()
    source.srcObject = media
    await source.play().catch(() => undefined)

    if (preview) attach(preview)

    const track = media.getVideoTracks()[0]
    if (track) await tameExposure(track)

    const settings = track?.getSettings() ?? {}
    emit({
      running: true,
      label: track?.label ?? '',
      width: settings.width ?? 0,
      height: settings.height ?? 0,
    })

    // A camera unplugged, or a phone whose browser reclaimed it while another app took it, ends the track rather
    // than erroring — so this is how the page learns the camera has gone rather than going on claiming to capture.
    track?.addEventListener('ended', () => {
      emit({ error: 'The camera stopped — it was unplugged, or another application took it.' })
      void stop()
    })

    timer = window.setInterval(() => void postOneFrame(), Math.round(1000 / postRate))
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err)
    emit({
      running: false,
      error: /denied|dismissed|NotAllowed/i.test(message)
        ? 'Permission to use the camera was declined. The browser will not ask again until you allow it — look ' +
          'for the camera icon in the address bar.'
        : message,
    })
    throw err
  }
}

/** stop releases the camera. `release` tells the receiver to go back to reading frames from a directory. */
export async function stop(release?: () => Promise<void>) {
  if (timer !== null) {
    window.clearInterval(timer)
    timer = null
  }
  // Every track, which is what actually releases the device and turns the indicator off. Clearing the element's
  // srcObject alone leaves the camera held.
  stream?.getTracks().forEach((track) => track.stop())
  stream = null
  if (grabber) grabber.srcObject = null
  if (preview) preview.srcObject = null
  emit({ running: false, label: '', width: 0, height: 0 })

  if (release) {
    try {
      await release()
    } catch (err) {
      emit({ error: err instanceof Error ? err.message : String(err) })
    }
  }
}

/** running is whether the camera is open, for callers that need it outside a render. */
export function running(): boolean {
  return state.running
}
