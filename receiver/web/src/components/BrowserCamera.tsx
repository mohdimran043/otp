import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  FormControl,
  FormControlLabel,
  InputLabel,
  MenuItem,
  Paper,
  Select,
  Stack,
  Switch,
  Typography,
  useMediaQuery,
  useTheme,
} from '@mui/material'
import VideocamIcon from '@mui/icons-material/Videocam'
import VideocamOffIcon from '@mui/icons-material/VideocamOff'

import * as camera from '../lib/browserCamera'
import { useUi } from '../store/ui'
import { previewAspect } from '../lib/previewBox'
import type { CaptureDetail } from '../lib/videoConstraints'
import { AlignmentGuide, AlignmentOverlay, useAlignment } from './AlignmentGuide'
import { onPanel } from '../theme'

// Capturing with this browser's camera.
//
// This is the path that can actually ask permission. A server process opening /dev/video0 gets no dialog and no
// operating-system indicator: the permission was granted once, by whoever passed the device into the container.
// `getUserMedia` produces the prompt everybody recognises and the light the operating system controls — and it
// works from any machine that can reach the receiver, including a phone.
//
// The camera itself lives in ../lib/browserCamera, outside React. This component is a view over it. That split is
// not tidiness: holding the stream in the component meant navigating from Settings to Live capture unmounted it
// and released the camera, which is the worst possible moment — Live capture is where you go *because* you just
// started the camera and want to watch frames arrive.

interface Props {
  onStart: () => Promise<void>
  onStop: () => Promise<void>
  taking: boolean
}

