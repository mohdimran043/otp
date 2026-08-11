import { useEffect, useRef } from 'react'
import { Alert, Box, IconButton, LinearProgress, Paper, Stack, Tooltip, Typography } from '@mui/material'
import VolumeOffIcon from '@mui/icons-material/VolumeOff'
import VolumeUpIcon from '@mui/icons-material/VolumeUp'
import { useQuery } from '@tanstack/react-query'

import { api, formatPercent } from '../api/client'
import { play } from '../lib/beep'
import { type ScanState, scanEvents } from '../lib/scanEvents'
import { Stat } from './Stat'
import { useUi } from '../store/ui'

// How often the receiver is asked what it has decoded. Fast enough that a beep lands while the operator is
// still holding the camera where it was, slow enough not to matter: two small reads twice a second.
const pollMs = 500

// What the camera is achieving, said out loud.
//
// An operator aiming a camera at a monitor cannot watch the monitor they are aiming at, and until now the page
// told them only how many frames it had *sent* — plus "accepted", which means the frame was queued for
// decoding and nothing about whether it decoded. So there was no way to tell a well-aimed camera from a badly
// focused one without walking to another screen.
//
// Everything here comes from two endpoints that already existed. Nothing in the decode pipeline changed: the
// receiver was already recording all of this, it simply was not being shown where it was needed.
export function ScanFeedback() {
  const { scanSound, setScanSound } = useUi()

  const session = useQuery({ queryKey: ['session'], queryFn: api.session, refetchInterval: pollMs })
  const transmissionId = session.data?.transmission_id

  const transmission = useQuery({
    queryKey: ['transmission', transmissionId],
    queryFn: () => api.transmission(transmissionId!),
    refetchInterval: pollMs,
    enabled: Boolean(transmissionId),
  })

  const state: ScanState = {
    transmissionId: transmissionId ?? null,
    chunksArrived: transmission.data?.chunks_arrived ?? 0,
    hasManifest: Boolean(transmission.data?.manifest_received_at),
    verified: Boolean(transmission.data?.merged?.verified),
  }

  // The previous state lives in a ref rather than in state: comparing two renders is the whole mechanism, and
  // storing it in state would re-render on every poll and compare a value against itself.
  const previous = useRef<ScanState>({
    transmissionId: null,
    chunksArrived: 0,
    hasManifest: false,
    verified: false,
  })

  // Serialised into the dependency list so the effect runs when the numbers change rather than on every render
  // — an object literal is a new reference each time and would fire continuously.
  const fingerprint = `${state.transmissionId}:${state.chunksArrived}:${state.hasManifest}:${state.verified}`

  useEffect(() => {
    const events = scanEvents(previous.current, state)
    previous.current = state

    // Muting stops the sound, not the tracking: the previous state is advanced above either way, so unmuting
    // does not then replay everything that happened while it was quiet.
    if (!scanSound) return
    events.forEach((event, index) => {
      // Spaced slightly when a single poll caught more than one event, so a manifest and a chunk are heard as
      // two things rather than as one muddled noise.
      if (index === 0) play(event)
      else window.setTimeout(() => play(event), index * 140)
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fingerprint, scanSound])

  const arrived = transmission.data?.chunks_arrived ?? 0
  const total = transmission.data?.chunk_count ?? 0
  const decoded = session.data?.frames_decoded ?? 0
  const failed = session.data?.frames_failed ?? 0

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

      {!transmissionId ? (
        <Alert severity="info" variant="outlined">
          Nothing is arriving yet. Point the camera at the sender's display page and start a transfer — the
          first sound will be the manifest, a low note, followed by one higher beep per chunk decoded.
        </Alert>
      ) : (
        <Stack spacing={2}>
          <Box>
            <Stack direction="row" justifyContent="space-between" sx={{ mb: 0.5 }}>
              <Typography variant="body2" color="text.secondary">
                {transmission.data?.filename ?? 'Receiving'}
              </Typography>
              <Typography variant="body2" sx={{ fontVariantNumeric: 'tabular-nums' }}>
                {arrived} / {total || '?'} chunks
              </Typography>
            </Stack>
            <LinearProgress
              variant={total > 0 ? 'determinate' : 'indeterminate'}
              value={total > 0 ? (arrived / total) * 100 : undefined}
              color={transmission.data?.merged?.verified ? 'success' : 'primary'}
            />
          </Box>

          {transmission.data?.merged?.verified && (
            <Alert severity="success" variant="outlined">
              Merged and verified against the hash the sender declared.
            </Alert>
          )}
        </Stack>
      )}

      {/* The decode figures are the aiming feedback, and they are shown rather than sounded on purpose: they
          change ten times a second, so a tone for each would drown out the beeps that mean progress. A decode
          rate that falls is a lens drifting out of focus long before any chunk stops arriving. */}
      <Stack direction="row" spacing={2} sx={{ mt: 2 }} flexWrap="wrap" useFlexGap>
        <Stat label="Frames decoded" value={decoded.toLocaleString()} hint="this capture session" />
        <Stat
          label="Decode rate"
          value={formatPercent(session.data?.decode_rate ?? 0)}
          hint={failed > 0 ? `${failed.toLocaleString()} unreadable` : 'nothing unreadable yet'}
          accent={(session.data?.decode_rate ?? 0) > 0.75 ? 'success' : 'warning'}
        />
      </Stack>
    </Paper>
  )
}
