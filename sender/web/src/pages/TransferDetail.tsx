import { useMemo, useState } from 'react'
import {
  Alert,
  AlertTitle,
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
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
import DeleteIcon from '@mui/icons-material/Delete'
import PlayArrowIcon from '@mui/icons-material/PlayArrow'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useParams } from 'react-router-dom'

import { api, eta, formatBytes, formatDuration, formatRate, type TransferStatus } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { FrameAudit } from '../components/FrameAudit'
import { Grid } from '../components/Grid'
import { SentFile } from '../components/SentFile'
import { Stat } from '../components/Stat'
import { StatusChip } from '../components/StatusChip'
import { TransferControls } from '../components/TransferControls'
import { useUi } from '../store/ui'
import { mono } from '../theme'

/** Spec is one settled fact about how this transfer was built. */
function Spec({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <Stack spacing={0.25}>
      <Typography variant="caption" color="text.secondary">
        {label}
      </Typography>
      <Typography sx={{ fontFamily: mono, fontSize: '0.82rem' }}>{value}</Typography>
      {hint && (
        <Typography variant="caption" color="text.secondary" sx={{ textTransform: 'none', letterSpacing: 0 }}>
          {hint}
        </Typography>
      )}
    </Stack>
  )
}

/**
 * Profile is what this transfer was actually encoded at.
 *
 * Every figure here was fixed when the file was uploaded and then written into every frame, so none of it
 * can be inferred from the settings page — those describe what the *next* transfer will use. A transfer
 * that read badly is diagnosed by its geometry, and until now the only way to know what a given transfer
 * was sent at was to remember what the form said when you filled it in.
 *
 * Pixels per cell is the figure that decides whether a capture can be read at all, so the frame's rendered
 * edge is given beside the grid rather than left as arithmetic for the reader.
 */
function Profile({ status }: { status: TransferStatus | undefined }) {
  if (!status) return null

  const cells = Math.max(status.grid_width, status.grid_height)
  const edge = cells > 0 && status.cell_pixels > 0 ? cells * status.cell_pixels : 0

  // A ratio, because that is how the pair is meant to be read: 15 per 100 is 15% redundancy. The count
  // actually emitted is scaled down for a transfer smaller than one block, which is why the frame and
  // chunk counts above can imply less parity than this suggests.
  const parity =
    status.fec_codec === 'none' || status.fec_parity_shards === 0
      ? 'none — a dropped frame cannot be repaired'
      : `${status.fec_codec} · ${status.fec_parity_shards} per ${status.fec_data_shards} ` +
        `(${Math.round((status.fec_parity_shards / Math.max(status.fec_data_shards, 1)) * 100)}% redundancy)`

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Stack direction="row" alignItems="baseline" spacing={2} sx={{ mb: 1.5 }} flexWrap="wrap" useFlexGap>
        <Typography variant="subtitle1">Profile</Typography>
        <Typography variant="caption" color="text.secondary">
          settled at upload and written into every frame — it cannot change for this transfer
        </Typography>
      </Stack>

      {/* Said explicitly, because its absence here would otherwise look like an omission. How many frames
          are tiled onto the panel at once is not a property of a transfer: every lane is an ordinary
          frame, nothing per-transfer records a lane count, and the display's setting decides it — and can
          be changed while this transfer is running. It belongs on the display page and only there. */}
      <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 1.5, textTransform: 'none', letterSpacing: 0 }}>
        Tiling is not listed: how many frames share the panel is the display's setting, not this
        transfer's, and it can be changed mid-transfer.
      </Typography>

      <Grid container spacing={2}>
        <Grid size={{ xs: 6, sm: 4, md: 3 }}>
          <Spec
            label="grid"
            value={`${status.grid_width}×${status.grid_height} cells`}
            hint={edge > 0 ? `${edge}×${edge} px rendered` : undefined}
          />
        </Grid>
        <Grid size={{ xs: 6, sm: 4, md: 3 }}>
          <Spec
            label="cell size"
            value={`${status.cell_pixels} px`}
            hint="the figure a camera has to resolve"
          />
        </Grid>
        <Grid size={{ xs: 6, sm: 4, md: 3 }}>
          <Spec label="quiet zone" value={`${status.quiet_zone} cells`} hint="the margin around the grid" />
        </Grid>
        <Grid size={{ xs: 6, sm: 4, md: 3 }}>
          <Spec
            label="encoding"
            value={status.encoder}
            hint={status.bit_depth > 0 ? `${status.bit_depth} bit${status.bit_depth === 1 ? '' : 's'} per cell` : undefined}
          />
        </Grid>
        <Grid size={{ xs: 6, sm: 4, md: 3 }}>
          <Spec
            label="compression"
            value={status.compression === 'none' ? 'none' : `${status.compression} level ${status.compression_level}`}
            hint={
              status.original_size > 0 && status.compressed_size > 0
                ? `${formatBytes(status.original_size)} → ${formatBytes(status.compressed_size)}`
                : undefined
            }
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 8, md: 3 }}>
          <Spec label="loss protection" value={parity} />
        </Grid>
        <Grid size={{ xs: 6, sm: 4, md: 3 }}>
          <Spec
            label="encryption"
            value={status.encryption}
            hint={
              status.encryption === 'none'
                ? 'the payload crossed the gap in the clear'
                : 'the payload only; a manifest is never encrypted'
            }
          />
        </Grid>
      </Grid>
    </Paper>
  )
}

