import { Alert, Box, Chip, Paper, Stack, Tooltip, Typography } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api, formatPercent, type CapturedFrame } from '../api/client'
import { useUi } from '../store/ui'

// Frames arriving, as they arrive.
//
// The counters above this say how many frames were captured and what fraction decoded, which answers "is it
// working" but not "is it working *now*". A count that stopped moving looks exactly like a count that is moving
// slowly, and the difference is the whole question when a camera has just been aimed.
//
// So this is the newest captures, newest first, refreshing on the interval the operator chose. Each one is the
// stored image — the bytes the decoder was actually given, not a re-render — with the chunk it carried and the
// confidence it was read at. A frame that failed is shown too, and shown differently: a page that displayed only
// successes would look healthy while a camera drifted out of focus.

/** kindOf sorts a capture into what an operator needs to distinguish at a glance. */
function kindOf(frame: CapturedFrame): { label: string; colour: 'success' | 'secondary' | 'warning' | 'error' } {
  if (!frame.decoded) return { label: 'unreadable', colour: 'error' }
  if (frame.is_manifest) return { label: 'manifest', colour: 'secondary' }
  if (frame.is_parity) return { label: `parity ${frame.chunk_number ?? '?'}`, colour: 'warning' }
  return { label: `chunk ${frame.chunk_number ?? '?'}`, colour: 'success' }
}

export function LiveFrames() {
  const { refreshMs } = useUi()
  const frames = useQuery({
    queryKey: ['recent-frames'],
    queryFn: () => api.recentFrames(48),
    refetchInterval: refreshMs,
  })

  const list = frames.data ?? []
  const decoded = list.filter((f) => f.decoded).length

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Stack direction="row" alignItems="baseline" spacing={2} sx={{ mb: 1 }} flexWrap="wrap" useFlexGap>
        <Typography variant="subtitle1">Frames arriving</Typography>
        <Typography variant="caption" color="text.secondary" sx={{ flexGrow: 1 }}>
          the newest captures, newest first — refreshing every {refreshMs}ms
        </Typography>
        {list.length > 0 && (
          <Chip
            size="small"
            color={decoded === list.length ? 'success' : 'warning'}
            label={`${decoded} of ${list.length} readable`}
          />
        )}
      </Stack>

      {list.length === 0 && (
        <Alert severity="info" variant="outlined">
          Nothing captured yet. The camera is open and waiting: a frame with nothing in it is not recorded, so
          this stays empty until the sender puts something on the display. Point the camera at the sender's{' '}
          <strong>Display</strong> page and start a transfer.
        </Alert>
      )}

      {list.length > 0 && (
        <Box
          sx={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fill, minmax(104px, 1fr))',
            gap: 1,
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
                      frame.decoded ? kind.label : `unreadable: ${frame.decode_error ?? 'unknown'}`,
                      `fiducials ${formatPercent(frame.finder_score)} · timing ${formatPercent(frame.timing_score)}`,
                      `contrast ${formatPercent(frame.contrast)} · bit errors ${formatPercent(frame.bit_error_rate)}`,
                      new Date(frame.captured_at).toLocaleTimeString(),
                    ].join('\n')}
                  </Box>
                }
              >
                <Box>
                  <Box
                    sx={{
                      border: 2,
                      borderColor: `${kind.colour}.main`,
                      borderRadius: 0.5,
                      bgcolor: '#000',
                      aspectRatio: '1',
                      overflow: 'hidden',
                    }}
                  >
                    {/* The stored capture, served straight from the object store: what the decoder was given,
                        rather than something re-rendered from what it concluded. */}
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
                    color={frame.decoded ? 'text.secondary' : 'error.main'}
                    sx={{ display: 'block', textAlign: 'center', mt: 0.25 }}
                  >
                    {kind.label}
                  </Typography>
                </Box>
              </Tooltip>
            )
          })}
        </Box>
      )}
    </Paper>
  )
}
