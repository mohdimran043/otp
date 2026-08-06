import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  FormControl,
  InputLabel,
  MenuItem,
  Paper,
  Select,
  Stack,
  Typography,
} from '@mui/material'
import VideocamIcon from '@mui/icons-material/Videocam'
import VideocamOffIcon from '@mui/icons-material/VideocamOff'

// Capturing with this browser's camera.
//
// This is the path that can actually ask permission. A server process opening /dev/video0 gets no dialog and no
// operating-system indicator: the permission was granted once, by whoever passed the device into the container.
// `getUserMedia` produces the prompt everybody recognises and the light the operating system controls — and it
// works from any machine that can reach the receiver, rather than only the one the receiver runs on.
//
// The camera is held by this page for as long as it is open. That is also what makes it stay on: the stream is
// owned by a MediaStream in this component, and it is released only when Stop is pressed or the page goes away.
//
// What it trades away is throughput. Encoding each frame in a canvas and posting it over HTTP will not keep up
// with reading V4L2 buffers directly, so this is the path for setting a camera up and watching it work, and the
// direct source is the path for moving fifty megabytes. Neither pretends to be the other.

interface Props {
  /** Called when capture starts or stops, so the page can switch the receiver's source to match. */
  onStart: () => Promise<void>
  onStop: () => Promise<void>
  /** Whether the receiver is currently taking posted frames. */
  taking: boolean
}

/** postRate is how many frames a second are sent. */
const postRate = 10

