// The receiver's API, as the browser sees it.
//
// The receiver has almost no write surface: frames arrive whether anybody asks or not, so what this app
// does is watch. The types below are the operator's view of a capture — is it decoding, how well, what is
// missing, and did the file that was supposed to arrive arrive intact.

export interface SessionView {
  capturing?: boolean
  id: string
  status: string
  source: string
  transmission_id?: string
  frames_captured: number
  frames_decoded: number
  frames_failed: number
  decode_rate: number
  started_at: string
  uptime_seconds: number
  // Absent when the build cannot report it, which is deliberately different from all-zeroes: "nothing is
  // asking" and "nothing was recoverable" would otherwise look identical.
  recovery?: RecoveryStats
}

// What the soft-decision retry managed, and how frames finished by the stage they failed at.
export interface RecoveryStats {
  attempted: number
  recovered: number
  // Total corrections tried across all attempts. Rising per-recovery is the earliest sign a camera is
  // drifting, visible while the recovered count still looks healthy.
  candidates: number
  // Keyed by stage: decoded, no_quad, descriptor_crc, header_crc, footer_crc, payload_crc, below_floors,
  // degenerate_geometry, unsupported_version, other.
  buckets: Record<string, number>
  /** Where the time goes: what every frame costs to decode, against what an offered frame costs in the engine. */
  mean_decode_ms: number
  mean_recover_ms: number
}

export interface MergedView {
  filename: string
  size_bytes: number
  sha256: string
  verified: boolean
  verify_error?: string
  verified_at?: string
}

export interface TransmissionView {
  transmission_id: string
  filename: string
  original_size: number
  expected_sha256: string
  chunk_count: number
  chunk_size: number
  callback_url?: string
  chunks_arrived: number
  chunks_recovered: number
  missing_count: number
  progress: number
  merged?: MergedView
  manifest_received_at: string
}

export interface GateView {
  min_tone_fraction: number
  note: string
}

/** How the camera is pointed, as measured from the most recent captured frame. */
export type AlignmentStatus = 'searching' | 'too_far' | 'too_close' | 'off_axis' | 'marginal' | 'good' | 'too_dense'

export interface AlignmentView {
  /** False before any frame has been measured in this session, and after a restart. */
  live: boolean
  /** How many of the sender's tiled frames are in view, and how many there should be. */
  lanes_found: number
  lanes_expected: number
  locked: boolean
  decoded: boolean
  /** How much of the view the grid spans, 0..1. */
  fill: number
  module_pixels: number
  /** What this frame's encoding needs. Colour needs several times what black and white does. */
  required_module_pixels: number
  /** Upper end of the target band; 0 when the encoding has no useful upper bound. */
  max_module_pixels: number
  /**
   * The best this capture could resolve at this geometry, with the frame filling the short side. Below
   * required_module_pixels no amount of aiming helps and the sender's grid is the fault — which is the
   * opposite conclusion from too_far, and the numbers alone cannot tell them apart.
   */
  achievable_module_pixels: number
  /** The largest grid this capture could resolve at this encoding. */
  max_grid_for_capture: number
  /**
   * A grid a little short of what the encoding wants, which decodes a few frames in a hundred rather than
   * none. Shown amber rather than red: painting it red while chunks are being acknowledged is what made the
   * previous message untrustworthy.
   */
  geometry_marginal: boolean
  /** 0 for square-on, rising as the camera moves off-axis. */
  perspective: number
  finder_score: number
  timing_score: number
  contrast: number
  /**
   * Fiducial centres normalised to 0..1 of the frame: top-left, top-right, bottom-left, bottom-right.
   * The lead lane only — see `lanes` for every frame in the picture.
   */
  corners: [number, number][]
  /**
   * Every frame located in this capture, one entry each, so a tiled display can be outlined lane by
   * lane. Null or empty from a receiver that predates it, which is why the overlay falls back to
   * `corners`.
   */
  lanes: LaneOutline[] | null
  status: AlignmentStatus
  advice: string
  at: string
}

/** One located frame's outline and its own decode result. */
export interface LaneOutline {
  /** Normalised exactly like AlignmentView.corners, in the same order. */
  corners: [number, number][]
  /** Whether this lane's payload read, as opposed to the lane merely being found. */
  decoded: boolean
  /** Which frame this lane carried. */
  frame_number: number
}

