import {
  Alert,
  Box,
  Chip,
  CircularProgress,
  Dialog,
  DialogContent,
  DialogTitle,
  Divider,
  Stack,
  Typography,
} from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api, formatPercent, type CapturedFrame } from '../api/client'
import { mono, useSignal } from '../theme'

// What happened to one photograph.
//
// The grid of captures answers "is it working" and stops there. Every question after that is about a
// single picture: this one failed, why — was the grid never found, or found and misread? Did anything
// try to rescue it? On a tiled display, did the other lane of the *same photograph*, taken through the
// same lens at the same instant under the same exposure, read? That last comparison is the most direct
// evidence this system can offer about what the channel is doing, and it was previously unavailable at
// any price: the figures existed only in a log line that could not be joined to the image.
//
// The picture is shown at the size it was stored, not cropped to a thumbnail. It is the evidence, and
// the operator is looking at it to see glare, blur, a hand, a reflection — none of which survive being
// scaled into a 104-pixel tile.

/** stageAdvice turns a failure bucket into what it means and what to do about it. */
const stageAdvice: Record<string, { meaning: string; action: string }> = {
  no_quad: {
    meaning: 'Fewer than four corner markers were found, so there was no grid to read.',
    action: 'The camera is not seeing the display clearly enough — aim, focus, or move closer.',
  },
  degenerate_geometry: {
    meaning: 'Four markers were found but they do not describe a rectangle.',
    action: 'Usually a reflection or a pattern in the scene being mistaken for a marker.',
  },
  descriptor_crc: {
    meaning: 'The markers were found; the block that says what size the grid is did not read.',
    action: 'A few dozen cells in the corner of the header. Steadier or closer usually fixes it.',
  },
  header_crc: {
    meaning: 'The header band failed its checksum even after majority voting across its copies.',
    action: 'The frame was found but is being read badly. Hold steadier, or improve the light.',
  },
  footer_crc: {
    meaning: 'The footer band failed. Nothing can be recovered from this frame.',
    action: 'The footer is what a correction would be checked against, so there is no oracle left.',
  },
  payload_crc: {
    meaning: 'Geometry and both bands read perfectly; individual cell colours were misread.',
    action: 'This is the recoverable case, and the one recovery exists for. Larger cells help most.',
  },
  below_floors: {
    meaning: 'It decoded, but the confidence in the read was under this receiver’s floor.',
    action: 'Held back deliberately rather than trusted. Aim better, or lower the floor knowingly.',
  },
  unsupported_version: {
    meaning: 'The frame was produced by a newer protocol than this receiver knows.',
    action: 'Update the receiver.',
  },
}

/** Row is one labelled fact. */
function Row({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <Stack direction="row" spacing={2} sx={{ py: 0.4 }}>
      <Typography variant="caption" sx={{ color: 'text.secondary', minWidth: 128 }}>
        {label}
      </Typography>
      <Typography sx={{ fontFamily: mono, fontSize: '0.8rem', color: tone ?? 'text.primary' }}>
        {value}
      </Typography>
    </Stack>
  )
}

/**
 * outcome is the one-word verdict and the colour that carries it.
 *
 * Takes the palette rather than reading it, because the two grounds need different weights of the same
 * hue and this is called from module scope as well as from inside a component.
 */
function outcome(
  frame: CapturedFrame,
  sig: ReturnType<typeof useSignal>,
): { word: string; colour: string } {
  if (!frame.decoded) return { word: 'UNREADABLE', colour: sig.fault }
  if (frame.recovered) return { word: 'RECOVERED', colour: sig.marginal }
  return { word: 'READ', colour: sig.lock }
}

/** what the frame carried, in the operator's terms rather than the protocol's. */
function carried(frame: CapturedFrame): string {
  if (!frame.decoded) return '—'
  if (frame.is_manifest) return 'the manifest (the file’s description)'
  if (frame.is_parity) return `parity ${frame.chunk_number ?? '?'}`
  return `chunk ${frame.chunk_number ?? '?'}`
}

