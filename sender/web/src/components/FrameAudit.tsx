import { useMemo, useState } from 'react'
import {
  Box,
  Chip,
  Dialog,
  DialogContent,
  DialogTitle,
  Divider,
  IconButton,
  Link,
  Stack,
  TextField,
  ToggleButton,
  ToggleButtonGroup,
  Tooltip,
  Typography,
} from '@mui/material'
import CloseIcon from '@mui/icons-material/Close'

import { api, formatBytes, type Chunk, type Frame } from '../api/client'

// The frame audit: every frame this transmission put on the screen, as an image, afterwards.
//
// It exists for one question, which comes up only after something has gone wrong: *what exactly did we
// display?* A receiver reporting that frame 214 failed to decode, or a chunk that took four attempts to
// get through, leaves an ambiguity that no counter resolves — the frame may have been rendered wrongly,
// or rendered correctly and photographed badly. The stored image settles it, because it is the same
// bytes that went to the channel rather than a re-render that might differ.
//
// Frames are addressed by frame number rather than by row id, because that is the number written into
// the frame's own header band and the number the receiver reports.

interface Props {
  transmissionId: string
  frames: Frame[]
  chunks: Chunk[]
}

/** hex renders the base64 SHA-256 the API sends as the hex an operator compares against. */
function hex(base64?: string): string {
  if (!base64) return '—'
  try {
    const raw = atob(base64)
    let out = ''
    for (let i = 0; i < raw.length; i += 1) out += raw.charCodeAt(i).toString(16).padStart(2, '0')
    return out
  } catch {
    return base64
  }
}

type Filter = 'all' | 'manifest' | 'repeated' | 'retransmitted'