export interface Chunk {
  id: string
  chunk_number: number
  is_parity: boolean
  size_bytes: number
  recovered: boolean
  received_at: string
}

export interface CapturedFrame {
  id: string
  session_id?: string
  sequence: number
  stored_path?: string
  decoded: boolean
  decode_error?: string
  transmission_id?: string
  frame_number?: number
  chunk_number?: number
  is_manifest: boolean
  is_parity: boolean
  bit_error_rate: number
  finder_score: number
  timing_score: number
  contrast: number
  captured_at: string

  /**
   * What happened on the way to the verdict, rather than only the verdict.
   *
   * All optional: a frame recorded before the receiver kept any of this reads as undefined, and the
   * detail view says "not recorded" rather than inventing a zero that looks like a measurement.
   */
  failure_stage?: string
  recovered?: boolean
  recovery_engine?: string
  recovery_stage?: string
  recovery_candidates?: number
  recovery_flips?: number
  recovery_ms?: number
  merged_shots?: number
  /** Which frame of a tiled photograph this row describes, and how many it held. */
  lane_index?: number
  lane_count?: number
  decode_ms?: number
}

/** One capture and every other lane read out of the same photograph. */
export interface FrameDetail {
  frame: CapturedFrame
  lanes: CapturedFrame[]
}

/** One stored certificate. The private key is never carried here — it does not leave the server. */
export interface CertificateView {
  role: 'local' | 'peer'
  certificate_pem: string
  fingerprint: string
  subject: string
  not_before?: string
  not_after?: string
  installed_at: string
  has_private_key: boolean
}

/**
 * What this side can do with certificates right now.
 *
 * `ready` is reported rather than left to be inferred from two absences, because it is the question every
 * caller is really asking: can certificate encryption be used at all.
 */
export interface CertificateStatus {
  local?: CertificateView
  peer?: CertificateView
  ready: boolean
  note: string
}

export interface DecoderConfig {
  protocol_version: number
  capture: {
    source: string
    dir: string
    idle_interval: string
    // How many frames are decoded at once. decode_workers is what was configured (0 meaning "per core"),
    // decode_workers_now is the number actually in force.
    decode_workers: number
    decode_workers_now: number
    // The deepest backlog of unread frames. One means the receiver kept up; a large number means the
    // display is producing frames faster than it can decode them.
    frames_behind: number
  }
  decoder: { min_finder_score: number; min_timing_score: number; encrypted: boolean }
  callback: { allowed_hosts: string[] | null; allow_any_host: boolean }
  // Where an operator can see the sending side of the same transfer. For their browser, not for this
  // process — the two applications still share only a protocol and a directory.
  peer?: { sender_ui_url: string }
}

// A capture device attached to this machine, as V4L2 reports it.
export interface CameraDevice {
  path: string
  name: string
  driver: string
  bus_info: string
  modes: CameraMode[]
  default: boolean
  selected: boolean
}

export interface CameraMode {
  format: string
  format_name: string
  width: number
  height: number
  fps: number[] | null
}

export interface CameraSelection {
  device: string
  format?: string
  width?: number
  height?: number
  fps?: number
  // Where frames come from. Empty leaves whatever is configured alone, so saving a camera choice does not
  // change the source as a side effect.
  source?: string
}

export interface CamerasView {
  supported: boolean
  source: string
  source_uses_camera: boolean
  // Whether a camera is open and running right now — which is what turns its light on. Reported separately
  // from the selection because the two came apart: choosing a device configured a mode and left the source
  // alone, so a saved setting reported success over a camera that never lit up.
  streaming: boolean
  // What the source may be set to, so the page offers a choice rather than a text field in which a typo
  // becomes a receiver that captures nothing.
  known_sources: string[]
  devices: CameraDevice[] | null
  selection: CameraSelection
  effective: CameraSelection
  substituted: boolean
  error?: string
}

// One attempt to hand a merged file to its callback URL. Attempts rather than a boolean, because a refused
// host, a 500, a timeout and a delivery not yet tried are four different problems.
export interface DeliveryView {
  url: string
  status: string
  attempts: number
  max_attempts: number
  http_status?: number
  last_error?: string
  delivered_at?: string
}

