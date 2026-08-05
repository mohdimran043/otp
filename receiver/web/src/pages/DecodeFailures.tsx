import {
  Box,
  Card,
  CardContent,
  CardMedia,
  Paper,
  Stack,
  Typography,
} from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api, formatPercent } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { useUi } from '../store/ui'

// The frames that could not be read, with the images.
//
// This page is the reason captures are written to disk before they are decoded. A failure count tells an
// operator something is wrong; the frame itself tells them what — a blur means focus, a tear means the
// sensor is out of sync with the panel, a frame half off the edge means aim. None of that is recoverable
// from a number.
export function DecodeFailures() {
  const refreshMs = useUi((state) => state.refreshMs)
  const frames = useQuery({
    queryKey: ['failed-frames'],
    queryFn: () => api.failedFrames(24),
    refetchInterval: refreshMs * 2,
  })

  const list = frames.data ?? []

  return (
    <Stack spacing={2}>
      <Typography variant="h5">Decode failures</Typography>
      <ErrorNotice error={frames.error} />

      {list.length === 0 ? (
        <Paper variant="outlined" sx={{ p: 3 }}>
          <Typography variant="body2" color="text.secondary">
            Every captured frame in this session was read successfully.
          </Typography>
        </Paper>
      ) : (
        <Grid container spacing={2}>
          {list.map((frame) => (
            <Grid key={frame.id} size={{ xs: 12, sm: 6, md: 4, lg: 3 }}>
              <Card variant="outlined">
                <CardMedia
                  component="img"
                  image={api.frameImageUrl(frame.id)}
                  alt={`capture ${frame.sequence}`}
                  sx={{ aspectRatio: '1 / 1', objectFit: 'contain', bgcolor: 'black' }}
                />
                <CardContent>
                  <Typography variant="subtitle2">capture {frame.sequence}</Typography>
                  <Box sx={{ mt: 0.5 }}>
                    <Typography variant="caption" color="text.secondary" display="block">
                      fiducials {formatPercent(frame.finder_score)} · timing{' '}
                      {formatPercent(frame.timing_score)} · contrast {frame.contrast.toFixed(0)}
                    </Typography>
                    <Typography variant="caption" color="error.main" display="block" sx={{ mt: 0.5 }}>
                      {frame.decode_error}
                    </Typography>
                  </Box>
                </CardContent>
              </Card>
            </Grid>
          ))}
        </Grid>
      )}
    </Stack>
  )
}
