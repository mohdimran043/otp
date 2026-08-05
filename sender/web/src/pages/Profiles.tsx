import { Paper, Stack, Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { Stat } from '../components/Stat'

// What this build can actually do, read from the server.
//
// It is a page rather than a hard-coded list because the answer belongs to the binary that is running: a
// deployment on an older build offers fewer encodings, and a UI that claimed otherwise would let an
// operator choose one that does not exist.
export function Profiles() {
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: api.profiles })
  const data = profiles.data

  return (
    <Stack spacing={3}>
      <Typography variant="h5">Profiles</Typography>
      <ErrorNotice error={profiles.error} />

      {data && (
        <Grid container spacing={2}>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Protocol version" value={data.protocol_version} />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Grid" value={data.defaults.grid} hint={`${data.defaults.cell_pixels}px cells`} />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Frame rate" value={`${data.defaults.fps}/s`} hint="reloadable while running" />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Defaults"
              value={data.defaults.encoder}
              hint={`${data.defaults.compression} · ${data.defaults.fec_codec}`}
            />
          </Grid>
        </Grid>
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Optical encodings
        </Typography>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Name</TableCell>
              <TableCell>Bit depths</TableCell>
              <TableCell>Description</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {(data?.encoders ?? []).map((encoder) => (
              <TableRow key={encoder.name} hover>
                <TableCell>{encoder.name}</TableCell>
                <TableCell>{encoder.bit_depths.join(', ')}</TableCell>
                <TableCell>
                  <Typography variant="caption">{encoder.description}</Typography>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Paper>

      <Grid container spacing={2}>
        <Grid size={{ xs: 12, md: 6 }}>
          <Paper variant="outlined" sx={{ p: 2 }}>
            <Typography variant="subtitle1" sx={{ mb: 1 }}>
              Compression
            </Typography>
            <Table size="small">
              <TableBody>
                {(data?.compressors ?? []).map((codec) => (
                  <TableRow key={codec.name} hover>
                    <TableCell sx={{ width: 110 }}>{codec.name}</TableCell>
                    <TableCell>
                      <Typography variant="caption">{codec.description}</Typography>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Paper>
        </Grid>

        <Grid size={{ xs: 12, md: 6 }}>
          <Paper variant="outlined" sx={{ p: 2 }}>
            <Typography variant="subtitle1" sx={{ mb: 1 }}>
              Error correction
            </Typography>
            <Table size="small">
              <TableBody>
                {(data?.fec_codecs ?? []).map((codec) => (
                  <TableRow key={codec.name} hover>
                    <TableCell sx={{ width: 130 }}>{codec.name}</TableCell>
                    <TableCell>
                      <Typography variant="caption">{codec.description}</Typography>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Paper>
        </Grid>
      </Grid>
    </Stack>
  )
}
