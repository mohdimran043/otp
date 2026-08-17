import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  Divider,
  FormControl,
  IconButton,
  InputAdornment,
  InputLabel,
  MenuItem,
  Paper,
  Select,
  Slider,
  Stack,
  Tab,
  Table,
  TableBody,
  TableCell,
  TableRow,
  Tabs,
  TextField,
  ToggleButton,
  ToggleButtonGroup,
  Typography,
} from '@mui/material'
import DeleteIcon from '@mui/icons-material/Delete'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { api, formatRate, type DisplaySettingsPatch } from '../api/client'
import { Certificates } from '../components/Certificates'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { Stat } from '../components/Stat'

function randomKeyHex(): string {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

// The two settings that set the transfer rate, and they are not symmetrical.
//
// **The frame rate** is a delay between two writes. Nothing already rendered depends on it, so it changes
// at any moment — including mid-transfer, which is exactly when an operator wants it, because a receiver
// falling behind is the reason to turn it down.
//
// **The geometry** — grid, cell size, encoding — is written into every frame header, and the chunk size was
// derived from the encoder's capacity at that grid. A transfer that changed geometry halfway would have its
// remaining chunks rendered to a shape its manifest never declared, and the receiver would reassemble the
// wrong file. So the server refuses it while anything is in flight, and this page says so before the
// operator tries.

/** Presets are the geometries worth reaching for, with the panel each one needs. */
const PRESETS = [
  { label: '96×96 at 6 px — 576 px, any monitor', w: 96, h: 96, cell: 6 },
  { label: '128×128 at 8 px — 1056 px, a 1080p panel', w: 128, h: 128, cell: 8 },
  { label: '192×192 at 5 px — 980 px, a 1080p panel', w: 192, h: 192, cell: 5 },
  { label: '256×256 at 4 px — 1040 px, a 1080p panel, small cells', w: 256, h: 256, cell: 4 },
  { label: '256×256 at 8 px — 2080 px, a 4K panel', w: 256, h: 256, cell: 8 },
  { label: '384×384 at 5 px — 1940 px, a 4K panel', w: 384, h: 384, cell: 5 },
  { label: '512×512 at 4 px — 2064 px, a 4K panel, small cells', w: 512, h: 512, cell: 4 },
]

/**
 * measureRefreshRate times animation frames to find the panel's refresh rate.
 *
 * The server cannot do this. It runs in a container with no display attached, and the only place the number
 * exists is the browser painting on the actual monitor. `requestAnimationFrame` fires once per composited
 * frame, so the median interval between callbacks is the refresh interval — 6.94 ms on a 144 Hz panel.
 *
 * The median rather than the mean, because the first few frames after a tab becomes active are irregular and
 * one 50 ms hitch would drag an average down by more than the measurement is worth.
 */
function measureRefreshRate(samples = 90, budgetMs = 2500): Promise<number> {
  return new Promise((resolve) => {
    const times: number[] = []
    const started = performance.now()
    let previous = started

    const finish = () => {
      // At least a handful of intervals, or the median is noise rather than a measurement.
      if (times.length < 8) {
        resolve(0)
        return
      }
      const sorted = [...times].sort((a, b) => a - b)
      const median = sorted[Math.floor(sorted.length / 2)] ?? 0
      resolve(median > 0 ? 1000 / median : 0)
    }

    const tick = (now: number) => {
      times.push(now - previous)
      previous = now
      // Bounded by wall clock as well as by sample count. A backgrounded tab throttles animation frames
      // to about one a second, so ninety samples would take a minute and a half — and a headless browser
      // may not paint at all. Either way the measurement must end rather than leave the page saying it is
      // still measuring for ever.
      if (times.length >= samples || now - started > budgetMs) {
        finish()
        return
      }
      requestAnimationFrame(tick)
    }

    requestAnimationFrame(tick)
    // A last resort for the case where no animation frame ever arrives.
    setTimeout(finish, budgetMs + 500)
  })
}

export function Settings() {
  const [tab, setTab] = useState(0)
  const client = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: api.settings, refetchInterval: 5000 })
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: api.profiles })
  const keys = useQuery({ queryKey: ['keys'], queryFn: api.keys })
  const data = settings.data

  const [fps, setFps] = useState<number | null>(null)
  const [grid, setGrid] = useState('')
  const [encoder, setEncoder] = useState('')
  const [refresh, setRefresh] = useState<number | null>(null)
  const [measuring, setMeasuring] = useState(false)
  const [note, setNote] = useState<string | null>(null)

  const [keyLabel, setKeyLabel] = useState('')
  const [keyHex, setKeyHex] = useState('')
  const keyValid = /^[0-9a-fA-F]{64}$/.test(keyHex.trim())

  const addKey = useMutation({
    mutationFn: () => api.addKey(keyHex.trim(), keyLabel.trim()),
    onSuccess: async () => {
      setKeyLabel('')
      setKeyHex('')
      await client.invalidateQueries({ queryKey: ['keys'] })
    },
  })
  const deleteKey = useMutation({
    mutationFn: (id: number) => api.deleteKey(id),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: ['keys'] })
    },
  })

  const sink = useMutation({
    mutationFn: (value: string) => api.updateSettings({ sink: value }),
    onSuccess: () => {
      setNote('Saved. The new channel takes hold on the next restart — this process keeps writing to the old one until then.')
      void client.invalidateQueries({ queryKey: ['settings'] })
    },
  })

  // The form follows the server until the operator touches it.
  useEffect(() => {
    if (!data) return
    setFps((current) => current ?? data.fps)
    setGrid((current) => current || `${data.grid_width}x${data.grid_height}@${data.cell_pixels}`)
    setEncoder((current) => current || data.encoder)
  }, [data])

  const measure = useCallback(async () => {
    setMeasuring(true)
    try {
      setRefresh(await measureRefreshRate())
    } finally {
      setMeasuring(false)
    }
  }, [])

  // Measured once on arrival, because the number is what makes the frame-rate field meaningful and an
  // operator should not have to know to ask for it.
  useEffect(() => {
    void measure()
  }, [measure])

  const apply = useMutation({
    mutationFn: (patch: DisplaySettingsPatch) => api.updateSettings(patch),
    onSuccess: (result) => {
      setNote(
        `Applied. One frame carries ${result.bytes_per_frame.toLocaleString()} bytes at ` +
          `${result.fps} fps — ${formatRate(result.bytes_per_second)} before compression.`,
      )
      void client.invalidateQueries({ queryKey: ['settings'] })
      void client.invalidateQueries({ queryKey: ['display'] })
    },
  })

  const chosen = useMemo(() => {
    const match = /^(\d+)x(\d+)@(\d+)$/.exec(grid)
    if (!match) return null
    return { w: Number(match[1]), h: Number(match[2]), cell: Number(match[3]) }
  }, [grid])

  const geometryChanged =
    chosen !== null &&
    data !== undefined &&
    (chosen.w !== data.grid_width || chosen.h !== data.grid_height || chosen.cell !== data.cell_pixels ||
      encoder !== data.encoder)

  const busy = (data?.transmitting ?? 0) > 0
  const overRefresh = refresh !== null && fps !== null && fps > refresh + 0.5
  const frameEdge = chosen ? chosen.w * chosen.cell + 2 * (data?.quiet_zone ?? 2) * chosen.cell : 0

  return (
    <Stack spacing={3}>
      {/* Two tabs rather than one long page. Certificates are managed rather than watched — an operator
          comes here to do something and leaves — so they do not belong interleaved with the figures
          above, which are read at a glance and never touched. */}
      <Tabs value={tab} onChange={(_, next: number) => setTab(next)} sx={{ borderBottom: 1, borderColor: 'divider' }}>
        <Tab label="Display" />
        <Tab label="Certificates" />
      </Tabs>

      {tab === 1 && <Certificates />}

      {tab === 0 && (
        <>
      <Typography variant="h5">Display</Typography>
      <ErrorNotice error={settings.error} />

      {data && (
        <Grid container spacing={2}>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Frame rate" value={`${data.fps} fps`} hint="changeable at any time" />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Payload per frame"
              value={`${data.bytes_per_frame.toLocaleString()} B`}
              hint={`${data.encoder}${data.bit_depth ? ` d${data.bit_depth}` : ''} on ${data.grid_width}×${data.grid_height}`}
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Channel rate"
              value={formatRate(data.bytes_per_second)}
              hint="before compression — a file that halves moves at twice this"
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Frame on screen"
              value={`${data.image_width_px}×${data.image_height_px} px`}
              hint={`${data.cell_pixels} px cells, ${data.quiet_zone} cell quiet zone`}
            />
          </Grid>
        </Grid>
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Frame rate
        </Typography>

        <Stack direction="row" spacing={2} alignItems="center" flexWrap="wrap" useFlexGap sx={{ mb: 2 }}>
          <Chip
            size="small"
            color={refresh ? 'success' : 'default'}
            label={
              measuring
                ? 'measuring this panel…'
                : refresh
                  ? `this panel: ${refresh.toFixed(1)} Hz`
                  : 'panel rate unknown'
            }
          />
          <Button size="small" onClick={() => void measure()} disabled={measuring}>
            Measure again
          </Button>
          {refresh !== null && (
            <Button size="small" onClick={() => setFps(Math.floor(refresh))}>
              Use {Math.floor(refresh)} fps
            </Button>
          )}
        </Stack>

        <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
          The panel's refresh rate is measured here rather than on the server, because the server runs in a
          container with no display attached — the only place the number exists is a browser painting on the
          actual monitor. A 144 Hz panel shows as a 6.94 ms frame interval.
        </Typography>

        {/* Room above for the always-on value label, which would otherwise sit on the text. */}
        <Box sx={{ px: 1, pt: 4, maxWidth: 520 }}>
          <Slider
            value={fps ?? 0}
            min={1}
            max={Math.max(60, Math.ceil(refresh ?? 60))}
            step={1}
            marks={[
              { value: 10, label: '10' },
              { value: 30, label: '30' },
              { value: 60, label: '60' },
              ...(refresh && refresh > 70 ? [{ value: Math.floor(refresh), label: `${Math.floor(refresh)}` }] : []),
            ]}
            valueLabelDisplay="on"
            onChange={(_, value) => setFps(value as number)}
          />
        </Box>

        {overRefresh && (
          <Alert severity="warning" variant="outlined" sx={{ mb: 2 }}>
            {fps} fps is above this panel's {refresh?.toFixed(1)} Hz. Frames the display cannot paint are
            never seen by the camera, so the extra ones become keep-alive repeats rather than throughput.
          </Alert>
        )}

        <Alert severity="info" variant="outlined" sx={{ mb: 2 }}>
          The receiver's decode speed is the other ceiling, and it is usually the lower one. Raising the
          frame rate past what the receiver can read does not move more bytes — it produces frames that are
          photographed and discarded. Watch the decode rate on the receiver while you change this.
        </Alert>

        <Button
          variant="contained"
          disabled={fps === null || fps === data?.fps || apply.isPending}
          onClick={() => fps !== null && apply.mutate({ fps })}
        >
          Apply frame rate
        </Button>
      </Paper>

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Geometry
        </Typography>

        {busy && (
          <Alert severity="warning" variant="outlined" sx={{ mb: 2 }}>
            {data?.transmitting} transfer(s) are in flight, so the geometry cannot change. It is written into
            every frame header and the chunk size is derived from it — changing it now would render the
            remaining chunks to a shape the manifest never declared. The frame rate above is unaffected.
          </Alert>
        )}

        <Stack spacing={2} sx={{ maxWidth: 620 }}>
          <FormControl size="small" fullWidth disabled={busy}>
            <InputLabel id="grid">Grid and cell size</InputLabel>
            <Select
              labelId="grid"
              label="Grid and cell size"
              value={PRESETS.some((p) => `${p.w}x${p.h}@${p.cell}` === grid) ? grid : ''}
              onChange={(event) => setGrid(event.target.value)}
            >
              {PRESETS.map((preset) => (
                <MenuItem key={preset.label} value={`${preset.w}x${preset.h}@${preset.cell}`}>
                  {preset.label}
                </MenuItem>
              ))}
            </Select>
          </FormControl>

          <TextField
            size="small"
            label="Or type it: cols x rows @ cell px"
            value={grid}
            disabled={busy}
            onChange={(event) => setGrid(event.target.value)}
            helperText={
              chosen
                ? `${chosen.w}×${chosen.h} cells at ${chosen.cell} px renders about ${frameEdge} px square`
                : 'for example 256x256@8'
            }
          />

          <FormControl size="small" fullWidth disabled={busy}>
            <InputLabel id="encoder">Encoding</InputLabel>
            <Select
              labelId="encoder"
              label="Encoding"
              value={encoder}
              onChange={(event) => setEncoder(event.target.value)}
            >
              {(profiles.data?.encoders ?? []).map((option) => (
                <MenuItem key={option.name} value={option.name}>
                  {option.name} — {option.description}
                </MenuItem>
              ))}
            </Select>
          </FormControl>

          <Typography variant="caption" color="text.secondary">
            Cell size is the camera's constraint, not the panel's. The frame's capacity depends only on the
            grid — 256×256 carries the same bytes at 4 px cells as at 8 px — so a larger cell costs screen
            area and buys nothing except the thing that matters most in a real installation: the camera being
            able to resolve it. The documented envelope tolerates blur up to about a fifth of a cell, so 4 px
            cells leave under a pixel of margin.
          </Typography>

          <ErrorNotice error={apply.error} />
          {note && (
            <Alert severity="success" variant="outlined" onClose={() => setNote(null)}>
              {note}
            </Alert>
          )}

          <Box>
            <Button
              variant="contained"
              disabled={busy || !chosen || !geometryChanged || apply.isPending}
              onClick={() =>
                chosen &&
                apply.mutate({
                  grid_width: chosen.w,
                  grid_height: chosen.h,
                  cell_pixels: chosen.cell,
                  encoder,
                })
              }
            >
              Apply geometry
            </Button>
          </Box>
        </Stack>
      </Paper>

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Transfer channel
        </Typography>

        {busy && (
          <Alert severity="warning" variant="outlined" sx={{ mb: 2 }}>
            {data?.transmitting} transfer(s) are in flight, so the channel cannot change: the
            remaining frames would go somewhere the receiver is not watching.
          </Alert>
        )}

        <Alert severity="info" variant="outlined" sx={{ mb: 2 }}>
          This is not reloadable, unlike everything above it: the display opens its channel once at
          startup and nothing here re-opens it. A change here takes hold on the next restart — this
          process keeps writing to the current one until then.
        </Alert>

        <ToggleButtonGroup
          exclusive
          disabled={busy || sink.isPending}
          value={data?.sink ?? 'file'}
          onChange={(_, value) => value && sink.mutate(value)}
        >
          <ToggleButton value="file">Shared directory</ToggleButton>
          <ToggleButton value="none">Camera</ToggleButton>
        </ToggleButtonGroup>
        <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>
          <strong>Shared directory</strong> writes each frame to the directory the receiver's file
          camera reads — the loopback path most deployments actually use. <strong>Camera</strong>{' '}
          discards that write so nothing is duplicated onto disk while a real camera watches the
          physical display instead.
        </Typography>

        <ErrorNotice error={sink.error} />
      </Paper>

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Encryption keys
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
          Saved here, a key can be picked by name from the "Send a file" form instead of being
          pasted into every transfer — the key itself still never crosses the optical channel. A key
          is never shown again once it is saved; only its fingerprint is.
        </Typography>
        <ErrorNotice error={keys.error} />

        {(keys.data?.length ?? 0) === 0 ? (
          <Alert severity="info" variant="outlined" sx={{ mb: 2 }}>
            No keys saved yet.
          </Alert>
        ) : (
          <Table size="small" sx={{ mb: 2 }}>
            <TableBody>
              {keys.data!.map((k) => (
                <TableRow key={k.id}>
                  <TableCell>{k.label || '(no label)'}</TableCell>
                  <TableCell sx={{ fontFamily: 'monospace' }}>{k.fingerprint}</TableCell>
                  <TableCell align="right">
                    <IconButton
                      size="small"
                      aria-label={`delete ${k.label || k.fingerprint}`}
                      disabled={deleteKey.isPending}
                      onClick={() => deleteKey.mutate(k.id)}
                    >
                      <DeleteIcon fontSize="small" />
                    </IconButton>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}

        <ErrorNotice error={addKey.error} />
        <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} alignItems="flex-start">
          <TextField
            label="Label"
            value={keyLabel}
            onChange={(event) => setKeyLabel(event.target.value)}
            size="small"
          />
          <TextField
            label="Key (64 hex characters)"
            value={keyHex}
            onChange={(event) => setKeyHex(event.target.value)}
            error={keyHex.length > 0 && !keyValid}
            size="small"
            sx={{ minWidth: 320 }}
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
          />
          <Button
            variant="contained"
            disabled={!keyValid || addKey.isPending}
            onClick={() => addKey.mutate()}
          >
            Add key
          </Button>
        </Stack>
      </Paper>

      <Divider />

      <Typography variant="caption" color="text.secondary">
        Changes made here last until the process restarts. The configuration file is not rewritten — it is
        yours, with your comments in it — so set the values there or in the environment to make them
        permanent. See <code>docs/OPTIMAL-CONFIG.md</code> for the settings that reach 1 MB/s and what they
        require of the panel and the camera.
      </Typography>
        </>
      )}
    </Stack>
  )
}
