import { useEffect, useMemo, useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  FormControl,
  InputLabel,
  MenuItem,
  Paper,
  Radio,
  Select,
  Stack,
  Typography,
} from '@mui/material'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { api, type CameraDevice, type CameraMode } from '../api/client'
import { ErrorNotice } from './ErrorNotice'

// Choosing the camera.
//
// The list comes from Video4Linux rather than from a text field, and that is the point of the panel: a
// device path typed by hand is a guess, and the wrong guess is not obvious. Most webcams register two
// nodes — a capture device and a metadata device — with identical names, so an operator picking
// /dev/video1 because it was in the list gets a receiver that captures nothing and reports it as an
// optical fault. Only devices that declare a video capture capability appear here.
//
// The mode is chosen from the camera's own list for a related reason: a V4L2 driver handed a resolution it
// does not support substitutes one instead of failing, so a receiver that asked for 1920×1080 and was
// silently given 640×480 fails to resolve the cell grid and blames the lens.

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
  const cameras = useQuery({ queryKey: ['cameras'], queryFn: api.cameras })

  const [devicePath, setDevicePath] = useState('')
  const [mode, setMode] = useState('')
  const [note, setNote] = useState<string | null>(null)

  const devices = cameras.data?.devices ?? []

  // The form follows the server until the operator touches it. Seeding from `effective` rather than from
  // `selection` matters: with nothing configured, `selection` is empty and `effective` is the default
  // camera at its best mode — which is what the receiver would actually open, and therefore what the form
  // should show as current.
  useEffect(() => {
    if (!cameras.data || devicePath) return
    const effective = cameras.data.effective
    if (effective.device) setDevicePath(effective.device)
    if (effective.width && effective.height) {
      setMode(`${effective.format ?? ''}|${effective.width}x${effective.height}|${effective.fps ?? 0}`)
    }
  }, [cameras.data, devicePath])

  const selectedDevice = useMemo(
    () => devices.find((device) => device.path === devicePath),
    [devices, devicePath],
  )
  const modes = useMemo(() => flatten(selectedDevice), [selectedDevice])

  const apply = useMutation({
    mutationFn: async () => {
      const chosen = modes.find((entry) => entry.key === mode)
      return api.selectCamera({
        device: devicePath,
        format: chosen?.mode.format,
        width: chosen?.mode.width,
        height: chosen?.mode.height,
        fps: chosen && chosen.fps > 0 ? chosen.fps : undefined,
      })
    },
    onSuccess: (result) => {
      setNote(result.warning ?? result.note ?? 'The camera is in use.')
      void client.invalidateQueries({ queryKey: ['cameras'] })
      void client.invalidateQueries({ queryKey: ['config'] })
    },
  })

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Typography variant="subtitle1" sx={{ mb: 1 }}>
        Camera
      </Typography>

      <ErrorNotice error={cameras.error} />

      {cameras.data && !cameras.data.supported && (
        <Alert severity="info" variant="outlined" sx={{ mb: 2 }}>
          This platform cannot enumerate capture devices — Video4Linux is Linux's interface. That is not
          the same as having no camera attached.
        </Alert>
      )}

      {cameras.data?.error && (
        <Alert severity="warning" variant="outlined" sx={{ mb: 2 }}>
          The capture devices could not be listed: {cameras.data.error}
        </Alert>
      )}

      {cameras.data && cameras.data.supported && devices.length === 0 && !cameras.data.error && (
        <Alert severity="warning" variant="outlined" sx={{ mb: 2 }}>
          No capture device is attached. In a container this usually means none was passed through — add
          <code> devices: ["/dev/video0:/dev/video0"] </code>
          to the receiver's compose service.
        </Alert>
      )}

      {cameras.data && !cameras.data.source_uses_camera && (
        <Alert severity="info" variant="outlined" sx={{ mb: 2 }}>
          This receiver's capture source is <strong>{cameras.data.source}</strong>, so it is reading frames
          rather than photographing them. A camera chosen here is recorded and takes effect when the source
          is <code>gocv</code>.
        </Alert>
      )}

      {cameras.data?.substituted && (
        <Alert severity="warning" variant="outlined" sx={{ mb: 2 }}>
          The configured camera <code>{cameras.data.selection.device}</code> is not attached.{' '}
          <code>{cameras.data.effective.device}</code> would be used instead.
        </Alert>
      )}

      {devices.length > 0 && (
        <Stack spacing={2}>
          <Stack spacing={1}>
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

          <FormControl size="small" fullWidth disabled={modes.length === 0}>
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

          <Typography variant="caption" color="text.secondary">
            The list is ordered largest frame first, fastest breaking the tie. Resolution is the harder
            constraint: cells the camera cannot resolve do not decode at all, whereas a slow camera only
            makes the sender wait — which the acknowledgement rule already makes safe. Watch the format
            too: the same size is often offered at 30 fps compressed and 5 fps uncompressed, because the
            bus cannot carry raw frames any faster.
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
              disabled={!devicePath || apply.isPending}
              onClick={() => apply.mutate()}
            >
              {apply.isPending ? 'Applying…' : 'Use this camera'}
            </Button>
          </Box>
        </Stack>
      )}
    </Paper>
  )
}
