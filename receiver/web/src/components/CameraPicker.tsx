import { useEffect, useMemo, useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  Divider,
  FormControl,
  InputLabel,
  MenuItem,
  Paper,
  Radio,
  Select,
  Stack,
  TextField,
  ToggleButton,
  ToggleButtonGroup,
  Typography,
} from '@mui/material'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { api, type CameraDevice, type CameraMode } from '../api/client'
import { ErrorNotice } from './ErrorNotice'

// Choosing where frames come from, and which camera.
//
// The controls are always here, even when no device could be enumerated — which was the whole problem with
// the first version of this panel. It rendered a list, and with an empty list it rendered nothing but an
// explanation, so an operator in development had a page that told them what was wrong and no way to act on
// it. A device path can now be typed, and the server takes it on trust when it has no camera to check
// against: refusing a mode the camera says it cannot do is only defensible when the camera can be asked.
//
// The list is still preferred when there is one. It comes from Video4Linux rather than from a text field
// because a device path typed by hand is a guess, and the wrong guess is not obvious — most webcams register
// two nodes with identical names, one of which is a metadata device that produces no images. Only devices
// that declare a video capture capability appear.

/** modeKey identifies a mode in the select, since a mode has no id of its own. */
function modeKey(mode: CameraMode, fps: number): string {
  return `${mode.format}|${mode.width}x${mode.height}|${fps}`
}

/** flatten expands each mode into one entry per frame rate, which is what is actually selectable. */
function flatten(device: CameraDevice | undefined): { key: string; label: string; mode: CameraMode; fps: number }[] {
  if (!device) return []
  const out: { key: string; label: string; mode: CameraMode; fps: number }[] = []
  for (const mode of device.modes ?? []) {
    const rates = mode.fps && mode.fps.length > 0 ? mode.fps : [0]
    for (const fps of rates) {
      out.push({
        key: modeKey(mode, fps),
        label:
          fps > 0
            ? `${mode.width}×${mode.height} · ${fps % 1 === 0 ? fps : fps.toFixed(2)} fps · ${mode.format}`
            : `${mode.width}×${mode.height} · ${mode.format}`,
        mode,
        fps,
      })
    }
  }
  return out
}