export function TransferDetail() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const client = useQueryClient()
  const refreshMs = useUi((state) => state.refreshMs)
  const [confirmingDelete, setConfirmingDelete] = useState(false)

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

  const startTransfer = useMutation({
    mutationFn: () => api.startTransfer(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['transfer', id] })
      void client.invalidateQueries({ queryKey: ['transfers'] })
    },
  })

  const deleteTransfer = useMutation({
    mutationFn: () => api.deleteTransfer(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['transfers'] })
      navigate('/transfers')
    },
  })

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
      <Stack direction="row" alignItems="center" spacing={2} flexWrap="wrap" useFlexGap>
        <Typography variant="h5">{status?.filename ?? 'Transfer'}</Typography>
        {status && <StatusChip status={status.status} />}
        <Typography variant="caption" color="text.secondary" sx={{ flexGrow: 1 }}>
          {id}
        </Typography>
        {status?.status === 'ready' && (
          <Button
            size="small"
            variant="contained"
            startIcon={<PlayArrowIcon />}
            disabled={startTransfer.isPending}
            onClick={() => startTransfer.mutate()}
          >
            {startTransfer.isPending ? 'Starting…' : 'Start'}
          </Button>
        )}
        {id && status && (
          <TransferControls
            transmissionId={id}
            status={status.status}
            ackedChunks={status.acked_chunks}
            chunkCount={status.chunk_count}
          />
        )}
        <Button
          size="small"
          variant="outlined"
          color="error"
          startIcon={<DeleteIcon />}
          onClick={() => setConfirmingDelete(true)}
        >
          Delete
        </Button>
      </Stack>

      <ErrorNotice error={transfer.error} />
      <ErrorNotice error={startTransfer.error} />

      <Dialog open={confirmingDelete} onClose={() => setConfirmingDelete(false)}>
        <DialogTitle>Delete this transfer?</DialogTitle>
        <DialogContent>
          <ErrorNotice error={deleteTransfer.error} />
          <DialogContentText component="div">
            This removes the transfer entirely — the row, its chunks, and every frame the pipeline
            wrote for it — rather than only marking it cancelled. There is no undo.
            <br />
            <br />
            {status && ['preparing', 'transmitting', 'paused'].includes(status.status) ? (
              <>
                This transfer is currently <strong>{status.status}</strong>, so the server will refuse
                this until it is cancelled first.
              </>
            ) : (
              'It is safe to delete: nothing is actively using it right now.'
            )}
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirmingDelete(false)}>Keep it</Button>
          <Button
            color="error"
            variant="contained"
            disabled={deleteTransfer.isPending}
            onClick={() => deleteTransfer.mutate()}
          >
            {deleteTransfer.isPending ? 'Deleting…' : 'Delete the transfer'}
          </Button>
        </DialogActions>
      </Dialog>

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

      <Profile status={status} />

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

      {id && transfer.data && (
        <SentFile
          transmissionId={id}
          filename={transfer.data.filename}
          sizeBytes={transfer.data.original_size}
          sha256={transfer.data.sha256}
        />
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Stack direction="row" alignItems="center" spacing={2} sx={{ mb: 1 }}>
          <Typography variant="subtitle1">Frames</Typography>
          <Box sx={{ flexGrow: 1 }} />
          <Button
            size="small"
            variant="outlined"
            component="a"
            href={api.frameArchiveUrl(id)}
            disabled={(transfer.data?.frame_count ?? 0) === 0}
          >
            Download frames
          </Button>
          {/* The same frames as paper. An anchor rather than a fetch: the document belongs to the
              browser's own download handling, and a large one should not be held open in a promise. */}
          <Tooltip title="One frame to a page, captioned and sized to print. Hold the sheets up to a camera to test the optical path with no display at all.">
            <span>
              <Button
                size="small"
                variant="outlined"
                component="a"
                href={api.framePrintableUrl(id)}
                disabled={(transfer.data?.frame_count ?? 0) === 0}
              >
                Print as PDF
              </Button>
            </span>
          </Tooltip>
        </Stack>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
          {(frames.data ?? []).length} rendered ·{' '}
          {(frames.data ?? []).filter((f) => f.is_manifest).length} of them manifests ·{' '}
          {(frames.data ?? []).reduce((sum, f) => sum + f.displayed_count, 0)} displays in total ·
          every one kept, so any of them can be inspected after the fact
        </Typography>
        {id && (
          <FrameAudit
            transmissionId={id}
            frames={frames.data ?? []}
            chunks={chunks.data ?? []}
          />
        )}
      </Paper>
    </Stack>
  )
}