/** LaneSummary is one row per frame found in this photograph. */
function LaneSummary({ lanes, currentId }: { lanes: CapturedFrame[]; currentId: string }) {
  const sig = useSignal()
  if (lanes.length <= 1) return null

  return (
    <>
      <Divider sx={{ my: 1.5 }} />
      <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
        {lanes.length} frames in this one photograph
      </Typography>
      <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 1 }}>
        Tiled frames are found and read independently. They share this picture, so they share its
        exposure, its focus and its instant — which makes one reading while another fails the most
        direct evidence there is about where the channel is losing frames.
      </Typography>
      <Stack spacing={0.5}>
        {lanes.map((lane) => {
          const o = outcome(lane, sig)
          return (
            <Stack
              key={lane.id}
              direction="row"
              spacing={1.5}
              alignItems="center"
              sx={{
                px: 1,
                py: 0.5,
                borderRadius: 0.5,
                border: 1,
                borderColor: lane.id === currentId ? `${o.colour}88` : 'divider',
                bgcolor: lane.id === currentId ? `${o.colour}0d` : 'transparent',
              }}
            >
              <Chip size="small" label={`lane ${(lane.lane_index ?? 0) + 1}`} />
              <Typography sx={{ fontFamily: mono, fontSize: '0.72rem', color: o.colour, minWidth: 92 }}>
                {o.word}
              </Typography>
              <Typography variant="caption" sx={{ color: 'text.secondary', flexGrow: 1 }}>
                {lane.decoded ? carried(lane) : (lane.failure_stage || 'failed')}
              </Typography>
              {lane.id === currentId && <Chip size="small" variant="outlined" label="this one" />}
            </Stack>
          )
        })}
      </Stack>
    </>
  )
}

