import { videoConstraints } from './videoConstraints'
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

/** postRate is how many frames a second are sent. */
const postRate = 10

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
 * start asks the browser for a camera and begins posting frames.
 *
 * `prepare` is called first so the receiver is taking posted frames before any are sent — the other order means
 * the first second of frames is refused.
 */
export async function start(options: { deviceId?: string; rearFacing?: boolean; prepare: () => Promise<void> }) {
  emit({ error: null, sent: 0, accepted: 0, idle: 0 })
  try {
    await options.prepare()

    // See videoConstraints: the resolution asked for here decides which grids can be decoded at all, and it
    // used to be pinned to 1080p — under which a 384 grid is unreadable at any framing.
    const constraints: MediaStreamConstraints = {
      video: videoConstraints({ deviceId: options.deviceId, rearFacing: options.rearFacing }),
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
