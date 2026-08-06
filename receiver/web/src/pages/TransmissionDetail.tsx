import { useMemo } from 'react'
import {
  Alert,
  AlertTitle,
  Box,
  Button,
  Paper,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material'
import DownloadIcon from '@mui/icons-material/Download'
import { useQuery } from '@tanstack/react-query'
import { useParams } from 'react-router-dom'

import { api, formatBytes, formatPercent } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { Compare } from '../components/Compare'
import { Delivery } from '../components/Delivery'
import { FilePreview } from '../components/FilePreview'
import { Stat } from '../components/Stat'
import { useUi } from '../store/ui'

export function TransmissionDetail() {
  const { id = '' } = useParams()
  const refreshMs = useUi((state) => state.refreshMs)

  const transmission = useQuery({
    queryKey: ['transmission', id],
    queryFn: () => api.transmission(id),
    refetchInterval: refreshMs,
    enabled: Boolean(id),
  })
  const chunks = useQuery({
    queryKey: ['receiver-chunks', id],
    queryFn: () => api.chunks(id),
    refetchInterval: refreshMs * 2,
    enabled: Boolean(id),
  })
  const missing = useQuery({
    queryKey: ['missing', id],
    queryFn: () => api.missing(id),
    refetchInterval: refreshMs,
    enabled: Boolean(id),
  })

  const data = transmission.data
  const merged = data?.merged

  // Which chunks are here and which are not, drawn rather than counted. Recovered chunks are marked
  // differently because they arrived from parity rather than from a frame — an operator seeing many of them
  // is looking at a channel the error correction is carrying.
  const map = useMemo(() => {
    if (!data) return []
    const arrived = new Map((chunks.data ?? []).filter((c) => !c.is_parity).map((c) => [c.chunk_number, c]))
    return Array.from({ length: Math.min(data.chunk_count, 4096) }, (_, index) => {
      const chunk = arrived.get(index)
      return { index, present: Boolean(chunk), recovered: chunk?.recovered ?? false }
    })
  }, [data, chunks.data])

  return (
    <Stack spacing={3}>
      <Stack direction="row" alignItems="baseline" spacing={2}>
        <Typography variant="h5">{data?.filename ?? 'Transmission'}</Typography>
        <Typography variant="caption" color="text.secondary">
          {id}
        </Typography>
      </Stack>

      <ErrorNotice error={transmission.error} />

      {merged && (
        <Alert severity={merged.verified ? 'success' : 'error'} variant="outlined">
          <AlertTitle>
            {merged.verified
              ? 'Merged and verified against the hash the sender declared'
              : 'Merged, but it does not match the hash the sender declared'}
          </AlertTitle>
          <Stack spacing={1}>
            <Box>
              <Typography variant="caption" color="text.secondary">
                received
              </Typography>
              <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
                {merged.sha256}
              </Typography>
            </Box>
            <Box>
              <Typography variant="caption" color="text.secondary">
                expected
              </Typography>
              <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
                {data?.expected_sha256}
              </Typography>
            </Box>
            {merged.verify_error && <Typography variant="body2">{merged.verify_error}</Typography>}
            {merged.verified && (
              <Box>
                <Button
                  size="small"
                  variant="outlined"
                  startIcon={<DownloadIcon />}
                  href={api.downloadUrl(id)}
                >
                  Download the file
                </Button>
              </Box>
            )}
          </Stack>
        </Alert>
      )}

      <Grid container spacing={2}>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Chunks arrived"
            value={`${data?.chunks_arrived ?? 0} / ${data?.chunk_count ?? 0}`}
            hint={formatPercent(data?.progress ?? 0)}
            accent={data && data.chunks_arrived === data.chunk_count ? 'success' : 'primary'}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Outstanding"
            value={missing.data?.count ?? 0}
            hint="still waiting to be sent again"
            accent={(missing.data?.count ?? 0) > 0 ? 'warning' : 'success'}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Rebuilt from parity"
            value={data?.chunks_recovered ?? 0}
            hint="never arrived, reconstructed"
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <Stat
            label="Size"
            value={formatBytes(data?.original_size ?? 0)}
            hint={`chunks of ${formatBytes(data?.chunk_size ?? 0)}`}
          />
        </Grid>
      </Grid>

      {merged && <FilePreview transmissionId={id} merged={merged} />}

      {data && <Compare transmission={data} />}

      {data && <Delivery transmission={data} />}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Stack direction="row" alignItems="baseline" spacing={2} sx={{ mb: 1 }}>
          <Typography variant="subtitle1">Chunk map</Typography>
          <Typography variant="caption" color="text.secondary">
            filled means arrived · a ring means rebuilt from parity · empty means still missing
          </Typography>
        </Stack>
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: '3px' }}>
          {map.map((cell) => (
            <Tooltip
              key={cell.index}
              title={`chunk ${cell.index}${cell.recovered ? ' · rebuilt from parity' : cell.present ? '' : ' · missing'}`}
            >
              <Box
                sx={{
                  width: 11,
                  height: 11,
                  borderRadius: '2px',
                  bgcolor: cell.recovered ? 'transparent' : cell.present ? 'success.main' : 'transparent',
                  border: 2,
                  borderColor: cell.recovered ? 'warning.main' : cell.present ? 'success.main' : 'divider',
                }}
              />
            </Tooltip>
          ))}
        </Box>
      </Paper>

      {(missing.data?.missing ?? []).length > 0 && (
        <Paper variant="outlined" sx={{ p: 2, borderColor: 'warning.main' }}>
          <Typography variant="subtitle2" sx={{ mb: 1 }}>
            Outstanding chunks
          </Typography>
          <Typography variant="body2" sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>
            {(missing.data?.missing ?? []).slice(0, 400).join(', ')}
          </Typography>
        </Paper>
      )}
    </Stack>
  )
}
