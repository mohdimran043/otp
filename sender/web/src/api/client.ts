// The sender's API, as the browser sees it.
//
// Types are declared here by hand rather than generated, and the reason is that they are the contract
// this app depends on: writing them out means a change to the API shows up as a type error in the code
// that consumed the old shape, rather than as an undefined at runtime in a panel nobody opened.

export interface TransferAccepted {
  transmission_id: string
  file_id: string
  filename: string
  size_bytes: number
  sha256: string
  callback_url?: string
  status: string
  jobs: string[]
}

export interface ResultView {
  verified: boolean
  sha256: string
  size: number
  chunks_received: number
  chunks_recovered: number
  frames_captured: number
  frames_failed: number
  callback_delivered: boolean
  callback_status?: number
  callback_error?: string
  seconds: number
  bytes_per_second: number
  error?: string
}

export interface TransferStatus {
  transmission_id: string
  filename: string
  // The hash the sender declared, for comparing against the receiver's computed one.
  sha256: string
  status: string
  callback_url?: string
  original_size: number
  compressed_size: number
  chunk_count: number
  chunk_size: number
  frame_count: number
  acked_chunks: number
  progress: number
  retransmits: number
  encoder: string
  compression: string
  fec_codec: string
  encryption: string
  /** How many frames are tiled onto the display at once. */
  lanes: number
  grid_width: number
  grid_height: number
  error?: string
  result?: ResultView
}

export interface Transmission {
  id: string
  status: string
  encoder: string
  compression: string
  fec_codec: string
  original_size: number
  compressed_size: number
  chunk_count: number
  frame_count: number
  acked_chunks: number
  retransmits: number
  callback_url?: string
  created_at: string
  error?: string
}

export interface Chunk {
  id: string
  esi: number
  is_parity: boolean
  size_bytes: number
  acked: boolean
  retry_count: number
}

export interface Frame {
  id: string
  frame_number: number
  is_manifest: boolean
  width_px: number
  height_px: number
  payload_bytes: number
  displayed_count: number
  last_displayed?: string

  // chunk_id links a frame to what it carried, which is what makes an audit useful: the frame's image
  // answers "what did we display", and the chunk's retry count answers "did it get through".
  chunk_id?: string

  // sha256 arrives base64 encoded, being []byte on the wire.
  sha256?: string
}

// DisplayFrame is the frame on the screen right now — the one a camera is looking at.
export interface DisplayFrame {
  sequence: number
  frame_number: number
  transmission_id?: string
  is_manifest: boolean
  width_px: number
  height_px: number
  bytes: number
  shown_at: string
  age_ms: number
  image_url: string

  // image_png is the frame itself, base64, present when the request asked for include=image.
  image_png?: string

  /**
   * cleared marks the end of a transfer: the screen is empty and a viewer should show nothing.
   *
   * Delivered as a frame rather than as an absence because the display is followed by a long poll
   * waiting for the sequence to advance — there is no way to send "nothing" down a channel that is
   * waiting for something. A cleared frame carries no image and no geometry.
   */
  cleared?: boolean
}

export interface DisplayStatus {
  sink: string
  live: boolean
  fps: number
  cell_px: number
  quiet_zone_px: number
  grid_cols: number
  grid_rows: number
  encoder: string
  bit_depth: number
  frames_shown: number

  // held is whether an operator has stopped the display. It comes from the server rather than being
  // remembered locally because the hold belongs to the display: a second tab, or this page after a
  // reload, has to be able to find out.
  held: boolean
  held_since?: string

  frame?: DisplayFrame
}

// The display's own settings: the two knobs that set the transfer rate.
export interface DisplaySettings {
  /** How many frames are tiled onto the display at once. */
  lanes: number
  fps: number
  brightness: number
  gamma: number
  window_size: number
  keep_alive: boolean
  sink: string
  grid_width: number
  grid_height: number
  cell_pixels: number
  quiet_zone: number
  encoder: string
  bit_depth: number
  image_width_px: number
  image_height_px: number
  bytes_per_frame: number
  bytes_per_second: number
  transmitting: number
}

// Only the fields being changed are sent. A form that posted its whole state back would reset a field it
// never showed — and with a frame rate, zero means "never display anything".
export type DisplaySettingsPatch = Partial<{
  // How many frames are tiled onto the display at once. Reloadable while a transfer is running,
  // because it decides how frames are shown rather than what they contain.
  lanes: number
  fps: number
  brightness: number
  gamma: number
  window_size: number
  grid_width: number
  grid_height: number
  cell_pixels: number
  quiet_zone: number
  encoder: string
  bit_depth: number
  // sink chooses the display channel: "file" (the shared directory) or "none" (camera-only,
  // discarding the display's own write). It is not reloadable — the sink opens once at process
  // start — so this takes effect on the next restart, same as the geometry fields above.
  sink: string
}>