// KeyView is one decryption key loaded into the receiver's keyring, as the API reports it. The key
// itself never appears here — only a fingerprint, the first 8 bytes of its SHA-256 in hex — because
// a page that can display a key is a page that can leak one.
export interface KeyView {
  id: number
  label: string
  fingerprint: string
  created_at: string
}

// ImportEntry is one image the importer looked at inside an uploaded archive — a zip entry, or
// one half of a split composite PNG. It mirrors the receiver's pipeline.IngestResult (decoded,
// is_manifest, transmission_id, chunk_number, error) plus the two fields the import endpoint
// itself adds: which file this was, and why it was skipped before ever reaching the pipeline
// (wrong extension, unreadable, too large, not a decodable PNG).
export interface ImportEntry {
  name: string
  decoded?: boolean
  is_manifest?: boolean
  transmission_id?: string
  chunk_number?: number
  error?: string
  skipped?: string
}

// ImportSummary is exactly what POST /api/v1/import responds with. `transmissions` carries the
// ids themselves, sorted, so the page can link straight to one rather than merely counting them.
// `truncated` is present only when the import stopped early (the caller went away, or the
// request's own deadline passed) — its absence means "no".
export interface ImportSummary {
  entries: ImportEntry[]
  ingested: number
  skipped: number
  transmissions: string[]
  truncated?: boolean
}

export class ApiError extends Error {
  constructor(readonly status: number, message: string) {
    super(message)
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: { Accept: 'application/json', ...(init?.headers ?? {}) },
  })
  const text = await response.text()
  if (!response.ok) {
    let message = text
    try {
      const parsed = JSON.parse(text) as { error?: string }
      if (parsed.error) message = parsed.error
    } catch {
      // Keep the raw body: a proxy error is HTML, and "Unexpected token <" would tell nobody anything.
    }
    throw new ApiError(response.status, message || response.statusText)
  }
  return text ? (JSON.parse(text) as T) : ({} as T)
}