export function BrowserCamera({ onStart, onStop, taking }: Props) {
  const video = useRef<HTMLVideoElement | null>(null)
  const canvas = useRef<HTMLCanvasElement | null>(null)
  const stream = useRef<MediaStream | null>(null)
  const timer = useRef<number | null>(null)
  const posting = useRef(false)

  const [devices, setDevices] = useState<MediaDeviceInfo[]>([])
  const [chosen, setChosen] = useState('')
  const [running, setRunning] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [stats, setStats] = useState({ sent: 0, accepted: 0, idle: 0 })
  const [label, setLabel] = useState('')

  // Devices can only be enumerated with labels once permission has been given — before that the browser returns
  // empty names, deliberately, so that a page cannot inventory somebody's hardware without asking. So the list is
  // read again after the stream opens.
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

  const stop = useCallback(
    async (tellServer = true) => {
      if (timer.current !== null) {
        window.clearInterval(timer.current)
        timer.current = null
      }
      // Every track stopped, which is what releases the camera and turns the indicator off. Stopping the video
      // element alone would leave the device held.
      stream.current?.getTracks().forEach((track) => track.stop())
      stream.current = null
      if (video.current) video.current.srcObject = null
      setRunning(false)
      setLabel('')
      if (tellServer) {
        try {
          await onStop()
        } catch (err) {
          setError(err instanceof Error ? err.message : String(err))
        }
      }
    },
    [onStop],
  )

  // Released when the page goes away. Without this the camera stays on after navigating elsewhere, which is both
  // alarming and the kind of thing that gets a permission revoked.
  useEffect(() => () => void stop(false), [stop])

  const postOneFrame = useCallback(async () => {
    // One request at a time. Without this guard a slow post overlaps the next tick and the queue grows until the
    // browser is posting frames from several seconds ago.
    if (posting.current) return
    const element = video.current
    const surface = canvas.current
    if (!element || !surface || element.videoWidth === 0) return

    posting.current = true
    try {
      surface.width = element.videoWidth
      surface.height = element.videoHeight
      const context = surface.getContext('2d')
      if (!context) return
      context.drawImage(element, 0, 0)

      // JPEG at high quality rather than PNG: a PNG of a 1080p frame is megabytes and the compression artefacts
      // at this quality are far inside what the decoder tolerates — the optical envelope budgets for a lens.
      const blob = await new Promise<Blob | null>((resolve) =>
        surface.toBlob(resolve, 'image/jpeg', 0.92),
      )
      if (!blob) return

      const response = await fetch('/api/v1/capture/frames', {
        method: 'POST',
        headers: { 'Content-Type': blob.type },
        body: blob,
      })
      if (!response.ok) {
        const text = await response.text()
        let message = text
        try {
          const parsed = JSON.parse(text) as { error?: string }
          if (parsed.error) message = parsed.error
        } catch {
          // Keep the raw body.
        }
        setError(message || response.statusText)
        return
      }
      setError(null)
      const result = (await response.json()) as { accepted: boolean }
      setStats((s) => ({
        sent: s.sent + 1,
        accepted: s.accepted + (result.accepted ? 1 : 0),
        idle: s.idle + (result.accepted ? 0 : 1),
      }))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      posting.current = false
    }
  }, [])

  const start = useCallback(async () => {
    setError(null)
    setStats({ sent: 0, accepted: 0, idle: 0 })
    try {
      // The receiver is switched first, so the frames this is about to post have somewhere to go. Doing it the
      // other way round means the first second of frames is refused.
      await onStart()

      // The permission prompt happens here. Ideal rather than exact: a camera that cannot manage 1080p gives what
      // it can, and the frames are posted at whatever size arrives.
      const media = await navigator.mediaDevices.getUserMedia({
        video: chosen
          ? { deviceId: { exact: chosen }, width: { ideal: 1920 }, height: { ideal: 1080 } }
          : { width: { ideal: 1920 }, height: { ideal: 1080 } },
        audio: false,
      })
      stream.current = media
      if (video.current) {
        video.current.srcObject = media
        await video.current.play()
      }
      setLabel(media.getVideoTracks()[0]?.label ?? '')
      setRunning(true)
      await enumerate()

      timer.current = window.setInterval(() => void postOneFrame(), Math.round(1000 / postRate))
    } catch (err) {
      // The most common failure is the operator declining, and it deserves a sentence rather than a DOMException.
      const message = err instanceof Error ? err.message : String(err)
      setError(
        /denied|dismissed|NotAllowed/i.test(message)
          ? 'Permission to use the camera was declined. The browser will not ask again until you allow it in the ' +
            'address bar — look for the camera icon.'
          : message,
      )
      await stop(true)
    }
  }, [chosen, enumerate, onStart, postOneFrame, stop])

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Typography variant="subtitle1" sx={{ mb: 1.5 }}>
        Use this browser's camera
      </Typography>

      <Stack spacing={2}>
        <Alert
          severity={running ? 'success' : 'info'}
          variant="outlined"
          icon={running ? <VideocamIcon fontSize="small" /> : <VideocamOffIcon fontSize="small" />}
        >
          {running ? (
            <>
              <strong>Capturing{label ? ` from ${label}` : ''}.</strong> Your browser is holding the camera — its
              light stays on for as long as this page is open — and posting {postRate} frames a second to the
              receiver. Frames appear under <strong>Live capture</strong> as soon as the sender displays
              something.
            </>
          ) : (
            <>
              Your browser will ask permission to use the camera, and the operating system's indicator will show
              while it is in use. This works from any machine that can reach the receiver, so the camera does not
              have to be attached to the machine the receiver runs on.
            </>
          )}
        </Alert>

        {error && (
          <Alert severity="warning" variant="outlined" onClose={() => setError(null)}>
            {error}
          </Alert>
        )}

        {devices.length > 1 && (
          <FormControl size="small" sx={{ maxWidth: 460 }} disabled={running}>
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

        {/* The preview is the honest confirmation that a camera is running: an indicator light can be believed,
            but seeing what the lens sees is how an operator knows it is pointed at the display. */}
        <Box
          sx={{
            position: 'relative',
            bgcolor: '#000',
            borderRadius: 1,
            overflow: 'hidden',
            maxWidth: 520,
            aspectRatio: '16 / 9',
            display: 'grid',
            placeItems: 'center',
          }}
        >
          <Box
            component="video"
            ref={video}
            muted
            playsInline
            sx={{ width: '100%', height: '100%', objectFit: 'contain', display: running ? 'block' : 'none' }}
          />
          {!running && (
            <Typography variant="body2" color="text.secondary">
              no camera running
            </Typography>
          )}
        </Box>
        <Box component="canvas" ref={canvas} sx={{ display: 'none' }} />

        {running && (
          <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
            <Chip size="small" label={`${stats.sent} posted`} />
            <Chip
              size="small"
              color={stats.accepted > 0 ? 'success' : 'default'}
              label={`${stats.accepted} held a frame`}
            />
            <Chip size="small" variant="outlined" label={`${stats.idle} showed nothing`} />
          </Stack>
        )}

        <Box>
          {!running ? (
            <Button variant="contained" color="success" startIcon={<VideocamIcon />} onClick={() => void start()}>
              Start camera
            </Button>
          ) : (
            <Button
              variant="contained"
              color="error"
              startIcon={<VideocamOffIcon />}
              onClick={() => void stop(true)}
            >
              Stop camera
            </Button>
          )}
        </Box>

        {!running && taking && (
          <Typography variant="caption" color="text.secondary">
            The receiver is set to take posted frames but nothing is posting them. Press Start, or switch the
            source back to <code>file</code> to read frames from a directory instead.
          </Typography>
        )}
      </Stack>
    </Paper>
  )
}