export interface TransferControl {
  transmission_id: string
  status: string
  acked_chunks: number
  chunk_count: number
  jobs_cancelled?: number
  note?: string
}

// KeyView is one saved encryption key, as the API reports it. The key itself never appears
// here — only a fingerprint, the first 8 bytes of its SHA-256 in hex — because a page that can
// display a key is a page that can leak one. A transfer picks a key by id (encryption_key_id)
// instead of carrying its hex again.
export interface KeyView {
  id: number
  label: string
  fingerprint: string
  created_at: string
}

export interface Job {
  id: string
  type: string
  status: string
  progress: number
  message: string
  error?: string
  attempts: number
  max_attempts: number
  created_at: string
}

export interface EncoderProfile {
  id: number
  name: string
  description: string
  bit_depths: number[]
  default_bit_depth: number
}

export interface CodecProfile {
  id: number
  name: string
  description: string
}

export interface Profiles {
  protocol_version: number
  encoders: EncoderProfile[]
  compressors: CodecProfile[]
  fec_codecs: CodecProfile[]
  defaults: {
    encoder: string
    bit_depth: number
    compression: string
    level: number
    fec_codec: string
    grid: string
    cell_pixels: number
    fps: number
    // encryption_configured is true when this deployment has a global encryption key set
    // (OTP_SENDER_ENCRYPTION_KEY_HEX). It never carries the key itself — only whether one
    // exists — which is enough for the form to pick a default that matches what the API
    // does with an omitted encryption field: encrypt under that key.
    encryption_configured: boolean
  }
}

// ApiError carries the status alongside the message, because the UI treats them differently: a 4xx is
// something the operator can fix from the form they are looking at, and a 5xx is not.
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
    // The server sends {"error": "..."} for anything it refused. Falling back to the raw body matters
    // for the cases it does not — a proxy returning HTML, say — because "Unexpected token <" tells an
    // operator nothing about what went wrong.
    let message = text
    try {
      const parsed = JSON.parse(text) as { error?: string }
      if (parsed.error) message = parsed.error
    } catch {
      // Keep the raw body.
    }
    throw new ApiError(response.status, message || response.statusText)
  }
  return text ? (JSON.parse(text) as T) : ({} as T)
}

