import {
  Alert,
  Chip,
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

import { api, type TransmissionView } from '../api/client'

// Where the file went, and whether it got there.
//
// The receiver is the only side that can answer this. The callback URL crossed the optical channel inside
// the manifest, and the delivery was made from here — so the sender knows only what the receiver later told
// it in a signed result record. This panel is the primary source.
//
// Attempts are listed rather than reduced to a tick, because the ways a delivery fails are not
// interchangeable: a host that is not on the allowlist is a configuration decision, a 500 is the far end's
// problem, a timeout may succeed on the next try, and one not yet attempted is simply early.

export function Delivery({ transmission }: { transmission: TransmissionView }) {
  const deliveries = useQuery({
    queryKey: ['deliveries', transmission.transmission_id],
    queryFn: () => api.deliveries(transmission.transmission_id),
    refetchInterval: 3000,
  })

  const declared = transmission.callback_url
  const list = deliveries.data ?? []

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Typography variant="subtitle1" sx={{ mb: 1 }}>
        Delivery to the callback URL
      </Typography>

      {!declared && (
        <Typography variant="body2" color="text.secondary">
          This transfer named no callback URL, so nothing is delivered anywhere. The file is still verified
          and can be downloaded above.
        </Typography>
      )}

      {declared && (
        <Stack spacing={1.5}>
          <div>
            <Typography variant="caption" color="text.secondary" display="block">
              The URL the sender asked for, as it arrived in the manifest
            </Typography>
            <Typography variant="body2" sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>
              {declared}
            </Typography>
          </div>

          {list.length === 0 && (
            <Alert severity="info" variant="outlined">
              No delivery has been attempted yet. A merged file is delivered only once it has been verified
              against the hash the sender declared — and only to a host on this receiver's allowlist, because
              the URL came from outside its trust boundary.
            </Alert>
          )}

          {list.length > 0 && (
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Outcome</TableCell>
                  <TableCell>HTTP</TableCell>
                  <TableCell>Attempts</TableCell>
                  <TableCell>When</TableCell>
                  <TableCell>Detail</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {list.map((d, index) => (
                  <TableRow key={`${d.url}-${index}`}>
                    <TableCell>
                      <Chip
                        size="small"
                        color={
                          d.status === 'delivered'
                            ? 'success'
                            : d.status === 'failed'
                              ? 'error'
                              : 'default'
                        }
                        label={d.status}
                      />
                    </TableCell>
                    <TableCell>{d.http_status ?? '—'}</TableCell>
                    <TableCell>
                      {d.attempts} / {d.max_attempts}
                    </TableCell>
                    <TableCell>
                      {d.delivered_at ? new Date(d.delivered_at).toLocaleString() : '—'}
                    </TableCell>
                    <TableCell sx={{ maxWidth: 320, wordBreak: 'break-word' }}>
                      {d.last_error || '—'}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}

          <Typography variant="caption" color="text.secondary">
            The endpoint receives the file body with an <code>X-OTP-SHA256</code> header. A well-behaved one
            recomputes the hash for itself rather than trusting the header; the demonstration endpoint does,
            and returns 422 on a mismatch.
          </Typography>
        </Stack>
      )}
    </Paper>
  )
}
