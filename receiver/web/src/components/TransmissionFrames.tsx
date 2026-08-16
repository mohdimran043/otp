import { useState } from 'react'
import { Alert, Box, Button, Chip, Paper, Stack, Tooltip, Typography } from '@mui/material'
import DownloadIcon from '@mui/icons-material/Download'
import { useQuery } from '@tanstack/react-query'

import { api, formatPercent, type CapturedFrame } from '../api/client'
import { FrameDetailDialog } from './FrameDetail'
import { signal } from '../theme'

// Every photograph this transfer was built from.
//
// The chunk grid above says which chunks arrived; this says what they arrived *in*. They answer
// different questions, and only the second one is answerable after the fact: a transfer that needed
// four hundred photographs to deliver ninety chunks was working badly in a way no count of chunks
// records, and the evidence for why is in the pictures.
//
// The archive is the other half. A channel cannot be reproduced from a description, so a change that
// claims to decode better has to be measured against the frames that actually failed — which is what
// receiver/ai/corpus replays. Downloading them is how a bad session becomes a regression test.

/** kindOf sorts a capture into what an operator needs to distinguish at a glance. */
function kindOf(frame: CapturedFrame): { label: string; colour: string } {
  if (!frame.decoded) return { label: frame.failure_stage || 'unreadable', colour: signal.fault }
  if (frame.recovered) return { label: `chunk ${frame.chunk_number ?? '?'}`, colour: signal.marginal }
  if (frame.is_manifest) return { label: 'manifest', colour: signal.idle }
  if (frame.is_parity) return { label: `parity ${frame.chunk_number ?? '?'}`, colour: signal.adjust }
  return { label: `chunk ${frame.chunk_number ?? '?'}`, colour: signal.lock }
}

export function TransmissionFrames({ transmissionId }: { transmissionId: string }) {
  const [open, setOpen] = useState<string | null>(null)

  const frames = useQuery({
    queryKey: ['transmission-frames', transmissionId],
    queryFn: () => api.transmissionFrames(transmissionId),
  })

  const list = frames.data ?? []
  const recovered = list.filter((f) => f.recovered).length
  const firstRead = list.filter((f) => f.decoded && !f.recovered).length

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Stack direction="row" alignItems="baseline" spacing={2} sx={{ mb: 1 }} flexWrap="wrap" useFlexGap>
        <Typography variant="subtitle1">Frames received</Typography>
        <Typography variant="caption" color="text.secondary" sx={{ flexGrow: 1 }}>
          every photograph this transfer was read from — click one to see what happened to it
        </Typography>

        {list.length > 0 && (
          <>
            <Chip size="small" label={`${firstRead} read first time`} />
            {recovered > 0 && (
              <Chip size="small" color="warning" label={`${recovered} recovered`} />
            )}
          </>
        )}

        {/* An anchor rather than a fetch: the archive runs to hundreds of megabytes and belongs to the
            browser's download manager, not to a promise held open in a component. */}
        <Button
          size="small"
          variant="outlined"
          startIcon={<DownloadIcon />}
          component="a"
          href={api.transmissionFramesZipUrl(transmissionId)}
          disabled={list.length === 0}
        >
          Download all photos
        </Button>
      </Stack>

      {list.length === 0 && (
        <Alert severity="info" variant="outlined">
          No captured frames are recorded against this transfer. Frames that never decoded carry nothing
          saying which transfer they belonged to, so they are held against the capture session instead —
          see the live capture page for everything the camera photographed, readable or not.
        </Alert>
      )}

      {list.length > 0 && (
        <>
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 1 }}>
            {list.length} photograph{list.length === 1 ? '' : 's'}, oldest first. Amber means the frame
            did not read on the first attempt and was rescued by the recovery engine — it is still the
            frame, verified against its own CRC32 and SHA-256, but a transfer whose successes are mostly
            amber is one small change away from failing.
          </Typography>

          <Box
            sx={{
              display: 'grid',
              gridTemplateColumns: 'repeat(auto-fill, minmax(96px, 1fr))',
              gap: 1,
              maxHeight: 460,
              overflowY: 'auto',
            }}
          >
            {list.map((frame) => {
              const kind = kindOf(frame)
              return (
                <Tooltip
                  key={frame.id}
                  title={
                    <Box component="span" sx={{ whiteSpace: 'pre-line' }}>
                      {[
                        `capture #${frame.sequence}`,
                        kind.label,
                        (frame.lane_count ?? 1) > 1
                          ? `lane ${(frame.lane_index ?? 0) + 1} of ${frame.lane_count}`
                          : '',
                        frame.recovered ? `recovered by ${frame.recovery_engine || 'the engine'}` : '',
                        `fiducials ${formatPercent(frame.finder_score)}`,
                        new Date(frame.captured_at).toLocaleTimeString(),
                        'click for detail',
                      ]
                        .filter(Boolean)
                        .join('\n')}
                    </Box>
                  }
                >
                  <Box
                    role="button"
                    tabIndex={0}
                    onClick={() => setOpen(frame.id)}
                    onKeyDown={(event) => {
                      if (event.key === 'Enter' || event.key === ' ') {
                        event.preventDefault()
                        setOpen(frame.id)
                      }
                    }}
                    sx={{
                      cursor: 'pointer',
                      '&:focus-visible': { outline: 2, outlineColor: 'primary.main' },
                    }}
                  >
                    <Box
                      sx={{
                        border: 2,
                        borderColor: kind.colour,
                        borderRadius: 0.5,
                        bgcolor: '#000',
                        aspectRatio: '1',
                        overflow: 'hidden',
                        transition: 'transform 120ms ease',
                        '&:hover': { transform: 'scale(1.04)' },
                      }}
                    >
                      <Box
                        component="img"
                        loading="lazy"
                        src={api.frameImageUrl(frame.id)}
                        alt={`capture ${frame.sequence}`}
                        sx={{ width: '100%', height: '100%', objectFit: 'contain', display: 'block' }}
                      />
                    </Box>
                    <Typography
                      variant="caption"
                      sx={{ display: 'block', textAlign: 'center', mt: 0.25, color: kind.colour }}
                    >
                      {kind.label}
                    </Typography>
                  </Box>
                </Tooltip>
              )
            })}
          </Box>
        </>
      )}

      <FrameDetailDialog frameId={open} onClose={() => setOpen(null)} />
    </Paper>
  )
}
