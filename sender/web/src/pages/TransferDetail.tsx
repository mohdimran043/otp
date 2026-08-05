import { useMemo } from 'react'
import {
  Alert,
  AlertTitle,
  Box,
  Chip,
  LinearProgress,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material'
import { useQuery } from '@tanstack/react-query'
import { useParams } from 'react-router-dom'

import { api, eta, formatBytes, formatDuration, formatRate } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { Stat } from '../components/Stat'
import { StatusChip } from '../components/StatusChip'
import { useUi } from '../store/ui'

export function TransferDetail() {
  const { id = '' } = useParams()
  const refreshMs = useUi((state) => state.refreshMs)

  const transfer = useQuery({
    queryKey: ['transfer', id],
    queryFn: () => api.transfer(id),
    refetchInterval: refreshMs,
    enabled: Boolean(id),
  })
  const chunks = useQuery({
    queryKey: ['chunks', id],
    queryFn: () => api.chunks(id),
    refetchInterval: refreshMs * 2,
    enabled: Boolean(id),
  })
  const jobs = useQuery({
    queryKey: ['jobs', id],
    queryFn: () => api.jobs(id),
    refetchInterval: refreshMs,
    enabled: Boolean(id),
  })
  const frames = useQuery({
    queryKey: ['frames', id],
    queryFn: () => api.frames(id),
    // Frames do not change once rendered, so this is fetched once rather than polled: a transmission can
    // have tens of thousands of them and re-fetching that on a timer would be the heaviest thing the UI does.
    enabled: Boolean(id),
  })

  const status = transfer.data
  const result = status?.result

  // The chunk map is the picture an operator actually wants during a transfer: which parts have arrived
  // and which have not, at a glance, rather than a count. Parity shards are drawn differently because a
  // missing one is not a gap in the file.
  const grid = useMemo(() => {
    const list = chunks.data ?? []
    return list.slice(0, 4096).map((chunk) => ({
      key: chunk.id,
      acked: chunk.acked,
      parity: chunk.is_parity,
      esi: chunk.esi,
      retries: chunk.retry_count,
    }))
  }, [chunks.data])

  const elapsed = result?.seconds ?? 0

  return (
    <Stack spacing={3}>
      <Stack direction="row" alignItems="center" spacing={2}>
        <Typography variant="h5">{status?.filename ?? 'Transfer'}</Typography>
        {status && <StatusChip status={status.status} />}
        <Typography variant="caption" color="text.secondary">
          {id}
        </Typography>
      </Stack>

      <ErrorNotice error={transfer.error} />

      {status?.error && (
        <Alert severity="error">
          <AlertTitle>This transfer failed</AlertTitle>
          {status.error}
        </Alert>
      )}

      {result && (
        <Alert severity={result.verified ? 'success' : 'error'} variant="outlined">
          <AlertTitle>
            {result.verified
              ? 'The receiver merged the file and verified it against the sender’s hash'
              : 'The receiver could not verify the merged file'}
          </AlertTitle>
          <Stack spacing={0.5}>
            <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
              {result.sha256}
            </Typography>
            <Typography variant="body2">
              {formatBytes(result.size)} in {formatDuration(result.seconds)} ·{' '}
              {formatRate(result.bytes_per_second)} · {result.chunks_received} chunks
              {result.chunks_recovered > 0 && `, ${result.chunks_recovered} rebuilt from parity`}
            </Typography>
            <Typography variant="body2">
              {result.callback_delivered
                ? `Delivered to the callback URL (HTTP ${result.callback_status})`
                : status?.callback_url
                  ? `Not delivered: ${result.callback_error ?? 'unknown reason'}`
                  : 'No callback was requested'}
            </Typography>
            {result.error && <Typography variant="body2">{result.error}</Typography>}
          </Stack>
        </Alert>
      )}

      <Grid container spacing={2}>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Acknowledged"
            value={`${status?.acked_chunks ?? 0} / ${status?.chunk_count ?? 0}`}
            hint="chunks the receiver confirmed"
            accent={status && status.acked_chunks === status.chunk_count ? 'success' : 'primary'}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Speed"
            value={result ? formatRate(result.bytes_per_second) : '—'}
            hint={result ? `over ${formatDuration(result.seconds)}` : 'measured when the receiver reports'}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Time remaining"
            value={status ? eta(status, elapsed || 1) : '—'}
            hint="from the rate achieved so far"
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Retransmissions"
            value={status?.retransmits ?? 0}
            hint="frames shown again after a timeout"
            accent={status && status.retransmits > 0 ? 'warning' : undefined}
          />
        </Grid>

        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat label="Original" value={formatBytes(status?.original_size ?? 0)} />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Compressed"
            value={formatBytes(status?.compressed_size ?? 0)}
            hint={
              status && status.original_size > 0 && status.compressed_size > 0
                ? `${((status.compressed_size / status.original_size) * 100).toFixed(1)}% of the original`
                : undefined
            }
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat label="Chunk size" value={formatBytes(status?.chunk_size ?? 0)} hint="one chunk fills one frame" />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Frames"
            value={status?.frame_count ?? 0}
            hint={`${status?.encoder ?? '—'} · ${status?.compression ?? '—'} · ${status?.fec_codec ?? '—'}`}
          />
        </Grid>
      </Grid>

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Preparation
        </Typography>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Stage</TableCell>
              <TableCell>Status</TableCell>
              <TableCell sx={{ width: 200 }}>Progress</TableCell>
              <TableCell>Message</TableCell>
              <TableCell align="right">Attempts</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {(jobs.data ?? []).map((job) => (
              <TableRow key={job.id} hover>
                <TableCell>{job.type}</TableCell>
                <TableCell>
                  <Chip
                    size="small"
                    label={job.status}
                    color={
                      job.status === 'completed'
                        ? 'success'
                        : job.status === 'failed'
                          ? 'error'
                          : job.status === 'running'
                            ? 'primary'
                            : 'default'
                    }
                  />
                </TableCell>
                <TableCell>
                  <LinearProgress variant="determinate" value={job.progress} />
                </TableCell>
                <TableCell>
                  <Typography variant="caption">{job.error || job.message}</Typography>
                </TableCell>
                <TableCell align="right">
                  {job.attempts} / {job.max_attempts}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Paper>

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Stack direction="row" alignItems="baseline" spacing={2} sx={{ mb: 1 }}>
          <Typography variant="subtitle1">Chunks</Typography>
          <Typography variant="caption" color="text.secondary">
            filled means acknowledged · outlined means still outstanding · a dot marks a parity shard
          </Typography>
        </Stack>
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: '3px' }}>
          {grid.map((chunk) => (
            <Tooltip
              key={chunk.key}
              title={`chunk ${chunk.esi}${chunk.parity ? ' (parity)' : ''}${
                chunk.retries > 0 ? ` · ${chunk.retries} retries` : ''
              }`}
            >
              <Box
                sx={{
                  width: 11,
                  height: 11,
                  borderRadius: chunk.parity ? '50%' : '2px',
                  bgcolor: chunk.acked ? 'success.main' : 'transparent',
                  border: 1,
                  borderColor: chunk.acked ? 'success.main' : chunk.retries > 0 ? 'warning.main' : 'divider',
                }}
              />
            </Tooltip>
          ))}
        </Box>
        {(chunks.data ?? []).length > 4096 && (
          <Typography variant="caption" color="text.secondary">
            showing the first 4096 of {(chunks.data ?? []).length}
          </Typography>
        )}
      </Paper>

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Frames
        </Typography>
        <Typography variant="body2" color="text.secondary">
          {(frames.data ?? []).length} rendered ·{' '}
          {(frames.data ?? []).filter((f) => f.is_manifest).length} of them manifests ·{' '}
          {(frames.data ?? []).reduce((sum, f) => sum + f.displayed_count, 0)} displays in total
        </Typography>
      </Paper>
    </Stack>
  )
}
