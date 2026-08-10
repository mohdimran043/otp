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
  sequence: number
  decoded: boolean
  decode_error?: string
  frame_number?: number
  chunk_number?: number
  is_manifest: boolean
  is_parity: boolean
  bit_error_rate: number
  finder_score: number
  timing_score: number
  contrast: number
  captured_at: string
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
  health: () => request<{ status: string; protocol_version: number }>('/health'),

  // The stored capture, served straight from the object store. It is a URL rather than a fetch because it
  // goes into an <img>: a frame an operator is looking at is exactly what the camera saw, and re-encoding
  // it through JavaScript would be showing them something else.
  frameImageUrl: (id: string) => `/api/v1/frames/${id}/image`,
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
