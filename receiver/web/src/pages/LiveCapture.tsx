import { Alert, LinearProgress, Paper, Stack, Typography } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api, formatBytes, formatDuration, formatPercent } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { LiveFrames } from '../components/LiveFrames'
import { Stat } from '../components/Stat'
import { useUi } from '../store/ui'

// The receiver's front page answers what an operator standing next to the camera wants to know: is anything
// arriving, is it being read, and how well. The decode rate is the figure that matters most — it falls
// before frames start failing outright, which makes it the earliest warning that the camera needs attention.
export function LiveCapture() {
  const refreshMs = useUi((state) => state.refreshMs)

  const session = useQuery({ queryKey: ['session'], queryFn: api.session, refetchInterval: refreshMs })
  const transmissions = useQuery({
    queryKey: ['transmissions'],
    queryFn: api.transmissions,
    refetchInterval: refreshMs,
  })

  const data = session.data
  const capturing = data && data.capturing !== false
  const active = (transmissions.data ?? []).filter((t) => !t.merged?.verified)

  return (
    <Stack spacing={3}>
      <Typography variant="h5">Live capture</Typography>
      <ErrorNotice error={session.error ?? transmissions.error} />

      {!capturing && (
        <Alert severity="warning">
          No capture session is running. The receiver is not watching the channel.
        </Alert>
      )}

      {capturing && data && (
        <Grid container spacing={2}>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Frames captured"
              value={data.frames_captured}
              hint={`over ${formatDuration(data.uptime_seconds)}`}
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Decode rate"
              value={formatPercent(data.decode_rate)}
              hint="frames read successfully"
              accent={data.decode_rate > 0.95 ? 'success' : data.decode_rate > 0.7 ? 'warning' : 'error'}
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Unreadable"
              value={data.frames_failed}
              hint="kept for inspection"
              accent={data.frames_failed > 0 ? 'warning' : 'success'}
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Source" value={data.source} hint={data.status} />
          </Grid>
        </Grid>
      )}

      {/* The counters above answer "is it working"; this answers "is it working now". A count that stopped
          moving looks exactly like one moving slowly, and that difference is the whole question when a camera
          has just been aimed. */}
      <LiveFrames />

      <Paper variant="outlined" sx={{ p: 2 }}>
        {/* Files, as opposed to the frames panel above. Both were called "Arriving now", which read as one
            section repeated rather than two different questions: which transfers are in flight, and which frames
            landed in the last second. */}
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Transfers in progress
        </Typography>
        {active.length === 0 ? (
          <Typography variant="body2" color="text.secondary">
            No transfer is in progress. The channel is quiet.
          </Typography>
        ) : (
          <Stack spacing={2}>
            {active.map((transmission) => (
              <Paper key={transmission.transmission_id} variant="outlined" sx={{ p: 2 }}>
                <Stack spacing={1}>
                  <Stack direction="row" justifyContent="space-between" alignItems="baseline">
                    <Typography variant="subtitle2">{transmission.filename}</Typography>
                    <Typography variant="caption" color="text.secondary">
                      {formatBytes(transmission.original_size)} · {transmission.transmission_id.slice(0, 8)}
                    </Typography>
                  </Stack>
                  <LinearProgress variant="determinate" value={Math.min(transmission.progress * 100, 100)} />
                  <Typography variant="caption" color="text.secondary">
                    {transmission.chunks_arrived} of {transmission.chunk_count} chunks ·{' '}
                    {transmission.missing_count} outstanding
                    {transmission.chunks_recovered > 0 &&
                      ` · ${transmission.chunks_recovered} rebuilt from parity`}
                  </Typography>
                </Stack>
              </Paper>
            ))}
          </Stack>
        )}
      </Paper>
    </Stack>
  )
}