export const api = {
  profiles: () => request<Profiles>('/api/v1/profiles'),

  transfers: (status?: string) =>
    request<{ transfers: Transmission[] | null }>(
      `/api/v1/transfers${status ? `?status=${encodeURIComponent(status)}` : ''}`,
    ).then((r) => r.transfers ?? []),

  transfer: (id: string) => request<TransferStatus>(`/api/v1/transfers/${id}`),

  chunks: (id: string) =>
    request<{ chunks: Chunk[] | null }>(`/api/v1/transfers/${id}/chunks`).then((r) => r.chunks ?? []),

  frames: (id: string) =>
    request<{ frames: Frame[] | null }>(`/api/v1/transfers/${id}/frames`).then((r) => r.frames ?? []),

  jobs: (id: string) =>
    request<{ jobs: Job[] | null }>(`/api/v1/transfers/${id}/jobs`).then((r) => r.jobs ?? []),

  // frameImage is the audit path: the exact bytes that were put on the screen, addressed by the frame
  // number the receiver reports when a decode fails.
  frameImage: (id: string, frameNumber: number) =>
    `/api/v1/transfers/${id}/frames/${frameNumber}/image`,

  // frameArchiveUrl is the whole-transfer download: every frame the display rendered, zipped (or the
  // single composite PNG when there is only one), so an operator can hand the images to a receiver that
  // has no camera at all.
  frameArchiveUrl: (id: string) => `/api/v1/transfers/${id}/frames/archive`,

  // Stopping a transfer. A status change on the row, which the display loop reads every frame — so a stop
  // takes effect within one frame interval rather than needing the process restarted.
  cancel: (id: string) =>
    request<TransferControl>(`/api/v1/transfers/${id}/cancel`, { method: 'POST' }),
  pause: (id: string) => request<TransferControl>(`/api/v1/transfers/${id}/pause`, { method: 'POST' }),
  resume: (id: string) => request<TransferControl>(`/api/v1/transfers/${id}/resume`, { method: 'POST' }),

  // startTransfer begins displaying a transfer that was prepared with autostart=false and is
  // sitting in "ready" waiting for an operator. 409 unless the transfer is actually ready.
  startTransfer: (id: string) =>
    request<TransferControl>(`/api/v1/transfers/${id}/start`, { method: 'POST' }),

  // deleteTransfer removes a transfer entirely — the row and everything the pipeline wrote for
  // it. 409 while it is preparing, transmitting, or paused: cancel it first.
  deleteTransfer: (id: string) => request<void>(`/api/v1/transfers/${id}`, { method: 'DELETE' }),

  // The saved-key keyring. Keys go in and fingerprints come out; the key itself is never
  // readable back. A transfer names one by id (encryption_key_id) instead of pasting its hex
  // into every request.
  keys: () => request<{ keys: KeyView[] | null }>('/api/v1/keys').then((r) => r.keys ?? []),
  addKey: (keyHex: string, label: string) =>
    request<KeyView>('/api/v1/keys', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key_hex: keyHex, label }),
    }),
  deleteKey: (id: number) => request<void>(`/api/v1/keys/${id}`, { method: 'DELETE' }),

  display: () => request<DisplayStatus>('/api/v1/display'),

  // Driving the display by hand. Hold stops it advancing so a frame can be looked at — or photographed
  // — for as long as it takes; showFrame is refused unless it is held, because a running scheduler would
  // replace the chosen frame within a frame interval and the control would appear to work without doing
  // anything.
  holdDisplay: () => request<DisplayStatus>('/api/v1/display/hold', { method: 'POST' }),

  releaseDisplay: () => request<DisplayStatus>('/api/v1/display/release', { method: 'POST' }),

  showFrame: (transmissionID: string, frameNumber: number) =>
    request<DisplayStatus>('/api/v1/display/frame', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ transmission_id: transmissionID, frame_number: frameNumber }),
    }),

  settings: () => request<DisplaySettings>('/api/v1/settings'),

  updateSettings: (patch: DisplaySettingsPatch) =>
    request<DisplaySettings>('/api/v1/settings', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(patch),
    }),

  // originalFile is the file as it was uploaded. inline asks for it to be shown in place, which the server
  // honours only for types it considers safe to render.
  originalFile: (id: string, inline = false) =>
    `/api/v1/transfers/${id}/file${inline ? '?inline=1' : ''}`,

  // nextDisplayFrame long-polls: it resolves when the display moves past `after`, or null when the poll
  // expires with nothing new. Returning null rather than throwing matters — an expired poll is the
  // normal case on an idle display, and a caller that treated it as an error would show a fault.
  nextDisplayFrame: async (after: number, signal?: AbortSignal): Promise<DisplayFrame | null> => {
    // include=image asks for the pixels in the same response. One round trip per frame rather than two:
    // at 30 fps there are 33 ms between frames and a second fetch spends a meaningful share of them.
    const response = await fetch(`/api/v1/display/next?after=${after}&include=image`, {
      headers: { Accept: 'application/json' },
      signal,
    })
    if (response.status === 204) return null
    const text = await response.text()
    if (!response.ok) {
      let message = text
      try {
        const parsed = JSON.parse(text) as { error?: string }
        if (parsed.error) message = parsed.error
      } catch {
        // Keep the raw body.
      }
      throw new ApiError(response.status, message || response.statusText)
    }
    return JSON.parse(text) as DisplayFrame
  },

  // The upload is a multipart form rather than JSON, because the file travels with it: the whole point
  // of this endpoint is that one request carries the bytes and the URL the result should go to.
  submit: (form: FormData) =>
    request<TransferAccepted>('/api/v1/transfers', { method: 'POST', body: form }),

  health: () => request<{ status: string; protocol_version: number }>('/health'),
}

// formatBytes and formatRate exist because every panel needs them and an operator reading "48234496"
// is doing arithmetic the interface should have done.
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

export function formatRate(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return '—'
  return `${formatBytes(bytesPerSecond)}/s`
}

export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—'
  if (seconds < 60) return `${seconds.toFixed(1)}s`
  const minutes = Math.floor(seconds / 60)
  const rest = Math.round(seconds % 60)
  if (minutes < 60) return `${minutes}m ${rest}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

// eta estimates the time left from the rate achieved so far.
//
// It is derived from acknowledged chunks rather than from displayed frames, because an acknowledged
// chunk is one that actually arrived: estimating from frames displayed would show a transfer running
// ahead of itself and then stalling as retransmissions caught up.
export function eta(status: TransferStatus, elapsedSeconds: number): string {
  if (status.acked_chunks <= 0 || status.chunk_count <= 0) return '—'
  const perChunk = elapsedSeconds / status.acked_chunks
  const remaining = status.chunk_count - status.acked_chunks
  if (remaining <= 0) return 'done'
  return formatDuration(perChunk * remaining)
}