export function FrameAudit({ transmissionId, frames, chunks }: Props) {
  const [filter, setFilter] = useState<Filter>('all')
  const [query, setQuery] = useState('')
  const [open, setOpen] = useState<Frame | null>(null)
  const [zoom, setZoom] = useState<1 | 2 | 3>(1)

  // A frame's interest is mostly its chunk's history: how many times the chunk had to be sent before it
  // was acknowledged is the difference between "displayed a lot because the display never idles" and
  // "displayed a lot because it kept not arriving".
  const chunkById = useMemo(() => new Map(chunks.map((c) => [c.id, c])), [chunks])

  const shown = useMemo(() => {
    const wanted = frames.filter((frame) => {
      const chunk = frame.chunk_id ? chunkById.get(frame.chunk_id) : undefined
      switch (filter) {
        case 'manifest':
          return frame.is_manifest
        case 'repeated':
          return frame.displayed_count > 1
        case 'retransmitted':
          return (chunk?.retry_count ?? 0) > 0
        default:
          return true
      }
    })
    if (!query.trim()) return wanted
    const number = Number.parseInt(query.trim(), 10)
    if (Number.isNaN(number)) return wanted
    return wanted.filter((frame) => frame.frame_number === number)
  }, [frames, chunkById, filter, query])

  const retransmitted = frames.filter(
    (f) => f.chunk_id && (chunkById.get(f.chunk_id)?.retry_count ?? 0) > 0,
  ).length

  const openChunk = open?.chunk_id ? chunkById.get(open.chunk_id) : undefined

  return (
    <>
      <Stack direction="row" spacing={2} alignItems="center" flexWrap="wrap" useFlexGap sx={{ mb: 1 }}>
        <ToggleButtonGroup
          size="small"
          exclusive
          value={filter}
          onChange={(_, value: Filter | null) => value !== null && setFilter(value)}
        >
          <ToggleButton value="all">All {frames.length}</ToggleButton>
          <ToggleButton value="manifest">Manifests</ToggleButton>
          <ToggleButton value="repeated">Repeated</ToggleButton>
          <ToggleButton value="retransmitted">Retransmitted {retransmitted}</ToggleButton>
        </ToggleButtonGroup>
        <TextField
          size="small"
          label="Frame number"
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          sx={{ width: 160 }}
        />
      </Stack>

      {shown.length === 0 ? (
        <Typography variant="body2" color="text.secondary">
          No frame matches.
        </Typography>
      ) : (
        <Box
          sx={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fill, minmax(88px, 1fr))',
            gap: 1,
          }}
        >
          {shown.slice(0, 600).map((frame) => {
            const chunk = frame.chunk_id ? chunkById.get(frame.chunk_id) : undefined
            const retried = (chunk?.retry_count ?? 0) > 0
            return (
              <Tooltip
                key={frame.id}
                title={
                  frame.is_manifest
                    ? `manifest · ${frame.displayed_count} displays`
                    : `frame ${frame.frame_number}${chunk ? ` · chunk ${chunk.esi}${chunk.is_parity ? ' (parity)' : ''}` : ''} · ${frame.displayed_count} displays${retried ? ` · ${chunk?.retry_count} retransmissions` : ''}`
                }
              >
                <Box
                  onClick={() => setOpen(frame)}
                  sx={{
                    cursor: 'pointer',
                    border: 2,
                    borderColor: retried
                      ? 'warning.main'
                      : frame.is_manifest
                        ? 'secondary.main'
                        : 'divider',
                    borderRadius: 0.5,
                    bgcolor: '#000',
                    aspectRatio: '1',
                    overflow: 'hidden',
                    display: 'grid',
                    placeItems: 'center',
                  }}
                >
                  {/* Lazily loaded: a transmission has hundreds of these and only the ones on screen
                      are worth a request. */}
                  <Box
                    component="img"
                    loading="lazy"
                    src={api.frameImage(transmissionId, frame.frame_number)}
                    alt={`frame ${frame.frame_number}`}
                    sx={{ width: '100%', height: '100%', objectFit: 'contain', display: 'block' }}
                  />
                </Box>
              </Tooltip>
            )
          })}
        </Box>
      )}

      {shown.length > 600 && (
        <Typography variant="caption" color="text.secondary">
          showing the first 600 of {shown.length} — filter or search for a specific frame number
        </Typography>
      )}

      <Dialog open={open !== null} onClose={() => setOpen(null)} maxWidth="lg">
        {open && (
          <>
            <DialogTitle sx={{ pr: 6 }}>
              {open.is_manifest ? 'Manifest frame' : `Frame ${open.frame_number}`}
              <IconButton
                onClick={() => setOpen(null)}
                sx={{ position: 'absolute', right: 8, top: 8 }}
              >
                <CloseIcon />
              </IconButton>
            </DialogTitle>
            <DialogContent>
              <Stack spacing={2}>
                <Stack direction="row" spacing={1} alignItems="center" flexWrap="wrap" useFlexGap>
                  <ToggleButtonGroup
                    size="small"
                    exclusive
                    value={zoom}
                    onChange={(_, value: 1 | 2 | 3 | null) => value !== null && setZoom(value)}
                  >
                    {([1, 2, 3] as const).map((factor) => (
                      <ToggleButton key={factor} value={factor}>
                        {factor}×
                      </ToggleButton>
                    ))}
                  </ToggleButtonGroup>
                  <Link
                    href={api.frameImage(transmissionId, open.frame_number)}
                    target="_blank"
                    rel="noreferrer"
                    variant="body2"
                  >
                    open the PNG
                  </Link>
                </Stack>

                {/* Whole multiples and no smoothing, for the same reason the display uses them: a cell
                    resampled across its own boundary is not the cell that was sent, and an auditor
                    looking for a rendering fault would be looking at one the viewer introduced. */}
                <Box sx={{ bgcolor: '#000', p: 1, display: 'grid', placeItems: 'center', maxHeight: '60vh', overflow: 'auto' }}>
                  <Box
                    component="img"
                    src={api.frameImage(transmissionId, open.frame_number)}
                    alt={`frame ${open.frame_number}`}
                    sx={{
                      width: open.width_px * zoom,
                      height: open.height_px * zoom,
                      imageRendering: 'pixelated',
                      display: 'block',
                    }}
                  />
                </Box>

                <Divider />

                <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
                  <Chip size="small" label={`${open.width_px}×${open.height_px} px`} />
                  <Chip size="small" label={`${formatBytes(open.payload_bytes)} payload`} />
                  <Chip size="small" label={`${open.displayed_count} displays`} />
                  {open.is_manifest && <Chip size="small" color="secondary" label="manifest" />}
                  {openChunk && (
                    <>
                      <Chip
                        size="small"
                        label={`chunk ${openChunk.esi}${openChunk.is_parity ? ' · parity' : ''}`}
                      />
                      <Chip
                        size="small"
                        color={openChunk.acked ? 'success' : 'default'}
                        label={openChunk.acked ? 'acknowledged' : 'not acknowledged'}
                      />
                      {openChunk.retry_count > 0 && (
                        <Chip
                          size="small"
                          color="warning"
                          label={`${openChunk.retry_count} retransmissions`}
                        />
                      )}
                    </>
                  )}
                </Stack>

                <Box>
                  <Typography variant="caption" color="text.secondary" display="block">
                    Frame SHA-256 — the hash of these exact bytes, as stored
                  </Typography>
                  <Typography variant="body2" sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>
                    {hex(open.sha256)}
                  </Typography>
                </Box>

                {open.last_displayed && (
                  <Typography variant="caption" color="text.secondary">
                    last displayed {new Date(open.last_displayed).toLocaleString()}
                  </Typography>
                )}

                {openChunk && openChunk.retry_count > 0 && (
                  <Typography variant="caption" color="text.secondary">
                    This chunk was sent again {openChunk.retry_count} time
                    {openChunk.retry_count === 1 ? '' : 's'} because no acknowledgement arrived before
                    the timeout. The image above is what was displayed each time — identical bytes — so
                    a decode that failed here failed in the channel or the camera, not in the
                    rendering.
                  </Typography>
                )}
              </Stack>
            </DialogContent>
          </>
        )}
      </Dialog>
    </>
  )
}
