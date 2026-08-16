import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  IconButton,
  Paper,
  Stack,
  ToggleButton,
  ToggleButtonGroup,
  Tooltip,
  Typography,
} from '@mui/material'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'

import { api, formatBytes, type DisplayFrame } from '../api/client'

// The display: the page a camera is pointed at.
//
// This is the optical channel's transmitting end when the channel is a real one. Three things about it
// are not cosmetic, and getting any of them wrong breaks decoding rather than merely looking untidy:
//
//  1. **Scaling is integer-only.** A frame is a grid of square cells, and the decoder samples the median
//     of each cell's pixels. Fractional scaling resamples cell edges into their neighbours — at 1.5×,
//     every other boundary lands mid-pixel and blends two cells that were meant to be distinguished.
//     Integer multiples replicate pixels exactly, so a cell stays a cell.
//  2. **Smoothing is off.** `image-rendering: pixelated` is the same argument: the browser's default
//     bilinear filter is a blur, and blur is precisely what the operating envelope budgets for the
//     lens. Spending it here leaves none for the optics.
//  3. **The next frame is decoded before it is swapped in.** Setting `src` and letting the browser
//     fetch would show white — or a half-painted image — for as long as the decode took, and a camera
//     that captured that moment would record a frame failure the channel never caused.

/** Scale is the integer magnification applied to the frame. `fit` picks the largest that still fits. */
type Scale = 'fit' | 1 | 2 | 3 | 4

interface Shown {
  meta: DisplayFrame
  src: string
}