export function CameraPicker() {
  const client = useQueryClient()
  const cameras = useQuery({ queryKey: ['cameras'], queryFn: api.cameras, refetchInterval: 10000 })

  const [source, setSource] = useState('')
  const [devicePath, setDevicePath] = useState('')
  const [mode, setMode] = useState('')
  const [width, setWidth] = useState('')
  const [height, setHeight] = useState('')
  const [fps, setFps] = useState('')
  const [format, setFormat] = useState('')
  const [note, setNote] = useState<string | null>(null)

  const devices = cameras.data?.devices ?? []
  const enumerated = devices.length > 0

  // The form follows the server until the operator touches it. Seeding from `effective` rather than
  // `selection` matters: with nothing configured, `selection` is empty and `effective` is what the receiver
  // would actually open, which is what the form should show as current.
  useEffect(() => {
    const view = cameras.data
    if (!view) return
    const e = view.effective
    setSource((current) => current || view.source)
    setDevicePath((current) => current || e.device || view.selection.device || '')
    if (e.width && e.height) {
      setMode((current) => current || `${e.format ?? ''}|${e.width}x${e.height}|${e.fps ?? 0}`)
      setWidth((current) => current || String(e.width))
      setHeight((current) => current || String(e.height))
    }
    if (e.fps) setFps((current) => current || String(e.fps))
    if (e.format) setFormat((current) => current || e.format || '')
  }, [cameras.data])

  const selectedDevice = useMemo(
    () => devices.find((device) => device.path === devicePath),
    [devices, devicePath],
  )
  const modes = useMemo(() => flatten(selectedDevice), [selectedDevice])

  const apply = useMutation({
    mutationFn: async () => {
      const chosen = modes.find((entry) => entry.key === mode)
      // From the enumerated mode when there is one, from the typed fields when there is not.
      return api.selectCamera({
        device: devicePath.trim(),
        source: source || undefined,
        format: chosen?.mode.format ?? (format.trim() || undefined),
        width: chosen?.mode.width ?? (Number(width) || undefined),
        height: chosen?.mode.height ?? (Number(height) || undefined),
        fps: chosen && chosen.fps > 0 ? chosen.fps : Number(fps) || undefined,
      })
    },
    onSuccess: (result) => {
      setNote(result.note ?? result.warning ?? 'Saved.')
      void client.invalidateQueries({ queryKey: ['cameras'] })
      void client.invalidateQueries({ queryKey: ['config'] })
    },
  })

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Typography variant="subtitle1" sx={{ mb: 1.5 }}>
        Capture source and camera
      </Typography>

      <ErrorNotice error={cameras.error} />

      <Stack spacing={2}>
        {/* Where frames come from. The first choice, and the one that decides whether the rest matters. */}
        <Box>
          <Typography variant="caption" color="text.secondary" display="block" sx={{ mb: 0.5 }}>
            Where frames come from
          </Typography>
          <ToggleButtonGroup
            size="small"
            exclusive
            value={source}
            onChange={(_, value: string | null) => value !== null && setSource(value)}
          >
            {(cameras.data?.known_sources ?? ['file', 'gocv']).map((option) => (
              <ToggleButton key={option} value={option}>
                {option === 'file' ? 'file — read a directory' : 'gocv — open a camera'}
              </ToggleButton>
            ))}
          </ToggleButtonGroup>
          {cameras.data && source !== cameras.data.source && (
            <Alert severity="info" variant="outlined" sx={{ mt: 1 }}>
              Changing the source takes effect when the receiver next starts. The capture loop holds its
              source open, so it is not swapped underneath a running session.
            </Alert>
          )}
        </Box>

        <Divider />

        {cameras.data && !cameras.data.supported && (
          <Alert severity="info" variant="outlined">
            This platform cannot enumerate capture devices — Video4Linux is Linux's interface. That is not the
            same as having no camera attached, so a device path can still be set below.
          </Alert>
        )}

        {cameras.data?.error && (
          <Alert severity="warning" variant="outlined">
            The capture devices could not be listed: {cameras.data.error}
          </Alert>
        )}

        {cameras.data?.substituted && (
          <Alert severity="warning" variant="outlined">
            The configured camera <code>{cameras.data.selection.device}</code> is not attached.{' '}
            <code>{cameras.data.effective.device}</code> would be used instead.
          </Alert>
        )}

        {enumerated ? (
          <Stack spacing={1}>
            <Typography variant="caption" color="text.secondary">
              Devices Video4Linux reports as able to capture video
            </Typography>
            {devices.map((device) => (
              <Box
                key={device.path}
                onClick={() => {
                  setDevicePath(device.path)
                  setMode('')
                }}
                sx={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 1,
                  p: 1,
                  borderRadius: 1,
                  border: 1,
                  borderColor: device.path === devicePath ? 'primary.main' : 'divider',
                  cursor: 'pointer',
                }}
              >
                <Radio size="small" checked={device.path === devicePath} />
                <Box sx={{ flexGrow: 1, minWidth: 0 }}>
                  <Typography variant="body2" sx={{ fontWeight: 500 }}>
                    {device.name}
                  </Typography>
                  <Typography variant="caption" color="text.secondary">
                    {device.path} · {device.driver}
                    {device.bus_info ? ` · ${device.bus_info}` : ''} · {(device.modes ?? []).length} modes
                  </Typography>
                </Box>
                {device.default && <Chip size="small" label="default" />}
                {device.selected && <Chip size="small" color="success" label="in use" />}
              </Box>
            ))}
          </Stack>
        ) : (
          <Alert severity="warning" variant="outlined">
            No capture device is attached, so there is nothing to list — but a device can still be named
            below and it will be taken on trust. In a container this usually means none was passed through:
            <Box component="pre" sx={{ m: 0, mt: 1, fontSize: 12, whiteSpace: 'pre-wrap' }}>
              docker compose -f demo/docker-compose.yml -f demo/docker-compose.camera.yml up -d
            </Box>
          </Alert>
        )}

        <TextField
          size="small"
          label="Device"
          value={devicePath}
          onChange={(event) => setDevicePath(event.target.value)}
          placeholder="/dev/video0"
          helperText={
            enumerated
              ? 'Chosen above, or type another path if you know it is there'
              : 'A Video4Linux path such as /dev/video0, or a bare camera index such as 0'
          }
          sx={{ maxWidth: 460 }}
        />

        {modes.length > 0 ? (
          <FormControl size="small" sx={{ maxWidth: 460 }}>
            <InputLabel id="camera-mode">Resolution and frame rate</InputLabel>
            <Select
              labelId="camera-mode"
              label="Resolution and frame rate"
              value={modes.some((entry) => entry.key === mode) ? mode : ''}
              onChange={(event) => setMode(event.target.value)}
            >
              {modes.map((entry, index) => (
                <MenuItem key={entry.key} value={entry.key}>
                  {entry.label}
                  {index === 0 ? ' — best available' : ''}
                </MenuItem>
              ))}
            </Select>
          </FormControl>
        ) : (
          // Nothing to choose from, so the mode is typed. The camera may still refuse it, and if it does the
          // symptom is a substituted resolution rather than an error — which is why the list is preferred
          // whenever there is one.
          <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
            <TextField
              size="small"
              label="Width"
              value={width}
              onChange={(e) => setWidth(e.target.value)}
              sx={{ width: 120 }}
            />
            <TextField
              size="small"
              label="Height"
              value={height}
              onChange={(e) => setHeight(e.target.value)}
              sx={{ width: 120 }}
            />
            <TextField
              size="small"
              label="FPS"
              value={fps}
              onChange={(e) => setFps(e.target.value)}
              sx={{ width: 110 }}
            />
            <TextField
              size="small"
              label="Format"
              value={format}
              onChange={(e) => setFormat(e.target.value)}
              placeholder="MJPG"
              sx={{ width: 130 }}
            />
          </Stack>
        )}

        <Typography variant="caption" color="text.secondary">
          Prefer the largest frame the camera offers, and watch the format: the same size is often 30 fps
          compressed and 5 fps uncompressed, because the bus cannot carry raw frames any faster. Resolution
          is the harder constraint — cells the camera cannot resolve do not decode at all, whereas a slow
          camera only makes the sender wait, which the acknowledgement rule already makes safe.
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
            disabled={apply.isPending || (!devicePath.trim() && source === cameras.data?.source)}
            onClick={() => apply.mutate()}
          >
            {apply.isPending ? 'Saving…' : 'Save capture settings'}
          </Button>
        </Box>
      </Stack>
    </Paper>
  )
}
