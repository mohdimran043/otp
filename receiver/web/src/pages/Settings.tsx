import { Alert, Paper, Stack, Table, TableBody, TableCell, TableRow, Typography } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api, formatPercent } from '../api/client'
import { CameraPicker } from '../components/CameraPicker'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { Stat } from '../components/Stat'
import { useUi } from '../store/ui'

// What the decoder is doing, and which of it can be changed without a restart.
//
// The confidence floors are the interesting pair: they are the receiver's own policy rather than the
// protocol's, and they are reloadable precisely because tuning a marginal camera means trying a threshold
// and watching what happens.
export function Settings() {
  const { refreshMs, setRefreshMs } = useUi()
  const config = useQuery({ queryKey: ['config'], queryFn: api.config })
  const data = config.data

  return (
    <Stack spacing={3}>
      <Typography variant="h5">Capture</Typography>
      <ErrorNotice error={config.error} />

      <CameraPicker />

      <Typography variant="h5">Decoder</Typography>

      {data && (
        <Grid container spacing={2}>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Protocol version" value={data.protocol_version} />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Capture source" value={data.capture.source} hint={data.capture.dir} />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Decoding at once"
              value={`${data.capture.decode_workers_now} frames`}
              hint={
                data.capture.decode_workers > 0
                  ? 'configured'
                  : 'one per core, less one — set OTP_RECEIVER_CAPTURE_DECODE_WORKERS to override'
              }
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Deepest backlog"
              value={`${data.capture.frames_behind.toLocaleString()} frames`}
              hint="1 means it kept up; a large number means the display is ahead of the decoder"
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Fiducial floor"
              value={formatPercent(data.decoder.min_finder_score)}
              hint="frames below this are discarded unread"
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Payload encryption"
              value={data.decoder.encrypted ? 'on' : 'off'}
              hint={data.decoder.encrypted ? 'a key is configured' : 'payloads arrive in the clear'}
            />
          </Grid>
        </Grid>
      )}

      {data && (
        <Paper variant="outlined" sx={{ p: 2 }}>
          <Typography variant="subtitle1" sx={{ mb: 1 }}>
            Callback delivery
          </Typography>
          {data.callback.allow_any_host ? (
            <Alert severity="warning" variant="outlined">
              This receiver will deliver a merged file to any host the sender names. The URL crosses the
              optical channel from outside this machine, so an allowlist is the only thing that stops it
              being used to reach somewhere it should not.
            </Alert>
          ) : (data.callback.allowed_hosts ?? []).length === 0 ? (
            <Alert severity="info" variant="outlined">
              No hosts are allowed, so no merged file will be delivered anywhere. Files are still received,
              verified, and downloadable from here.
            </Alert>
          ) : (
            <Table size="small">
              <TableBody>
                {(data.callback.allowed_hosts ?? []).map((host) => (
                  <TableRow key={host}>
                    <TableCell>{host}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </Paper>
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          This browser
        </Typography>
        <Table size="small">
          <TableBody>
            <TableRow>
              <TableCell sx={{ width: 200 }}>Refresh interval</TableCell>
              <TableCell>
                {[500, 1000, 2000, 5000].map((ms) => (
                  <Typography
                    key={ms}
                    component="span"
                    variant="body2"
                    onClick={() => setRefreshMs(ms)}
                    sx={{
                      mr: 2,
                      cursor: 'pointer',
                      fontWeight: refreshMs === ms ? 700 : 400,
                      textDecoration: refreshMs === ms ? 'underline' : 'none',
                    }}
                  >
                    {ms / 1000}s
                  </Typography>
                ))}
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </Paper>
    </Stack>
  )
}
