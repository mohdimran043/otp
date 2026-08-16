import { useEffect, useState } from 'react'
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
import { chooseGeometry, usableFrameArea } from '../lib/cellFit'
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
// 64 is the smallest grid worth offering, and it is offered for tiling rather than despite it.
//
// Its header and footer bands take a large share of a small grid — they hold repeated copies for
// majority voting, which is a fixed cost — so a 64-cell frame carries far less than its area
// suggests. What it buys is cell size: four 64-cell lanes span the display like a single 136-cell
// grid where four 96-cell lanes span it like a 200-cell one, and on a phone camera that difference
// decides whether the cells can be read at all.
const GRID_PRESETS = [64, 80, 96, 128, 192, 256, 384, 512, 1024]
const CELL_PRESETS = [1, 2, 3, 4, 6, 8]

// The lane count is not chosen here, deliberately.
//
// It used to be, and the value was dropped on the floor: the form sent it, the sender parsed it into
// a field nothing read, and the scheduler tiled by the display's own setting regardless. The two
// disagreed by default — this form offered one lane while the display was set to two — and the
// damage was not the ignored dropdown but the geometry chosen beside it. A grid sized to fill the
// screen as a single lane is twice the screen's width once two of them are tiled, and the display
// scales only by whole numbers, so it is pinned at 1x and hangs off the edge with a lane out of shot.
//
// So the display's setting is read and used to size the geometry, and the only control lives beside
// the thing it changes. Lanes can be changed mid-transfer from there, which is a property worth
// keeping: every lane is an ordinary frame, so the ones already rendered stay valid.



// COLOUR_GRID_CEILING is the largest grid Auto will choose for a colour payload.
//
// A colour cell is matched against a palette rather than thresholded, so reading one is a
// measurement, and its accuracy comes from how many camera pixels were averaged over it. Measured on
// a 1080p handheld capture: 11 px per cell decoded every frame, 8.5 decoded about one in a hundred,
// 5.9 never decoded at all. A 1080p camera framing a panel generously resolves roughly 900 px across
// the grid, so 80 cells lands near 11 and 128 lands near 8.5 — which is why Auto used to pick a grid
// that could not be read, on a screen where it looked perfectly sharp.
//
// Eighty now, down from 96, and the demotion is worth recording because 96 passed every check it was
// given and still transferred nothing.
//
// A 96-cell colour frame on a 1080 short side measured 10.41 px per cell — above the 10 the shared
// model asks for, inside the aiming display's window, with fiducials and timing both at 1.0 and the
// geometry located perfectly on every frame. Across 519 captures it decoded 9, and all 9 were
// manifests. Not one chunk frame ever read.
//
// The asymmetry is arithmetic, not luck. A CRC is a product over every cell its payload spans, so the
// per-cell error rate is raised to the length of the payload: a manifest is about 110 bytes and a
// chunk is 1903, which at three bits a cell is ~290 cells against ~5,075. A per-cell error under one
// percent — invisible to every geometry gate, because the geometry is genuinely fine — lets a quarter
// of the manifests through and leaves a chunk odds of roughly e⁻²⁴. The operator sees a locked grid,
// a green outline, and a transfer that never advances.
//
// So the 10 px/cell floor is where "some frames decode" begins, not where a transfer completes, and
// Auto has to aim well clear of it. Eighty gives 12.9 px/cell at the same capture, which is the
// geometry behind the only byte-exact camera transfer this project has recorded.
//
// It bounds Auto only. Choosing 96 or more deliberately is still allowed and still right for a
// file-loopback channel or a camera with more sensor than a phone: this is the default for someone
// who has not thought about it, and for them a frame that decodes beats a frame that carries more.
const COLOUR_GRID_CEILING = 80


