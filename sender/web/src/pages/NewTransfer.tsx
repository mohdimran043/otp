import { useState } from 'react'
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  FormControlLabel,
  InputAdornment,
  MenuItem,
  Slider,
  Stack,
  Switch,
  TextField,
  Typography,
} from '@mui/material'
import ExpandMoreIcon from '@mui/icons-material/ExpandMore'
import UploadFileIcon from '@mui/icons-material/UploadFile'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'

import { api, formatBytes } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { useUi } from '../store/ui'

// 1024 is a valid preset for display visibility, not a camera-proven one: it round-trips
// byte-exact on the shared-directory (file-loopback) channel, which is how most operators
// actually run this, but it has not been shown to survive a real camera capturing a real
// panel. The cell-size control and the note below it exist so that distinction is visible
// rather than assumed.
//
// 80 and 96 are at the other end, and they exist for the opposite reason: a colour payload
// photographed off a panel needs pixels per cell far more than it needs cells. Each cell is
// matched against eight palette entries rather than put on one side of a threshold, so its
// accuracy comes from how many camera pixels were averaged to read it — and that is exactly what
// a denser grid spends. Measured handheld against a 1080p capture: at 128 a frame filling 94% of
// the view gives 8.6 px per cell and 232 consecutive frames located perfectly and failed their
// payload CRC; at 80 the same framing gives about 14, which is twice the samples per cell.
//
// 72 is the floor and is deliberately not offered. The header and footer bands are a fixed number
// of rows, so below about 72 they consume the grid and what is left carries too few bytes to be
// worth a frame.
const GRID_PRESETS = [80, 96, 128, 192, 256, 384, 512, 1024]
const CELL_PRESETS = [1, 2, 3, 4, 6, 8]

// The quiet zone used for this estimate is a guess, not a fetch — the real one lives in the
// server's configuration and the exact figure barely moves the answer. It only has to be close
// enough that "fits the screen" is not off by a border's worth of pixels.
const ASSUMED_QUIET_ZONE = 2

function frameEdgePx(grid: number, cell: number): number {
  return (grid + 2 * ASSUMED_QUIET_ZONE) * cell
}

// COLOUR_GRID_CEILING is the largest grid Auto will choose for a colour payload.
//
// A colour cell is matched against a palette rather than thresholded, so reading one is a
// measurement, and its accuracy comes from how many camera pixels were averaged over it. Measured on
// a 1080p handheld capture: 11 px per cell decoded every frame, 8.5 decoded about one in a hundred,
// 5.9 never decoded at all. A 1080p camera framing a panel generously resolves roughly 900 px across
// the grid, so 80 cells lands near 11 and 128 lands near 8.5 — which is why Auto used to pick a grid
// that could not be read, on a screen where it looked perfectly sharp.
//
// It bounds Auto only. Choosing 128 or more deliberately is still allowed and still right for a
// file-loopback channel or a camera with more sensor than a phone: this is the default for someone
// who has not thought about it, and for them a frame that decodes beats a frame that carries more.
const COLOUR_GRID_CEILING = 96

/** isColour reports whether an encoder carries more than one bit per cell. */
function isColour(encoder: string | undefined): boolean {
  return !!encoder && encoder !== 'grayscale'
}

