import { useEffect, useRef } from 'react'
import { Alert, Box, IconButton, LinearProgress, Paper, Stack, Tooltip, Typography } from '@mui/material'
import VolumeOffIcon from '@mui/icons-material/VolumeOff'
import VolumeUpIcon from '@mui/icons-material/VolumeUp'
import { useQuery } from '@tanstack/react-query'

import { api, formatPercent } from '../api/client'
import { play } from '../lib/beep'
import { type DecodeSample, recentDecode } from '../lib/decodeRate'
import { type ScanState, scanEvents } from '../lib/scanEvents'
import { Stat } from './Stat'
import { useUi } from '../store/ui'

// How often the receiver is asked what it has decoded. Fast enough that a beep lands while the camera is still
// where it was, cheap enough not to matter: two small reads twice a second.
const pollMs = 500

// How many samples the recent-decode window keeps. Twenty at 500ms is the last ten seconds, which is long
// enough to smooth out one unlucky frame and short enough that moving the camera shows up while you are still
// moving it.
const windowSamples = 20

// What the camera is achieving, said out loud.
//
// An operator aiming a camera at a screen cannot watch the screen they are aiming at, and the page used to
// report only how many frames it had *sent* — plus "accepted", which means the frame was queued for decoding and
// says nothing about whether it decoded.
//
// Two things here are deliberately *not* taken from the capture session, because taking them from there was
// wrong in a way that cost an evening:
//
//   * Whether anything is arriving comes from the transmission actually existing, not from the session's
//     transmission_id. That field is the last transmission the session ever saw and it is never cleared, so
//     after any transfer — including one since deleted — it still names something. The panel read that as
//     "receiving", showed a progress bar against an unknown total, and left it animating over an idle channel.
//   * The decode figures are measured over a rolling window rather than taken as session lifetime totals. A
//     session lives for hours, so lifetime figures read healthy long after the camera stopped decoding
//     anything, which is precisely when an operator is looking at them for help.
export function ScanFeedback() {
  const { scanSound, setScanSound } = useUi()

  const session = useQuery({ queryKey: ['session'], queryFn: api.session, refetchInterval: pollMs })
  const candidateId = session.data?.transmission_id

  const transmission = useQuery({
    queryKey: ['transmission', candidateId],
    queryFn: () => api.transmission(candidateId!),
    refetchInterval: pollMs,
    enabled: Boolean(candidateId),
    // A transmission the session remembers but the store no longer has is gone, not a transient failure. One
    // quiet 404 is the answer, not something to hammer.
    retry: false,
  })

  // The transmission has to exist to count as arriving. Without this the session's stale id was enough to show a
  // progress bar for something that had been deleted.
  const live = Boolean(candidateId) && Boolean(transmission.data)
  const data = live ? transmission.data : undefined

  const state: ScanState = {
    transmissionId: live ? candidateId! : null,
    chunksArrived: data?.chunks_arrived ?? 0,
    hasManifest: Boolean(data?.manifest_received_at),
    verified: Boolean(data?.merged?.verified),
  }

  // Previous state in a ref: comparing successive polls is the whole mechanism, and holding it in state would
  // re-render on every poll and compare a value against itself.
  const previous = useRef<ScanState>({
    transmissionId: null,
    chunksArrived: 0,
    hasManifest: false,
    verified: false,
  })

  const fingerprint = `${state.transmissionId}:${state.chunksArrived}:${state.hasManifest}:${state.verified}`

  useEffect(() => {
    const events = scanEvents(previous.current, state)
    previous.current = state

    // Muting silences the sound, not the tracking: previous state advances either way, so unmuting does not
    // replay everything that happened while it was quiet.
    if (!scanSound) return
    events.forEach((event, index) => {
      if (index === 0) play(event)
      else window.setTimeout(() => play(event), index * 140)
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fingerprint, scanSound])

  // The rolling window over the session's cumulative counters.
  const samples = useRef<DecodeSample[]>([])
  const decodedTotal = session.data?.frames_decoded
  const failedTotal = session.data?.frames_failed
  if (decodedTotal !== undefined && failedTotal !== undefined) {
    const last = samples.current[samples.current.length - 1]
    if (!last || last.decoded !== decodedTotal || last.failed !== failedTotal) {
      samples.current = [
        ...samples.current.slice(-(windowSamples - 1)),
        { at: performance.now(), decoded: decodedTotal, failed: failedTotal },
      ]
    }
  }
  const recent = recentDecode(samples.current)

  const arrived = data?.chunks_arrived ?? 0
  const total = data?.chunk_count ?? 0

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Stack direction="row" alignItems="center" spacing={2} sx={{ mb: 2 }}>
        <Typography variant="subtitle1" sx={{ flexGrow: 1 }}>
          Scanning
        </Typography>
        <Tooltip title={scanSound ? 'Silence the beeps' : 'Beep on every chunk decoded'}>
          <IconButton onClick={() => setScanSound(!scanSound)} aria-label={scanSound ? 'mute' : 'unmute'}>
            {scanSound ? <VolumeUpIcon /> : <VolumeOffIcon />}
          </IconButton>
        </Tooltip>
      </Stack>

      {!live ? (
        // Split by what the camera is actually managing, because the two faults need opposite responses and
        // "nothing is arriving" alone sent an operator looking in the wrong place.
        recent.rate === null ? (
          <Alert severity="info" variant="outlined">
            No frames are reaching the decoder. Either the camera is not running, or what it sees does not look
            like a frame at all — the receiver wants the display to fill a good part of the view, so move the
            camera closer until the frame dominates the picture.
          </Alert>
        ) : recent.rate === 0 ? (
          <Alert severity="warning" variant="outlined">
            <strong>Frames are arriving but none of them can be read</strong> — {recent.failed.toLocaleString()}{' '}
            in the last few seconds. That is aim, focus or distance, not the transfer. Get square on to the
            screen, fill more of the view, and make sure the picture is sharp.
          </Alert>
        ) : (
          <Alert severity="info" variant="outlined">
            Decoding frames, but none belong to a transfer yet — {formatPercent(recent.rate)} of the last{' '}
            {(recent.decoded + recent.failed).toLocaleString()} frames read. Start a transfer on the sender.
          </Alert>
        )
      ) : (
        <Stack spacing={2}>
          <Box>
            <Stack direction="row" justifyContent="space-between" sx={{ mb: 0.5 }}>
              <Typography variant="body2" color="text.secondary">
                {data?.filename ?? 'Receiving'}
              </Typography>
              <Typography variant="body2" sx={{ fontVariantNumeric: 'tabular-nums' }}>
                {arrived} / {total || '?'} chunks
              </Typography>
            </Stack>
            {/* Determinate only when the total is known. An indeterminate bar reads as "working on it", which is
                exactly the wrong thing to show when nothing is happening. */}
            <LinearProgress
              variant={total > 0 ? 'determinate' : 'indeterminate'}
              value={total > 0 ? (arrived / total) * 100 : undefined}
              color={data?.merged?.verified ? 'success' : 'primary'}
            />
          </Box>

          {data?.merged?.verified && (
            <Alert severity="success" variant="outlined">
              Merged and verified against the hash the sender declared.
            </Alert>
          )}
        </Stack>
      )}

      {/* Shown rather than sounded: these change ten times a second, so a tone for each would drown out the
          beeps that mean progress. A decode rate falling is a lens drifting out of focus, visible long before a
          chunk stops arriving. */}
      <Stack direction="row" spacing={2} sx={{ mt: 2 }} flexWrap="wrap" useFlexGap>
        <Stat
          label="Decoding now"
          value={recent.rate === null ? '—' : formatPercent(recent.rate)}
          hint={
            recent.rate === null
              ? 'no frames reaching the decoder'
              : `${recent.decoded.toLocaleString()} read, ${recent.failed.toLocaleString()} unreadable, last 10s`
          }
          accent={recent.rate === null ? undefined : recent.rate > 0.5 ? 'success' : 'warning'}
        />
        <Stat
          label="Session total"
          value={(session.data?.frames_decoded ?? 0).toLocaleString()}
          hint="frames decoded since this capture source started — history, not now"
        />
      </Stack>
    </Paper>
  )
}