// fitGridAndCell solves the (grid, cell) pair that fits this screen, for whichever of the two
// pieces the operator left on "Auto". The server cannot do this — it runs with no display
// attached — so the browser looking at its own screen has to.
//
// A grid fixed and cell on auto asks lib/cellFit for the cheapest cell size that still reaches the
// largest whole-number-scaled display — which is emphatically not the largest cell that fits. The
// display scales by integers, so an 80-cell grid at 8 px renders 672 and is shown at 672, while the
// same grid at 4 px renders 336 and is shown at 1008: the bigger cell gave the smaller picture and
// four times the encoding work. A cell fixed and grid on auto picks the largest grid that fits at
// that cell. Both on auto takes the largest grid that can be shown at all, since grid is what
// carries payload, and then sizes its cell the same way.
//
// The encoder bounds that search rather than steering it: a colour payload cannot use the denser
// grids at all on a camera channel, so they are removed from consideration before the preference
// for "largest" is applied. See COLOUR_GRID_CEILING.
function fitGridAndCell(
  grid: 'auto' | number,
  cell: 'auto' | number,
  encoder?: string,
  lanes = 1,
): { grid: number; cell: number } {
  // The space one lane may occupy, not the whole screen.
  //
  // Lanes are tiled, so four of them span two lane-widths across and two down; sizing a lane against
  // the full screen produces a display twice the screen's size, which then cannot be shown at a whole
  // multiple and overflows. Four 96-cell lanes at 8 px a cell came to 1600 pixels square on a 1080p
  // panel exactly this way.
  //
  // Whole multiples are not negotiable — a cell resampled across a fractional boundary is a cell the
  // decoder reads wrongly — so the fit has to be right here rather than corrected by scaling later.
  // Measured the way the Display page will measure it, not against the raw screen.
  //
  // Those are different numbers and the difference was a bug: the screen is the physical panel, the page
  // has the browser's viewport minus the room it keeps for the caption. Sizing against the screen chose
  // geometries the page could not then show — grid 512 rendered 1032 px, which fits a 1080 screen and
  // overflows every real viewport, and the page will not scale by a fraction to rescue it.
  const usable = usableFrameArea(window.screen.width, window.screen.height, lanes)

  return chooseGeometry(grid, cell, GRID_PRESETS, CELL_PRESETS, usable, encoder, COLOUR_GRID_CEILING)
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
  const settings = useQuery({ queryKey: ['settings'], queryFn: api.settings })

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
  // The display's lane count, which is the one that will actually be used. Falling back to a single
  // lane until it loads keeps the first render honest rather than optimistic: a geometry sized for
  // one lane and shown in one is readable, where the reverse overflows the screen.
  const lanes = settings.data?.lanes ?? 1
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

  const resolved = fitGridAndCell(grid, cell, encoder, lanes)

  // Set when the sender has refused a geometry as unreadable and the operator has chosen to send it
  // regardless. Deliberately not sticky: it is cleared whenever the geometry or encoder changes, so an
  // override granted for one grid cannot silently carry to a different one.
  const [sendAnyway, setSendAnyway] = useState(false)
  useEffect(() => setSendAnyway(false), [grid, cell, encoder])

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
      if (sendAnyway) form.append('send_anyway', 'true')
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

      {/* A refusal an operator can act on.

          The check behind it rests on the receiving camera's resolution, which the sender is told
          rather than able to measure — so it can be wrong in a way only the person holding the camera
          knows about. Leaving them with a message and no button turns a good estimate into a wall. */}
      {submit.isError && !sendAnyway && /cannot be read|too dense/i.test(String(submit.error)) && (
        <Alert
          severity="warning"
          variant="outlined"
          action={
            <Button
              size="small"
              color="warning"
              onClick={() => {
                setSendAnyway(true)
                submit.reset()
              }}
            >
              Send anyway
            </Button>
          }
        >
          That estimate assumes what the receiving camera can resolve, which this side is told rather
          than able to measure. If your camera captures more than the sender has been configured for,
          correct it there — otherwise send it and watch what actually arrives.
        </Alert>
      )}

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