// fitGridAndCell solves the (grid, cell) pair that fits this screen, for whichever of the two
// pieces the operator left on "Auto". The server cannot do this — it runs with no display
// attached — so the browser looking at its own screen has to.
//
// A grid fixed and cell on auto picks the largest cell that still fits, because a bigger cell is
// what a camera needs to resolve — screen area is free once the grid decision is made. A cell
// fixed and grid on auto picks the largest grid that fits at that cell. Both on auto searches
// every combination and prefers the largest cell first, then the largest grid, because visibility
// matters more than raw capacity when neither has been chosen deliberately.
//
// The encoder bounds that search rather than steering it: a colour payload cannot use the denser
// grids at all on a camera channel, so they are removed from consideration before the preference
// for "largest" is applied. See COLOUR_GRID_CEILING.
function fitGridAndCell(
  grid: 'auto' | number,
  cell: 'auto' | number,
  encoder?: string,
): { grid: number; cell: number } {
  const usable = Math.min(window.screen.width, window.screen.height)

  // An explicit choice is never second-guessed, whatever the encoder.
  if (grid !== 'auto' && cell !== 'auto') return { grid, cell }

  const grids = isColour(encoder) ? GRID_PRESETS.filter((g) => g <= COLOUR_GRID_CEILING) : GRID_PRESETS

  if (grid !== 'auto') {
    const fits = CELL_PRESETS.filter((c) => frameEdgePx(grid, c) <= usable)
    return { grid, cell: fits.at(-1) ?? CELL_PRESETS[0]! }
  }

  if (cell !== 'auto') {
    const fits = grids.filter((g) => frameEdgePx(g, cell) <= usable)
    return { grid: fits.at(-1) ?? grids[0]!, cell }
  }

  let best: { grid: number; cell: number } | null = null
  for (const g of grids) {
    for (const c of CELL_PRESETS) {
      if (frameEdgePx(g, c) > usable) continue
      if (!best || c > best.cell || (c === best.cell && g > best.grid)) {
        best = { grid: g, cell: c }
      }
    }
  }
  return best ?? { grid: grids[0]!, cell: CELL_PRESETS[0]! }
}

