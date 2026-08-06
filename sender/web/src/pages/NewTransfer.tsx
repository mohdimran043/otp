import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  MenuItem,
  Slider,
  Stack,
  TextField,
  Typography,
} from '@mui/material'
import UploadFileIcon from '@mui/icons-material/UploadFile'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'

import { api, formatBytes } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { useUi } from '../store/ui'

// The one form that starts everything: a file, and where the result should go.
//
// The choices below it all have defaults from the server's configuration, so an operator who knows nothing
// about encodings can send a file by choosing two things. The ones who do care can change the rest, and
// the lists come from the server rather than being hard-coded here — a build that added an encoding would
// otherwise not offer it.
export function NewTransfer() {
  const navigate = useNavigate()
  const { lastProfile, rememberProfile } = useUi()

  const profiles = useQuery({ queryKey: ['profiles'], queryFn: api.profiles })

  const [file, setFile] = useState<File | null>(null)
  const [callbackUrl, setCallbackUrl] = useState(lastProfile.callbackUrl)
  const [encoder, setEncoder] = useState(lastProfile.encoder)
  const [compression, setCompression] = useState(lastProfile.compression)
  const [fecCodec, setFecCodec] = useState(lastProfile.fecCodec)
  const [level, setLevel] = useState<number>(0)

  const defaults = profiles.data?.defaults

  const submit = useMutation({
    mutationFn: async () => {
      if (!file) throw new Error('Choose a file first')
      const form = new FormData()
      form.append('file', file)
      form.append('filename', file.name)
      if (callbackUrl) form.append('callback_url', callbackUrl)
      if (encoder) form.append('encoder', encoder)
      if (compression) form.append('compression', compression)
      if (fecCodec) form.append('fec_codec', fecCodec)
      if (level > 0) form.append('level', String(level))
      return api.submit(form)
    },
    onSuccess: (accepted) => {
      rememberProfile({ callbackUrl, encoder, compression, fecCodec })
      navigate(`/transfers/${accepted.transmission_id}`)
    },
  })

  return (
    <Stack spacing={3} sx={{ maxWidth: 900 }}>
      <Typography variant="h5">Send a file</Typography>

      <ErrorNotice error={profiles.error ?? submit.error} />

      <Card variant="outlined">
        <CardContent>
          <Stack spacing={3}>
            <Box>
              <Button component="label" variant="outlined" startIcon={<UploadFileIcon />}>
                {file ? 'Choose a different file' : 'Choose a file'}
                <input
                  hidden
                  type="file"
                  onChange={(event) => setFile(event.target.files?.[0] ?? null)}
                />
              </Button>
              {file && (
                <Typography variant="body2" sx={{ mt: 1 }}>
                  {file.name} · {formatBytes(file.size)}
                </Typography>
              )}
            </Box>

            <TextField
              label="Callback URL"
              fullWidth
              value={callbackUrl}
              onChange={(event) => setCallbackUrl(event.target.value)}
              placeholder="https://intake.example.com/received"
              // The explanation matters because the behaviour is not what a caller would assume: the file
              // itself is posted here by the *receiver*, after it has been reassembled and checked, not by
              // the sender.
              helperText="Where the receiver posts the file once it has merged and verified it. Leave empty for no delivery."
            />

            <Grid container spacing={2}>
              <Grid size={{ xs: 12, md: 4 }}>
                <TextField
                  select
                  fullWidth
                  label="Encoding"
                  value={encoder}
                  onChange={(event) => setEncoder(event.target.value)}
                  helperText={
                    profiles.data?.encoders.find((e) => e.name === encoder)?.description ??
                    `Default: ${defaults?.encoder ?? '—'}`
                  }
                >
                  <MenuItem value="">Use the configured default</MenuItem>
                  {profiles.data?.encoders.map((option) => (
                    <MenuItem key={option.name} value={option.name}>
                      {option.name}
                    </MenuItem>
                  ))}
                </TextField>
              </Grid>

              <Grid size={{ xs: 12, md: 4 }}>
                <TextField
                  select
                  fullWidth
                  label="Compression"
                  value={compression}
                  onChange={(event) => setCompression(event.target.value)}
                  helperText={
                    profiles.data?.compressors.find((c) => c.name === compression)?.description ??
                    `Default: ${defaults?.compression ?? '—'}`
                  }
                >
                  <MenuItem value="">Use the configured default</MenuItem>
                  {profiles.data?.compressors.map((option) => (
                    <MenuItem key={option.name} value={option.name}>
                      {option.name}
                    </MenuItem>
                  ))}
                </TextField>
              </Grid>

              <Grid size={{ xs: 12, md: 4 }}>
                <TextField
                  select
                  fullWidth
                  label="Error correction"
                  value={fecCodec}
                  onChange={(event) => setFecCodec(event.target.value)}
                  helperText={
                    profiles.data?.fec_codecs.find((c) => c.name === fecCodec)?.description ??
                    `Default: ${defaults?.fec_codec ?? '—'}`
                  }
                >
                  <MenuItem value="">Use the configured default</MenuItem>
                  {profiles.data?.fec_codecs.map((option) => (
                    <MenuItem key={option.name} value={option.name}>
                      {option.name}
                    </MenuItem>
                  ))}
                </TextField>
              </Grid>
            </Grid>

            <Box>
              <Typography variant="body2" gutterBottom>
                Compression level {level === 0 ? '(codec default)' : level}
              </Typography>
              <Slider
                value={level}
                min={0}
                max={9}
                step={1}
                marks
                valueLabelDisplay="auto"
                onChange={(_, value) => setLevel(value as number)}
              />
              <Typography variant="caption" color="text.secondary">
                One scale for every codec: 1 is fastest, 9 smallest. On a channel this slow, smaller usually
                wins.
              </Typography>
            </Box>

            {defaults && (
              <Alert severity="info" variant="outlined">
                Frames are rendered on a {defaults.grid} grid at {defaults.cell_pixels}px cells and displayed
                at {defaults.fps} frames a second. Both are changeable under <strong>Settings</strong> — the
                frame rate at any time, the grid only while nothing is in flight, because it is written into
                every frame header and the chunk size is derived from it.
              </Alert>
            )}

            <Box>
              <Button
                variant="contained"
                size="large"
                disabled={!file || submit.isPending}
                onClick={() => submit.mutate()}
              >
                {submit.isPending ? 'Uploading…' : 'Start the transfer'}
              </Button>
            </Box>
          </Stack>
        </CardContent>
      </Card>
    </Stack>
  )
}