export const api = {
  session: () => request<SessionView>('/api/v1/session'),
  transmissions: () =>
    request<{ transmissions: TransmissionView[] | null }>('/api/v1/transmissions').then(
      (r) => r.transmissions ?? [],
    ),
  transmission: (id: string) => request<TransmissionView>(`/api/v1/transmissions/${id}`),
  chunks: (id: string) =>
    request<{ chunks: Chunk[] | null }>(`/api/v1/transmissions/${id}/chunks`).then((r) => r.chunks ?? []),
  missing: (id: string) =>
    request<{ missing: number[] | null; count: number }>(`/api/v1/transmissions/${id}/missing`),
  // The newest captures, decoded or not — what a live page needs to show frames arriving.
  recentFrames: (limit = 40) =>
    request<{ frames: CapturedFrame[] | null; capturing: boolean }>(
      `/api/v1/frames/recent?limit=${limit}`,
    ).then((r) => r.frames ?? []),

  failedFrames: (limit = 30) =>
    request<{ frames: CapturedFrame[] | null }>(`/api/v1/frames/failed?limit=${limit}`).then(
      (r) => r.frames ?? [],
    ),
  deliveries: (id: string) =>
    request<{ deliveries: DeliveryView[] | null }>(`/api/v1/transmissions/${id}/deliveries`).then(
      (r) => r.deliveries ?? [],
    ),

  config: () => request<DecoderConfig>('/api/v1/config'),

  // The decryption keyring. `request` already takes a RequestInit, so POST and DELETE need no
  // dedicated helper — they pass a method (and a body, for POST) the same way selectCamera does.
  keys: () => request<{ keys: KeyView[] | null }>('/api/v1/keys').then((r) => r.keys ?? []),
  addKey: (keyHex: string, label: string) =>
    request<KeyView>('/api/v1/keys', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key_hex: keyHex, label }),
    }),
  deleteKey: (id: number) => request<void>(`/api/v1/keys/${id}`, { method: 'DELETE' }),

  // The blank-screen threshold: how much of a captured image must be dark, and how much light, before the
  // receiver bothers decoding it. Worth surfacing because when it is set too high nothing is observable — a
  // rejected frame reaches neither the decoder nor the failure log, so frames are posted, counted, and vanish.
  // Where the camera is pointed, polled several times a second while someone is aiming one. It reports the
  // last frame rather than an average, because an average describes where the camera used to be.
  alignment: () => request<AlignmentView>('/api/v1/capture/alignment'),

  captureGate: () => request<GateView>('/api/v1/capture/gate'),
  setCaptureGate: (minToneFraction: number) =>
    request<GateView>('/api/v1/capture/gate', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ min_tone_fraction: minToneFraction }),
    }),

  // Removing a transmission entirely — its manifest, chunks, merged file and acknowledgements — rather
  // than merely hiding it. There is no undo: the object store rows this deletes are gone with it.
  deleteTransmission: (id: string) => request<void>(`/api/v1/transmissions/${id}`, { method: 'DELETE' }),

  // importFrames replays a downloaded frame archive (a zip of frame PNGs, or the single
  // composite PNG a one-chunk transfer produces) into the running pipeline. No Content-Type is
  // set here — the browser fills in the multipart boundary itself, and overriding it would
  // strip that boundary out from under `body`.
  importFrames: (file: File) => {
    const body = new FormData()
    body.append('file', file)
    return request<ImportSummary>('/api/v1/import', { method: 'POST', body })
  },

  cameras: () => request<CamerasView>('/api/v1/cameras'),

  // The selection is a PUT rather than a POST: choosing a camera replaces the choice rather than adding
  // to a collection, and sending it twice must mean the same thing as sending it once.
  selectCamera: (selection: CameraSelection) =>
    request<{
      selection: CameraSelection
      source: string
      warning?: string
      note?: string
      // Set when the server chose the mode itself, with a sentence saying what it chose and why.
      auto_configured?: boolean
      configured?: string
      applied?: boolean
      capturing_from?: string
      error_detail?: string
    }>(
      '/api/v1/cameras/selection',
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(selection),
      },
    ),
  certificates: () => request<CertificateStatus>('/api/v1/certificates'),

  generateCertificate: (name?: string) =>
    request<CertificateStatus>('/api/v1/certificates/generate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(name ? { name } : {}),
    }),

  /** Installs the other side's public certificate. The PEM is posted as-is. */
  installPeerCertificate: (certificatePEM: string) =>
    request<CertificateStatus>('/api/v1/certificates/peer', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ certificate_pem: certificatePEM }),
    }),

  removePeerCertificate: () =>
    request<CertificateStatus>('/api/v1/certificates/peer', { method: 'DELETE' }),

  health: () => request<{ status: string; protocol_version: number }>('/health'),

  // The stored capture, served straight from the object store. It is a URL rather than a fetch because it
  // goes into an <img>: a frame an operator is looking at is exactly what the camera saw, and re-encoding
  // it through JavaScript would be showing them something else.
  frameImageUrl: (id: string) => `/api/v1/frames/${id}/image`,

  frame: (id: string) => request<FrameDetail>(`/api/v1/frames/${id}`),

  transmissionFrames: (id: string, limit = 500) =>
    request<{ frames: CapturedFrame[] | null }>(
      `/api/v1/transmissions/${id}/frames?limit=${limit}`,
    ).then((r) => r.frames ?? []),

  /** A link rather than a fetch: the archive is hundreds of megabytes and belongs to the browser's
   *  own download manager, not to a promise held in a component. */
  transmissionFramesZipUrl: (id: string) => `/api/v1/transmissions/${id}/frames.zip`,
  downloadUrl: (id: string) => `/api/v1/transmissions/${id}/file`,

  // inlineUrl asks for the file to be shown in place rather than downloaded. The server honours it only for
  // the types its allowlist permits — an SVG or an HTML file arrives as an opaque download however this asks,
  // because rendering one from this origin would be script running on the page that runs the receiver.
  inlineUrl: (id: string) => `/api/v1/transmissions/${id}/file?inline=1`,
}

export function formatBytes(bytes: number): string {
  // Rounded, because this formats rates as well as sizes. A byte count is always a whole number but
  // bytes-per-second is not, and "137.83533765032377 B/s" is not a figure anybody reads.
  if (bytes < 1024) return `${bytes < 10 ? bytes.toFixed(1) : Math.round(bytes)} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(value < 10 ? 2 : 1)} ${units[unit]}`
}

export function formatPercent(fraction: number): string {
  if (!Number.isFinite(fraction)) return '—'
  return `${(fraction * 100).toFixed(1)}%`
}

export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—'
  if (seconds < 60) return `${seconds.toFixed(0)}s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ${Math.round(seconds % 60)}s`
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`
}
