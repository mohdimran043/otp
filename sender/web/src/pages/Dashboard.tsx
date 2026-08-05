import { Box, Paper, Stack, Typography } from '@mui/material'
import { useQuery } from '@tanstack/react-query'
import { Link as RouterLink } from 'react-router-dom'
import { Button } from '@mui/material'

import { api, formatBytes, formatRate } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { Stat } from '../components/Stat'
import { StatusChip } from '../components/StatusChip'
import { TransferTable } from '../components/TransferTable'
import { useUi } from '../store/ui'

// The dashboard answers three questions in order: is anything moving, is anything stuck, and what has
// finished. That ordering is why the active transfers come first and the history last — an operator opens
// this page because something is happening now, not to browse.
export function Dashboard() {
  const refreshMs = useUi((state) => state.refreshMs)

  const transfers = useQuery({
    queryKey: ['transfers'],
    queryFn: () => api.transfers(),
    refetchInterval: refreshMs,
  })

  const list = transfers.data ?? []
  const active = list.filter((t) => ['preparing', 'ready', 'transmitting'].includes(t.status))
  const failed = list.filter((t) => t.status === 'failed')
  const completed = list.filter((t) => t.status === 'completed')

  const bytesInFlight = active.reduce((sum, t) => sum + t.original_size, 0)
  const chunksOutstanding = active.reduce((sum, t) => sum + Math.max(t.chunk_count - t.acked_chunks, 0), 0)
  const retransmits = list.reduce((sum, t) => sum + t.retransmits, 0)

  return (
    <Stack spacing={3}>
      <Stack direction="row" alignItems="center" justifyContent="space-between">
        <Typography variant="h5">Dashboard</Typography>
        <Button component={RouterLink} to="/send" variant="contained">
          Send a file
        </Button>
      </Stack>

      <ErrorNotice error={transfers.error} />

      <Grid container spacing={2}>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat label="Transmitting" value={active.length} hint={formatBytes(bytesInFlight) + ' in flight'} accent="primary" />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Chunks outstanding"
            value={chunksOutstanding}
            hint="not yet acknowledged by the receiver"
            accent={chunksOutstanding > 0 ? 'warning' : undefined}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Retransmissions"
            value={retransmits}
            hint="frames shown again after a timeout"
            accent={retransmits > 0 ? 'warning' : undefined}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Failed"
            value={failed.length}
            hint={failed.length ? 'needs an operator' : 'nothing to look at'}
            accent={failed.length ? 'error' : 'success'}
          />
        </Grid>
      </Grid>

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          In progress
        </Typography>
        {active.length === 0 ? (
          <Typography variant="body2" color="text.secondary">
            Nothing is transmitting. The display is idle.
          </Typography>
        ) : (
          <TransferTable transfers={active} />
        )}
      </Paper>

      {failed.length > 0 && (
        <Paper variant="outlined" sx={{ p: 2, borderColor: 'error.main' }}>
          <Typography variant="subtitle1" sx={{ mb: 1 }}>
            Failed
          </Typography>
          <TransferTable transfers={failed} />
        </Paper>
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Stack direction="row" alignItems="center" justifyContent="space-between" sx={{ mb: 1 }}>
          <Typography variant="subtitle1">Completed</Typography>
          <Button component={RouterLink} to="/transfers" size="small">
            All transfers
          </Button>
        </Stack>
        {completed.length === 0 ? (
          <Typography variant="body2" color="text.secondary">
            No completed transfers yet.
          </Typography>
        ) : (
          <TransferTable transfers={completed.slice(0, 8)} />
        )}
      </Paper>

      <Box>
        <Stack direction="row" spacing={1} alignItems="center">
          <Typography variant="caption" color="text.secondary">
            Newest first · refreshing every {(refreshMs / 1000).toFixed(1)}s ·
          </Typography>
          {list.length > 0 && (
            <>
              <StatusChip status={list[0]!.status} />
              <Typography variant="caption" color="text.secondary">
                {formatRate(0) === '—' ? '' : ''}
              </Typography>
            </>
          )}
        </Stack>
      </Box>
    </Stack>
  )
}
