import {
  Chip,
  Link,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { useQuery } from '@tanstack/react-query'
import { Link as RouterLink } from 'react-router-dom'

import { api, formatBytes, formatPercent } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { useUi } from '../store/ui'

export function Transmissions() {
  const refreshMs = useUi((state) => state.refreshMs)
  const transmissions = useQuery({
    queryKey: ['transmissions'],
    queryFn: api.transmissions,
    refetchInterval: refreshMs,
  })

  return (
    <Stack spacing={2}>
      <Typography variant="h5">Transmissions</Typography>
      <ErrorNotice error={transmissions.error} />

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>File</TableCell>
              <TableCell align="right">Size</TableCell>
              <TableCell sx={{ width: 160 }}>Chunks</TableCell>
              <TableCell align="right">Outstanding</TableCell>
              <TableCell align="right">From parity</TableCell>
              <TableCell>Verified</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {(transmissions.data ?? []).map((transmission) => (
              <TableRow key={transmission.transmission_id} hover>
                <TableCell>
                  <Link
                    component={RouterLink}
                    to={`/transmissions/${transmission.transmission_id}`}
                    underline="hover"
                  >
                    {transmission.filename}
                  </Link>
                </TableCell>
                <TableCell align="right">{formatBytes(transmission.original_size)}</TableCell>
                <TableCell>
                  {transmission.chunks_arrived} / {transmission.chunk_count} (
                  {formatPercent(transmission.progress)})
                </TableCell>
                <TableCell align="right">{transmission.missing_count}</TableCell>
                <TableCell align="right">{transmission.chunks_recovered}</TableCell>
                <TableCell>
                  {transmission.merged ? (
                    <Chip
                      size="small"
                      label={transmission.merged.verified ? 'verified' : 'failed'}
                      color={transmission.merged.verified ? 'success' : 'error'}
                    />
                  ) : (
                    <Chip size="small" label="in progress" />
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {(transmissions.data ?? []).length === 0 && (
          <Typography variant="body2" color="text.secondary">
            Nothing has arrived yet.
          </Typography>
        )}
      </Paper>
    </Stack>
  )
}