export function BrowserCamera({ onStart, onStop, taking }: Props) {
  const state = useSyncExternalStore(camera.subscribe, camera.snapshot)
  const theme = useTheme()
  const onPhone = useMediaQuery(theme.breakpoints.down('sm'))

  // Only polled while the camera is running. Aiming advice about a camera that is not capturing is
  // both useless and misleading, and the request would run for as long as the tab stayed open.
  const alignment = useAlignment(state.running)

  const [devices, setDevices] = useState<MediaDeviceInfo[]>([])
  const [chosen, setChosen] = useState('')
  // Rear camera by default on a phone: the point is to photograph a display, and the front camera points at the
  // person holding it. On a laptop there is no rear camera and the preference is simply ignored.
  const [rearFacing, setRearFacing] = useState(onPhone)
  // Balanced by default, because colour is the fragile case and 1080p is what colour needs. Raising it
  // is the right move for a binary payload at a dense grid and the wrong one otherwise, so it is a
  // decision put in front of the operator rather than guessed at.
  const [detail, setDetail] = useState<CaptureDetail>('balanced')
  const { captureFormat, setCaptureFormat } = useUi()

  useEffect(() => setRearFacing(onPhone), [onPhone])

  const enumerate = useCallback(async () => {
    try {
      const all = await navigator.mediaDevices.enumerateDevices()
      setDevices(all.filter((d) => d.kind === 'videoinput'))
    } catch {
      // Not fatal: the camera can still be opened with the browser's default.
    }
  }, [])

  useEffect(() => {
    void enumerate()
  }, [enumerate])

  // The capture loop lives outside React, so the chosen format is pushed to it rather than read from
  // here. Applied on change and on mount, so a preference restored from storage takes effect without
  // the operator having to touch the control again.
  useEffect(() => {
    camera.setCaptureFormat(captureFormat)
  }, [captureFormat])

  const secure = typeof window !== 'undefined' && window.isSecureContext

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Typography variant="subtitle1" sx={{ mb: 1.5 }}>
        Use this browser's camera
      </Typography>

      <Stack spacing={2}>
        {!secure && (
          <Alert severity="error" variant="outlined">
            <strong>A browser will not give a camera to an insecure page.</strong> Open this receiver over HTTPS,
            or over <code>localhost</code>, and the prompt will appear. A plain <code>http://</code> address on a
            LAN is the one case that cannot work, whatever permissions are granted.
          </Alert>
        )}

        <Alert
          severity={state.running ? 'success' : 'info'}
          variant="outlined"
          icon={state.running ? <VideocamIcon fontSize="small" /> : <VideocamOffIcon fontSize="small" />}
        >
          {state.running ? (
            <>
              <strong>Capturing{state.label ? ` from ${state.label}` : ''}</strong>
              {state.width > 0 && ` at ${state.width}×${state.height}`}. The camera stays on while you move around
              this interface — it is released only when you press Stop or close the tab. Watch the frames land
              under <strong>Live capture</strong>.
            </>
          ) : (
            <>
              Your browser will ask permission, and the operating system will show its own indicator while the
              camera is in use. This works from any machine that can reach the receiver — including a phone, which
              is often the easiest camera to aim at a display.
            </>
          )}
        </Alert>

        {state.error && (
          <Alert severity="warning" variant="outlined">
            {state.error}
          </Alert>
        )}

        {!state.running && (
          <Stack spacing={1.5}>
            {devices.length > 1 && (
              <FormControl size="small" sx={{ maxWidth: 460 }}>
                <InputLabel id="browser-camera">Camera</InputLabel>
                <Select
                  labelId="browser-camera"
                  label="Camera"
                  value={chosen}
                  onChange={(event) => setChosen(event.target.value)}
                >
                  <MenuItem value="">Whichever the browser prefers</MenuItem>
                  {devices.map((device, index) => (
                    <MenuItem key={device.deviceId} value={device.deviceId}>
                      {device.label || `Camera ${index + 1}`}
                    </MenuItem>
                  ))}
                </Select>
              </FormControl>
            )}

            <FormControl size="small" sx={{ maxWidth: 460 }}>
              <InputLabel id="capture-detail">Capture detail</InputLabel>
              <Select
                labelId="capture-detail"
                label="Capture detail"
                value={detail}
                onChange={(e) => setDetail(e.target.value as CaptureDetail)}
              >
                <MenuItem value="balanced">Balanced — 1080p, best for colour</MenuItem>
                <MenuItem value="maximum">Maximum — full sensor, for dense binary grids</MenuItem>
              </Select>
            </FormControl>
            <Typography variant="body2" color="text.secondary">
              {detail === 'balanced'
                ? 'More pixels are not better for a colour payload: past 1080p the sensor resolves the panel’s own pixel grid and a cell stops being one colour.'
                : 'Full sensor gives a dense grid the pixels per cell it needs. A binary cell is thresholded rather than measured, so the panel’s subpixels barely touch it.'}
            </Typography>

            <FormControl size="small" sx={{ maxWidth: 460 }}>
              <InputLabel id="capture-format">Capture format</InputLabel>
              <Select
                labelId="capture-format"
                label="Capture format"
                value={captureFormat}
                onChange={(e) => setCaptureFormat(e.target.value as 'jpeg' | 'png')}
              >
                <MenuItem value="jpeg">JPEG — small frames, loses colour detail</MenuItem>
                <MenuItem value="png">Lossless — large frames, best for colour</MenuItem>
              </Select>
            </FormControl>
            <Typography variant="body2" color="text.secondary">
              {captureFormat === 'jpeg'
                ? 'JPEG stores colour at half resolution in each direction, and a colour payload carries its symbols entirely in colour — so a cell photographed at ten pixels has the thing that identifies it recorded at five. Measured on a two-lane display, this roughly doubles the fraction of cells left ambiguous. Keep it only if the link cannot carry lossless frames.'
                : 'Lossless keeps the colour detail the payload is actually carried in. Frames are several megabytes rather than several hundred kilobytes, so prefer this on a local link and watch for dropped frames over a tunnel.'}
            </Typography>

            {!chosen && (
              <FormControlLabel
                control={
                  <Switch checked={rearFacing} onChange={(e) => setRearFacing(e.target.checked)} size="small" />
                }
                label={
                  <Typography variant="body2">
                    Prefer the rear camera{' '}
                    <Typography component="span" variant="caption" color="text.secondary">
                      — on a phone this is the one to point at a display; a laptop has none and ignores it
                    </Typography>
                  </Typography>
                }
              />
            )}
          </Stack>
        )}

        {/* The preview is the honest confirmation: an indicator light can be believed, but seeing what the lens
            sees is how an operator knows it is pointed at the display.
 
            Its shape follows the stream rather than being fixed at 16/9. A phone streams portrait, and a 9:16
            stream letterboxed into a 16:9 box is a sliver between black bars — the whole frame is technically
            visible and far too small to frame against, which is exactly when an operator needs it most. It also
            fills the available width on a phone instead of stopping at 520px, since on a phone that cap was most
            of the screen going unused. */}
        <Box
          sx={{
            position: 'relative',
            bgcolor: '#000',
            borderRadius: 1,
            overflow: 'hidden',
            width: '100%',
            maxWidth: { xs: '100%', sm: 640 },
            mx: 'auto',
            aspectRatio: previewAspect(state.width, state.height),
            display: 'grid',
            placeItems: 'center',
          }}
        >
          <Box
            component="video"
            ref={camera.attach}
            muted
            playsInline
            sx={{
              width: '100%',
              height: '100%',
              objectFit: 'contain',
              display: state.running ? 'block' : 'none',
            }}
          />


          {/* The measured grid, drawn over the live preview. This is the part that makes aiming direct
              rather than inferential: the outline turns green the moment the frame actually decodes, so
              "is it working" is answered by looking at the screen instead of by reading a counter. */}
          {state.running && <AlignmentOverlay alignment={alignment.data} />}

          {/* Not text.secondary: this sits on the preview, which is black in either theme, and the
              light theme's secondary is a slate meant for a white page. */}
          {!state.running && (
            <Typography variant="body2" sx={{ color: onPanel }}>
              no camera running
            </Typography>
          )}
        </Box>

        {state.running && (
          <AlignmentGuide alignment={alignment.data} steadiness={state.steadiness} blurred={state.blurred} />
        )}

        {state.running && (
          <Typography variant="caption" color="text.secondary">
            Posting {state.width}x{state.height} — this preview is exactly what is being sent. The outline
            is the grid the decoder found: keep all four corners inside the frame, and aim for the reading
            below rather than for how it looks here.
          </Typography>
        )}

        {state.running && (
          <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
            <Chip size="small" label={`${state.sent} posted`} />
            <Chip
              size="small"
              color={state.accepted > 0 ? 'success' : 'default'}
              label={`${state.accepted} held a frame`}
            />
            <Chip size="small" variant="outlined" label={`${state.idle} showed nothing`} />
          </Stack>
        )}

        <Box>
          {!state.running ? (
            <Button
              variant="contained"
              color="success"
              size={onPhone ? 'large' : 'medium'}
              fullWidth={onPhone}
              startIcon={<VideocamIcon />}
              disabled={!secure}
              onClick={() =>
                void camera
                  .start({ deviceId: chosen || undefined, rearFacing, detail, prepare: onStart })
                  .then(enumerate)
                  .catch(() => undefined)
              }
            >
              Start camera
            </Button>
          ) : (
            <Button
              variant="contained"
              color="error"
              size={onPhone ? 'large' : 'medium'}
              fullWidth={onPhone}
              startIcon={<VideocamOffIcon />}
              onClick={() => void camera.stop(onStop)}
            >
              Stop camera
            </Button>
          )}
        </Box>

        {!state.running && taking && (
          <Typography variant="caption" color="text.secondary">
            The receiver is set to take posted frames but nothing is posting them. Press Start, or switch the
            source back to <code>file</code> to read frames from a directory instead.
          </Typography>
        )}
      </Stack>
    </Paper>
  )
}