function randomKeyHex(): string {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

// The one form that starts everything: a file, and where the result should go.
//
// Only the file picker, the "start immediately" switch, and the submit button are visible by
// default — everything else has a default that already matches what the deployment does, and an
// operator who knows nothing about encodings can send a file without opening "Advanced settings"
// at all. The ones who do care can open it; the lists inside it come from the server rather than
// being hard-coded here, so a build that added an encoding would otherwise not offer it.
export function NewTransfer() {
  const navigate = useNavigate()
  const { lastProfile, rememberProfile } = useUi()

  const profiles = useQuery({ queryKey: ['profiles'], queryFn: api.profiles })
  const keys = useQuery({ queryKey: ['keys'], queryFn: api.keys })

  const [file, setFile] = useState<File | null>(null)
  const [autostart, setAutostart] = useState(true)
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const [needsKeyNotice, setNeedsKeyNotice] = useState(false)

  const [callbackUrl, setCallbackUrl] = useState(lastProfile.callbackUrl)
  const [encoder, setEncoder] = useState(lastProfile.encoder)
  const [compression, setCompression] = useState(lastProfile.compression)
  const [fecCodec, setFecCodec] = useState(lastProfile.fecCodec)
  const [level, setLevel] = useState<number>(0)
  // '' means "the operator has not touched this dropdown" — its effective value below
  // follows the server's answer once profiles loads, rather than being frozen at whatever
  // guess was made on the first render.
  const [encryption, setEncryption] = useState('')
  const [keySource, setKeySource] = useState<'saved' | 'paste'>('saved')
  const [keyId, setKeyId] = useState<number | ''>('')
  const [keyHex, setKeyHex] = useState('')
  const [grid, setGrid] = useState<'auto' | number>('auto')
  const [cell, setCell] = useState<'auto' | number>('auto')

  const defaults = profiles.data?.defaults

  // A deployment with a global encryption key configured has always encrypted a transfer
  // that says nothing about encryption at all (see parseTransferRequest's legacy path); one
  // without a key has always sent it in the clear. Untouched, this form must default to
  // whichever of those the deployment actually does — never silently to "none" — so the
  // encryption choice a caller sees matches what leaving it alone has always meant.
  const effectiveEncryption =
    encryption || (defaults?.encryption_configured ? 'default' : 'none')
  const needsKey = effectiveEncryption !== 'none' && effectiveEncryption !== 'default'

  const keyValid = /^[0-9a-fA-F]{64}$/.test(keyHex.trim())
  const hasSavedKeys = (keys.data ?? []).length > 0
  const hasValidKey = needsKey && (keySource === 'saved' ? keyId !== '' : keyValid)

  const resolved = fitGridAndCell(grid, cell, encoder)

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
      // false = manual/deferred: the transfer is prepared and left in "ready" for an operator to
      // start from the transfer's own page. Omitted (or true) keeps the round-1 behaviour of
      // displaying the moment preparation finishes.
      if (!autostart) form.append('autostart', 'false')
      // "default" omits the field entirely rather than sending "none" or the deployment's
      // key type explicitly — that omission is exactly what parseTransferRequest reads as
      // "do what this deployment has always done", which is the legacy behaviour this
      // option exists to preserve.
      if (effectiveEncryption === 'default') {
        // Deliberately nothing appended.
      } else if (effectiveEncryption !== 'none') {
        form.append('encryption', effectiveEncryption)
        if (keySource === 'saved' && keyId !== '') {
          form.append('encryption_key_id', String(keyId))
        } else {
          form.append('encryption_key_hex', keyHex.trim())
        }
      } else {
        form.append('encryption', 'none')
      }
      form.append('grid_width', String(resolved.grid))
      form.append('grid_height', String(resolved.grid))
      form.append('cell_pixels', String(resolved.cell))
      return api.submit(form)
    },
    onSuccess: (accepted) => {
      rememberProfile({ callbackUrl, encoder, compression, fecCodec })
      navigate(`/transfers/${accepted.transmission_id}`)
    },
  })

  const handleSubmit = () => {
    if (!file || submit.isPending) return
    if (needsKey && !hasValidKey) {
      // The reason submission cannot proceed lives inside the collapsed Advanced section — a
      // cipher was chosen but no key backs it — so the operator would otherwise stare at a
      // button that silently does nothing. Opening it here is what makes the reason visible.
      setAdvancedOpen(true)
      setNeedsKeyNotice(true)
      return
    }
    setNeedsKeyNotice(false)
    submit.mutate()
  }

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

            <FormControlLabel
              control={
                <Switch checked={autostart} onChange={(event) => setAutostart(event.target.checked)} />
              }
              label={
                <Box>
                  <Typography variant="body2">Start displaying immediately</Typography>
                  <Typography variant="caption" color="text.secondary">
                    {autostart
                      ? 'The display begins the moment preparation finishes.'
                      : 'Off — the transfer is prepared and left waiting in "ready"; start it yourself from its own page when you want the display to begin.'}
                  </Typography>
                </Box>
              }
            />

            <Accordion
              expanded={advancedOpen}
              onChange={(_, expanded) => setAdvancedOpen(expanded)}
              variant="outlined"
            >
              <AccordionSummary expandIcon={<ExpandMoreIcon />}>
                <Typography>Advanced settings</Typography>
              </AccordionSummary>
              <AccordionDetails>
                <Stack spacing={3}>
                  <TextField
                    label="Callback URL"
                    fullWidth
                    value={callbackUrl}
                    onChange={(event) => setCallbackUrl(event.target.value)}
                    placeholder="https://intake.example.com/received"
                    // The explanation matters because the behaviour is not what a caller would assume: the
                    // file itself is posted here by the *receiver*, after it has been reassembled and
                    // checked, not by the sender.
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
                        value={effectiveEncryption}
                        onChange={(event) => setEncryption(event.target.value)}
                      >
                        {defaults?.encryption_configured && (
                          <MenuItem value="default">Deployment default (encrypted)</MenuItem>
                        )}
                        <MenuItem value="none">
                          None — anyone who can see the display can read the file
                        </MenuItem>
                        <MenuItem value="aes256gcm">AES-256-GCM</MenuItem>
                        <MenuItem value="chacha20poly1305">ChaCha20-Poly1305</MenuItem>
                      </TextField>
                    </Grid>

                    {needsKey && (
                      <>
                        <Grid size={{ xs: 12, md: 4 }}>
                          <TextField
                            select
                            fullWidth
                            label="Key source"
                            value={hasSavedKeys ? keySource : 'paste'}
                            disabled={!hasSavedKeys}
                            onChange={(event) => setKeySource(event.target.value as 'saved' | 'paste')}
                            helperText={hasSavedKeys ? undefined : 'No saved keys yet — paste one below.'}
                          >
                            <MenuItem value="saved" disabled={!hasSavedKeys}>
                              A saved key
                            </MenuItem>
                            <MenuItem value="paste">Paste a key</MenuItem>
                          </TextField>
                        </Grid>

                        {keySource === 'saved' && hasSavedKeys ? (
                          <Grid size={{ xs: 12, md: 4 }}>
                            <TextField
                              select
                              fullWidth
                              label="Saved key"
                              value={keyId}
                              onChange={(event) => setKeyId(event.target.value ? Number(event.target.value) : '')}
                              helperText="The key never leaves the sender — the receiver was given it out of band."
                            >
                              <MenuItem value="">Choose a key…</MenuItem>
                              {keys.data!.map((k) => (
                                <MenuItem key={k.id} value={k.id}>
                                  {k.label || '(no label)'} — {k.fingerprint}
                                </MenuItem>
                              ))}
                            </TextField>
                          </Grid>
                        ) : (
                          <Grid size={{ xs: 12, md: 4 }}>
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
                      </>
                    )}
                  </Grid>

                  {needsKeyNotice && needsKey && !hasValidKey && (
                    <Alert severity="warning" variant="outlined" onClose={() => setNeedsKeyNotice(false)}>
                      Choose a saved key or paste one before sending — {effectiveEncryption} needs one.
                    </Alert>
                  )}

                  <Grid container spacing={2}>
                    <Grid size={{ xs: 12, md: 6 }}>
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
                          Auto — fit my screen ({resolved.grid} × {resolved.grid})
                        </MenuItem>
                        {GRID_PRESETS.map((preset) => (
                          <MenuItem key={preset} value={preset}>
                            {preset} × {preset}
                          </MenuItem>
                        ))}
                      </TextField>
                    </Grid>

                    <Grid size={{ xs: 12, md: 6 }}>
                      <TextField
                        select
                        fullWidth
                        label="Cell size"
                        value={cell}
                        onChange={(event) =>
                          setCell(event.target.value === 'auto' ? 'auto' : Number(event.target.value))
                        }
                        helperText={`Auto picks ${resolved.cell} px for this grid on this screen.`}
                      >
                        <MenuItem value="auto">Auto — fit my screen ({resolved.cell} px)</MenuItem>
                        {CELL_PRESETS.map((preset) => (
                          <MenuItem key={preset} value={preset}>
                            {preset} px
                          </MenuItem>
                        ))}
                      </TextField>
                    </Grid>
                  </Grid>

                  <Alert severity="info" variant="outlined">
                    Small-cell, large grids — 1024 or 512 at 1–2 px — round-trip byte-exact over the
                    shared-directory channel and are useful for that, but they are not proven against a
                    real camera. A camera pointed at the display wants the measured configurations
                    instead: 384×384 at about 5 px.
                  </Alert>

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
                      One scale for every codec: 1 is fastest, 9 smallest. On a channel this slow, smaller
                      usually wins.
                    </Typography>
                  </Box>

                  {defaults && (
                    <Alert severity="info" variant="outlined">
                      Frames display at {defaults.fps} frames a second, changeable under{' '}
                      <strong>Settings</strong> at any time.
                    </Alert>
                  )}
                </Stack>
              </AccordionDetails>
            </Accordion>

            <Box>
              <Button
                variant="contained"
                size="large"
                disabled={!file || submit.isPending}
                onClick={handleSubmit}
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