export function FrameDetailDialog({
  frameId,
  onClose,
}: {
  frameId: string | null
  onClose: () => void
}) {
  const sig = useSignal()
  const detail = useQuery({
    queryKey: ['frame', frameId],
    queryFn: () => api.frame(frameId!),
    enabled: frameId !== null,
  })

  const frame = detail.data?.frame
  const lanes = detail.data?.lanes ?? []
  const o = frame ? outcome(frame, sig) : null
  const advice = frame?.failure_stage ? stageAdvice[frame.failure_stage] : undefined

  return (
    <Dialog open={frameId !== null} onClose={onClose} maxWidth="md" fullWidth>
      <DialogTitle sx={{ pb: 1 }}>
        <Stack direction="row" spacing={2} alignItems="baseline" flexWrap="wrap" useFlexGap>
          <Typography sx={{ fontFamily: mono, fontSize: '1rem' }}>
            capture #{frame?.sequence ?? '…'}
          </Typography>
          {o && (
            <Typography sx={{ fontFamily: mono, fontWeight: 600, color: o.colour }}>{o.word}</Typography>
          )}
          {frame && (frame.lane_count ?? 1) > 1 && (
            <Chip size="small" label={`lane ${(frame.lane_index ?? 0) + 1} of ${frame.lane_count}`} />
          )}
        </Stack>
      </DialogTitle>

      <DialogContent dividers>
        {detail.isLoading && (
          <Stack alignItems="center" sx={{ py: 4 }}>
            <CircularProgress size={28} />
          </Stack>
        )}

        {detail.error && (
          <Alert severity="error" variant="outlined">
            Could not read this frame: {String(detail.error)}
          </Alert>
        )}

        {frame && (
          <Stack spacing={1}>
            {/* The evidence itself, at a size where glare, blur and movement are visible. */}
            <Box
              sx={{
                bgcolor: '#000',
                borderRadius: 0.5,
                border: 1,
                borderColor: 'divider',
                display: 'flex',
                justifyContent: 'center',
                maxHeight: 420,
                overflow: 'hidden',
              }}
            >
              <Box
                component="img"
                src={api.frameImageUrl(frame.id)}
                alt={`capture ${frame.sequence}`}
                sx={{ maxWidth: '100%', maxHeight: 420, objectFit: 'contain' }}
              />
            </Box>

            {!frame.decoded && advice && (
              <Alert severity="warning" variant="outlined" sx={{ mt: 1 }}>
                <strong>{frame.failure_stage}</strong> — {advice.meaning}
                <br />
                {advice.action}
              </Alert>
            )}

            {frame.recovered && (
              <Alert severity="info" variant="outlined" sx={{ mt: 1 }}>
                This frame did not read on the first attempt. The{' '}
                <strong>{frame.recovery_engine || 'recovery'}</strong> engine rescued it
                {frame.recovery_flips ? ` by correcting ${frame.recovery_flips} cells` : ''}
                {frame.recovery_candidates ? `, after trying ${frame.recovery_candidates} candidates` : ''}.
                Its payload still had to match the frame's own CRC32 and SHA-256, so it is the frame
                rather than a guess.
              </Alert>
            )}

            <Divider sx={{ my: 1 }} />

            <Typography variant="subtitle2">What it carried</Typography>
            <Box>
              <Row label="outcome" value={o!.word.toLowerCase()} tone={o!.colour} />
              <Row label="carried" value={carried(frame)} />
              {frame.frame_number !== undefined && (
                <Row label="frame number" value={String(frame.frame_number)} />
              )}
              {frame.transmission_id && <Row label="transfer" value={frame.transmission_id} />}
              <Row label="photographed" value={new Date(frame.captured_at).toLocaleString()} />
            </Box>

            <Divider sx={{ my: 1 }} />

            <Typography variant="subtitle2">How well it was seen</Typography>
            <Box>
              <Row
                label="fiducials"
                value={formatPercent(frame.finder_score)}
                tone={frame.finder_score >= 0.99 ? sig.lock : sig.adjust}
              />
              <Row label="timing" value={formatPercent(frame.timing_score)} />
              <Row label="contrast" value={frame.contrast.toFixed(1)} />
              <Row
                label="header bit errors"
                value={formatPercent(frame.bit_error_rate)}
                tone={frame.bit_error_rate > 0 ? sig.adjust : undefined}
              />
            </Box>

            <Divider sx={{ my: 1 }} />

            <Typography variant="subtitle2">What was tried</Typography>
            <Box>
              <Row
                label="decode"
                value={
                  frame.decode_ms
                    ? `${frame.decode_ms.toFixed(1)} ms`
                    : 'not recorded for this frame'
                }
              />
              {frame.merged_shots ? (
                <Row
                  label="merged shots"
                  value={`${frame.merged_shots} photographs of this frame combined`}
                  tone={sig.marginal}
                />
              ) : null}
              <Row
                label="recovery"
                value={
                  frame.recovered
                    ? `${frame.recovery_engine || 'engine'} · ${frame.recovery_stage || 'stage'} · ` +
                      `${frame.recovery_flips ?? 0} flips · ${frame.recovery_candidates ?? 0} candidates · ` +
                      `${(frame.recovery_ms ?? 0).toFixed(0)} ms`
                    : frame.decoded
                      ? 'not needed — it read first time'
                      : 'attempted and did not succeed'
                }
                tone={frame.recovered ? sig.marginal : undefined}
              />
              {frame.failure_stage && <Row label="died at" value={frame.failure_stage} tone={sig.fault} />}
              {frame.decode_error && (
                <Stack direction="row" spacing={2} sx={{ py: 0.4 }}>
                  <Typography variant="caption" sx={{ color: 'text.secondary', minWidth: 128 }}>
                    error
                  </Typography>
                  <Typography
                    sx={{ fontFamily: mono, fontSize: '0.72rem', color: sig.fault, wordBreak: 'break-word' }}
                  >
                    {frame.decode_error}
                  </Typography>
                </Stack>
              )}
            </Box>

            <LaneSummary lanes={lanes} currentId={frame.id} />
          </Stack>
        )}
      </DialogContent>
    </Dialog>
  )
}