export function Display() {
  const [params, setParams] = useSearchParams()
  const camera = params.get('camera') === '1'
  const [scale, setScale] = useState<Scale>('fit')
  const [shown, setShown] = useState<Shown | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [viewport, setViewport] = useState({ w: window.innerWidth, h: window.innerHeight })

  // The status panel is polled slowly and separately from the frame loop. It carries configuration and
  // totals, neither of which changes at frame rate, and folding it into the long-poll would mean either
  // sending it with every frame or not having it while the display is idle.
  const status = useQuery({
    queryKey: ['display'],
    queryFn: () => api.display(),
    refetchInterval: 2000,
  })

  // sequence lives in a ref rather than in state: the loop below reads it on every iteration, and a
  // state update would restart the effect and abort the request in flight.
  const sequence = useRef(0)

  useEffect(() => {
    const controller = new AbortController()
    let stopped = false

    const follow = async () => {
      while (!stopped) {
        try {
          const frame = await api.nextDisplayFrame(sequence.current, controller.signal)
          if (stopped) return
          if (!frame) continue // The poll expired with nothing new. Ask again.

          sequence.current = frame.sequence
          setError(null)

          const src = frame.image_png
            ? `data:image/png;base64,${frame.image_png}`
            : frame.image_url

          // Decode before swapping, so the visible image never goes blank between frames.
          const image = new Image()
          image.src = src
          try {
            await image.decode()
          } catch {
            // decode() is unavailable on a few browsers and rejects on a cancelled load. Showing the
            // frame anyway is strictly better than dropping it: the worst case is the flash this was
            // avoiding, and the alternative is a display that stops.
          }
          if (!stopped) setShown({ meta: frame, src })
        } catch (err) {
          if (stopped || controller.signal.aborted) return
          setError(err instanceof Error ? err.message : String(err))
          // Back off before retrying, so a sender that is down does not turn into a request flood.
          await new Promise((resolve) => setTimeout(resolve, 1000))
        }
      }
    }

    void follow()
    return () => {
      stopped = true
      controller.abort()
    }
  }, [])

  useEffect(() => {
    const onResize = () => setViewport({ w: window.innerWidth, h: window.innerHeight })
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  // Driving the display by hand.
  //
  // The hold is server-side, so `held` is read from the status rather than kept here: this page is not the
  // only viewer, and under camera mode it is not a viewer at all but the transmitting end of the channel.
  // A local flag would let two tabs disagree about whether the screen is stopped.
  const queryClient = useQueryClient()
  const held = status.data?.held ?? false
  const onDisplay = shown?.meta ?? status.data?.frame
  const transmission = onDisplay?.transmission_id
  const frameNumber = onDisplay?.frame_number ?? 0

  // The frame count comes from the transfer rather than the display, because the display knows only what is
  // on it now. Fetched once per transmission and cached, not polled: it cannot change for a transmission
  // whose frames have already been rendered.
  const transfer = useQuery({
    queryKey: ['transfer', transmission],
    queryFn: () => api.transfer(transmission as string),
    enabled: Boolean(transmission),
    staleTime: Infinity,
  })
  const frameCount = transfer.data?.frame_count ?? 0

  // Lanes can change while a transfer runs: every lane is an ordinary frame, so the ones already
  // rendered are equally valid shown one at a time or four at a time. Changing it mid-transfer is
  // exactly what an operator aiming a camera wants — fewer lanes means larger cells, and whether that
  // trade is worth making is something only the camera can answer.
  const setLanes = useMutation({
    mutationFn: (lanes: number) => api.updateSettings({ lanes }),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['settings'] }),
  })
  const settings = useQuery({ queryKey: ['settings'], queryFn: api.settings })
  const lanes = settings.data?.lanes ?? 1

  const setHold = useMutation({
    mutationFn: (next: boolean) => (next ? api.holdDisplay() : api.releaseDisplay()),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['display'] }),
    onError: (err: unknown) => setError(err instanceof Error ? err.message : String(err)),
  })

  const step = useMutation({
    mutationFn: (to: number) => api.showFrame(transmission as string, to),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['display'] }),
    onError: (err: unknown) => setError(err instanceof Error ? err.message : String(err)),
  })

  // Clamped rather than wrapped. Stepping off the end of a transmission and landing back at its beginning
  // would look like the display had jumped on its own, which is the one thing an operator watching a held
  // screen must be able to rule out.
  const stepBy = useCallback(
    (delta: number) => {
      if (!transmission || !frameCount) return
      const to = Math.min(Math.max(frameNumber + delta, 0), frameCount - 1)
      if (to !== frameNumber) step.mutate(to)
    },
    [transmission, frameCount, frameNumber, step],
  )

  const canStep = held && Boolean(transmission) && frameCount > 0

  // Arrow keys, because stepping through frames at the rig means one hand on the camera. Ignored while a
  // text field has focus, and absent in camera mode, which owns Escape for leaving.
  useEffect(() => {
    if (camera || !canStep) return
    const onKey = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null
      if (target && ['INPUT', 'TEXTAREA'].includes(target.tagName)) return
      if (event.key === 'ArrowLeft') {
        event.preventDefault()
        stepBy(-1)
      }
      if (event.key === 'ArrowRight') {
        event.preventDefault()
        stepBy(1)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [camera, canStep, stepBy])

  // The frame's own pixel size, held across the moment between frames.
  //
  // Every frame of a transmission is rendered at one size, so this should be a constant — and it is,
  // right up until a frame is swapped. `shown` is replaced wholesale on each new frame and the status
  // poll behind it can be a moment stale, so the two sources can both be briefly absent, and the
  // fallback of zero collapsed the container to nothing and then let it spring back. Under a camera
  // that reads as the display resizing on every frame, which is the one thing a display being
  // photographed must never do: the receiver's geometry search has to re-find the frame each time,
  // and an operator aiming at it sees the target move.
  //
  // A ref rather than state, deliberately. It must not itself cause a render — it is a memory of what
  // the size already was, and re-rendering because the size did not change would be the same fault
  // wearing a different hat.
  const measured = shown?.meta ?? status.data?.frame
  const lastSize = useRef({ w: 0, h: 0 })
  if (measured?.width_px && measured?.height_px) {
    lastSize.current = { w: measured.width_px, h: measured.height_px }
  }
  const width = lastSize.current.w
  const height = lastSize.current.h

  // The largest integer multiple that fits, never zero: a frame larger than the window is shown at 1×
  // and cropped rather than shrunk, because shrinking is the fractional scaling this must not do.
  const fitted = useMemo(() => {
    if (!width || !height) return 1
    const room = camera ? 1 : 0.82 // Leave space for the caption when there is one.
    return Math.max(1, Math.floor(Math.min(viewport.w / width, (viewport.h * room) / height)))
  }, [width, height, viewport, camera])

  const applied = scale === 'fit' ? fitted : scale
  const tooBig = width > 0 && (width * applied > viewport.w || height * applied > viewport.h)

  const enterCamera = useCallback(async () => {
    setParams({ camera: '1' })
    // Full screen matters for a real installation: browser chrome in the camera's field of view is
    // light the sensor has to expose for, and a frame that shares the exposure with a white toolbar
    // loses contrast where it needs it most.
    try {
      await document.documentElement.requestFullscreen()
    } catch {
      // Denied without a gesture, or unsupported. The page is still usable.
    }
  }, [setParams])

  const leaveCamera = useCallback(() => {
    setParams({})
    if (document.fullscreenElement) void document.exitFullscreen()
  }, [setParams])

  useEffect(() => {
    if (!camera) return
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') leaveCamera()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [camera, leaveCamera])

  const frameImage = shown ? (
    // The image sits inside a box of its own fixed size rather than sizing the layout itself.
    //
    // Swapping the source leaves an instant with no decoded picture, and an <img> with nothing in it
    // has no intrinsic size — so a layout that measured the image would collapse and re-expand on
    // every frame. The wrapper holds the space; only what is drawn inside it changes.
    <Box
      sx={{
        width: width * applied,
        height: height * applied,
        display: 'block',
        flex: 'none',
        overflow: 'hidden',
        backgroundColor: '#000',
      }}
    >
      <Box
        component="img"
        src={shown.src}
        alt={`frame ${shown.meta.frame_number}`}
        sx={{
          width: '100%',
          height: '100%',
          imageRendering: 'pixelated',
          display: 'block',
        }}
      />
    </Box>
  ) : (
    <Box
      sx={{
        width: 320,
        height: 320,
        display: 'grid',
        placeItems: 'center',
        border: '1px dashed',
        borderColor: 'divider',
        color: 'text.secondary',
      }}
    >
      <Typography variant="body2">nothing on the display</Typography>
    </Box>
  )

  // Camera mode is black and empty by everything except the frame. Any other pixel on the screen is
  // stray light in the camera's field of view.
  if (camera) {
    return (
      <Box
        onDoubleClick={leaveCamera}
        sx={{
          position: 'fixed',
          inset: 0,
          bgcolor: '#000',
          display: 'grid',
          placeItems: 'center',
          cursor: 'none',
          zIndex: (theme) => theme.zIndex.modal + 1,
        }}
      >
        {frameImage}
      </Box>
    )
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={2} alignItems="center" flexWrap="wrap" useFlexGap>
        <Typography variant="h5" sx={{ flexGrow: 1 }}>
          Display
        </Typography>
        <ToggleButtonGroup
          size="small"
          exclusive
          value={lanes}
          onChange={(_, value: number | null) => value !== null && setLanes.mutate(value)}
          disabled={setLanes.isPending}
        >
          {[1, 2, 4].map((n) => (
            <ToggleButton key={n} value={n}>
              {n} lane{n === 1 ? '' : 's'}
            </ToggleButton>
          ))}
        </ToggleButtonGroup>
        <ToggleButtonGroup
          size="small"
          exclusive
          value={scale}
          onChange={(_, value: Scale | null) => value !== null && setScale(value)}
        >
          <ToggleButton value="fit">Fit</ToggleButton>
          {([1, 2, 3, 4] as const).map((factor) => (
            <ToggleButton key={factor} value={factor}>
              {factor}×
            </ToggleButton>
          ))}
        </ToggleButtonGroup>
        <Button variant="contained" onClick={enterCamera}>
          Camera view
        </Button>
      </Stack>

      <Paper variant="outlined" sx={{ p: 1.5 }}>
        <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap" useFlexGap>
          <Button
            variant={held ? 'contained' : 'outlined'}
            color={held ? 'warning' : 'primary'}
            onClick={() => setHold.mutate(!held)}
            disabled={setHold.isPending}
          >
            {held ? 'Resume' : 'Pause'}
          </Button>

          <Tooltip title={canStep ? 'Previous frame (←)' : ''}>
            <span>
              <IconButton
                aria-label="previous frame"
                onClick={() => stepBy(-1)}
                disabled={!canStep || frameNumber <= 0 || step.isPending}
              >
                ◀
              </IconButton>
            </span>
          </Tooltip>

          <Typography variant="body2" sx={{ minWidth: 130, textAlign: 'center' }}>
            {transmission && frameCount ? (
              <>
                frame <strong>{frameNumber}</strong> of {frameCount}
              </>
            ) : (
              <span style={{ opacity: 0.6 }}>no transmission</span>
            )}
          </Typography>

          <Tooltip title={canStep ? 'Next frame (→)' : ''}>
            <span>
              <IconButton
                aria-label="next frame"
                onClick={() => stepBy(1)}
                disabled={!canStep || frameNumber >= frameCount - 1 || step.isPending}
              >
                ▶
              </IconButton>
            </span>
          </Tooltip>

          <Typography variant="caption" color="text.secondary" sx={{ flexGrow: 1 }}>
            {held
              ? 'Held. The display shows only what you choose, and the transfer is not charged for the time it spends waiting.'
              : 'Running at the configured frame rate. Pause to step through frames by hand.'}
          </Typography>
        </Stack>
      </Paper>

      <Typography variant="body2" color="text.secondary">
        This is what a camera sees. Point it at <strong>Camera view</strong> — full screen, black
        surround, nothing else on the page. Scaling is restricted to whole multiples and smoothing is
        off, because a cell resampled across its own boundary is a cell the decoder reads wrongly.
      </Typography>

      {error && (
        <Alert severity="warning" onClose={() => setError(null)}>
          {error}
        </Alert>
      )}

      {tooBig && (
        <Alert severity="info">
          The frame is {width}×{height} px at {applied}×, which is larger than this window. A real
          installation needs a panel that can show it without shrinking — reduce{' '}
          <code>cell_pixels</code> or the grid instead.
        </Alert>
      )}

      <Paper variant="outlined" sx={{ bgcolor: '#000', p: 2, display: 'grid', placeItems: 'center' }}>
        {frameImage}
      </Paper>

      <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
        {shown ? (
          <>
            <Chip size="small" label={`sequence ${shown.meta.sequence}`} />
            <Chip
              size="small"
              color={shown.meta.is_manifest ? 'secondary' : 'default'}
              label={shown.meta.is_manifest ? 'manifest frame' : `frame ${shown.meta.frame_number}`}
            />
            <Chip size="small" label={`${width}×${height} px`} />
            <Chip size="small" label={`${applied}× on screen`} />
            <Chip size="small" label={formatBytes(shown.meta.bytes)} />
          </>
        ) : (
          <Chip size="small" label="idle" />
        )}
        {status.data && (
          <>
            <Chip size="small" variant="outlined" label={`sink: ${status.data.sink}`} />
            <Chip size="small" variant="outlined" label={`${status.data.fps} fps configured`} />
            <Chip
              size="small"
              variant="outlined"
              label={`${status.data.grid_cols}×${status.data.grid_rows} cells at ${status.data.cell_px} px`}
            />
            <Chip
              size="small"
              variant="outlined"
              label={`${status.data.encoder}${status.data.bit_depth ? ` d${status.data.bit_depth}` : ''}`}
            />
            <Chip size="small" variant="outlined" label={`${status.data.frames_shown} shown`} />
            {status.data.held && <Chip size="small" color="warning" label="held" />}
          </>
        )}
      </Stack>

      {shown?.meta.transmission_id && (
        <Typography variant="caption" color="text.secondary">
          Transmitting {shown.meta.transmission_id}. The total shown climbs even while the picture
          holds still: a frame whose chunk has not been acknowledged is repeated rather than skipped,
          because a camera pointed at a blank screen learns nothing.
        </Typography>
      )}
    </Stack>
  )
}
