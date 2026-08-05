import {
  LinearProgress,
  Link,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { Link as RouterLink } from 'react-router-dom'

import { formatBytes, type Transmission } from '../api/client'
import { StatusChip } from './StatusChip'

// One table, used by every list, so a transfer looks the same wherever it appears.
//
// Progress is drawn from acknowledged chunks rather than frames displayed, because that is what has
// actually arrived: a bar driven by frames would run ahead and then appear to go backwards as
// retransmissions were counted.
export function TransferTable({ transfers }: { transfers: Transmission[] }) {
  return (
    <Table size="small">
      <TableHead>
        <TableRow>
          <TableCell>Transfer</TableCell>
          <TableCell>Status</TableCell>
          <TableCell align="right">Size</TableCell>
          <TableCell align="right">Compressed</TableCell>
          <TableCell>Profile</TableCell>
          <TableCell sx={{ width: 220 }}>Acknowledged</TableCell>
          <TableCell align="right">Resent</TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        {transfers.map((transfer) => {
          const progress = transfer.chunk_count > 0 ? (transfer.acked_chunks / transfer.chunk_count) * 100 : 0
          return (
            <TableRow key={transfer.id} hover>
              <TableCell>
                <Link component={RouterLink} to={`/transfers/${transfer.id}`} underline="hover">
                  {transfer.id.slice(0, 8)}
                </Link>
              </TableCell>
              <TableCell>
                <StatusChip status={transfer.status} />
              </TableCell>
              <TableCell align="right">{formatBytes(transfer.original_size)}</TableCell>
              <TableCell align="right">
                {transfer.compressed_size > 0 ? formatBytes(transfer.compressed_size) : '—'}
              </TableCell>
              <TableCell>
                <Typography variant="caption" color="text.secondary">
                  {transfer.encoder} · {transfer.compression} · {transfer.fec_codec}
                </Typography>
              </TableCell>
              <TableCell>
                <Stack spacing={0.5}>
                  <LinearProgress
                    variant="determinate"
                    value={Math.min(progress, 100)}
                    color={transfer.status === 'failed' ? 'error' : 'primary'}
                  />
                  <Typography variant="caption" color="text.secondary">
                    {transfer.acked_chunks} / {transfer.chunk_count} chunks
                  </Typography>
                </Stack>
              </TableCell>
              <TableCell align="right">{transfer.retransmits}</TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
