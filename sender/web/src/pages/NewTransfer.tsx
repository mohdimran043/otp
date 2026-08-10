import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  InputAdornment,
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

const GRID_PRESETS = [128, 192, 256, 384, 512]

// The largest preset whose rendered frame fits this screen. The server cannot know a
// panel's size — only a browser on that panel can — so "auto" is computed here and
// submitted as an explicit number. The frame is roughly (grid + 8 border cells) × cell
// pixels on a side; 8 is deliberately generous so auto never picks a grid that clips.
function fitGrid(cellPixels: number): number {
  const usable = Math.min(window.screen.width, window.screen.height)
  const fits = GRID_PRESETS.filter((g) => (g + 8) * cellPixels <= usable)
  return fits.at(-1) ?? GRID_PRESETS[0]!
}

function randomKeyHex(): string {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

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
  const [encryption, setEncryption] = useState('none')
  const [keyHex, setKeyHex] = useState('')
  const [grid, setGrid] = useState<'auto' | number>('auto')

  const defaults = profiles.data?.defaults

  const keyValid = /^[0-9a-fA-F]{64}$/.test(keyHex.trim())

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
      if (encryption !== 'none') {
        form.append('encryption', encryption)
        form.append('encryption_key_hex', keyHex.trim())
      } else {
        form.append('encryption', 'none')
      }
      const cell = defaults?.cell_pixels ?? 4
      const g = grid === 'auto' ? fitGrid(cell) : grid
      form.append('grid_width', String(g))
      form.append('grid_height', String(g))
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

            <Grid container spacing={2}>
              <Grid size={{ xs: 12, md: 4 }}>
                <TextField
                  select
                  fullWidth
                  label="Encryption"
                  value={encryption}
                  onChange={(event) => setEncryption(event.target.value)}
                >
                  <MenuItem value="none">
                    None — anyone who can see the display can read the file
                  </MenuItem>
                  <MenuItem value="aes256gcm">AES-256-GCM</MenuItem>
                  <MenuItem value="chacha20poly1305">ChaCha20-Poly1305</MenuItem>
                </TextField>
              </Grid>

              {encryption !== 'none' && (
                <Grid>
                  <TextField
                    fullWidth
                    label="Key (64 hex characters)"
                    value={keyHex}
                    onChange={(event) => setKeyHex(event.target.value)}
                    error={keyHex.length > 0 && !keyValid}
                    slotProps={{
                      input: {
                        sx: { fontFamily: 'monospace' },
                        endAdornment: (
                          <InputAdornment position="end">
                            <Button size="small" onClick={() => setKeyHex(randomKeyHex())}>
                              Generate
                            </Button>
                          </InputAdornment>
                        ),
                      },
                    }}
                    helperText="Carry this key to the receiver's Settings page yourself — it never crosses the optical channel."
                  />
                </Grid>
              )}

              <Grid size={{ xs: 12, md: 4 }}>
                <TextField
                  select
                  fullWidth
                  label="Grid"
                  value={grid}
                  onChange={(event) =>
                    setGrid(event.target.value === 'auto' ? 'auto' : Number(event.target.value))
                  }
                  helperText="384 is the measured sweet spot on a 4K panel."
                >
                  <MenuItem value="auto">
                    Auto — fit my screen ({fitGrid(defaults?.cell_pixels ?? 4)})
                  </MenuItem>
                  {GRID_PRESETS.map((preset) => (
                    <MenuItem key={preset} value={preset}>
                      {preset} × {preset}
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
                Frames render on a per-transfer grid (set above); the cell size is global under Settings.
                Frames display at {defaults.fps} frames a second, changeable under <strong>Settings</strong>{' '}
                at any time.
              </Alert>
            )}

            <Box>
              <Button
                variant="contained"
                size="large"
                disabled={!file || submit.isPending || (encryption !== 'none' && !keyValid)}
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
