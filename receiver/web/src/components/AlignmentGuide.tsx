import { Box, Paper, Stack, Typography } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api, type AlignmentStatus, type AlignmentView } from '../api/client'
import { mono, signal } from '../theme'

// Live aiming feedback: the instrument face of this application.
//
// Aiming a camera at a display is otherwise done blind. The only signal is a decode rate that arrives
// seconds later and says nothing about which way to move, and an operator holding a phone at arm's
// length cannot read prose while they do it. So this is built for a glance from a metre away: one word
// large enough to read unfocused, one meter showing the number that decides whether anything decodes
// at all, and the detail underneath for when a glance is not enough.
//
// It polls rather than streaming. Every 400ms is well inside human reaction time, and it costs one
// small request against a receiver already taking ten photographs a second from the same page.
const pollMs = 400

// Words rather than sentences, because this is read while moving.
//
// Each verdict is one imperative and one clause of explanation. The imperative is what to do; the
// clause is why, for the operator who has time to wonder. Icons were tried here and removed: at this
// size the word is faster to read than a glyph, and a glyph beside it only competes with it.
const verdict: Record<AlignmentStatus, { colour: string; word: string; sub: string }> = {
  searching: { colour: signal.idle, word: 'SEARCHING', sub: 'no grid in view' },
  too_far: { colour: signal.adjust, word: 'CLOSER', sub: 'cells too small to read' },
  too_close: { colour: signal.adjust, word: 'BACK', sub: 'past the useful range' },
  off_axis: { colour: signal.adjust, word: 'SQUARE UP', sub: 'angle too steep' },
  marginal: { colour: signal.marginal, word: 'ALMOST', sub: 'found, not yet readable' },
  too_dense: { colour: signal.fault, word: 'TOO DENSE', sub: 'no aim fixes this' },
  good: { colour: signal.lock, word: 'LOCKED', sub: 'frames are decoding' },
}

function look(a: AlignmentView | undefined) {
  return verdict[a?.status ?? 'searching'] ?? verdict.searching
}

/**
 * AlignmentOverlay draws the grid the decoder actually found, over the live preview.
 *
 * This is what makes aiming direct rather than inferential: the outline sits on the thing being aimed
 * at, and turns green the moment frames decode, so "is it working" is answered by looking rather than
 * by reading a counter.
 *
 * Drawn in a 0..1 viewBox because the corners arrive normalised and the preview's size is a layout
 * decision with nothing to do with the capture's resolution. The SVG does that conversion for free and
 * stays correct when the element resizes or the phone is turned.
 */
export function AlignmentOverlay({ alignment }: { alignment: AlignmentView | undefined }) {
  if (!alignment?.live || !alignment.locked || alignment.corners?.length !== 4) return null

  const { colour } = look(alignment)
  const c = alignment.corners
  // Corners arrive top-left, top-right, bottom-left, bottom-right; a polygon needs perimeter order.
  const points = [c[0], c[1], c[3], c[2]]
    .filter((p): p is [number, number] => Array.isArray(p))
    .map(([x, y]) => `${x},${y}`)
    .join(' ')

  return (
    <Box
      component="svg"
      viewBox="0 0 1 1"
      preserveAspectRatio="none"
      aria-hidden
      sx={{ position: 'absolute', inset: 0, width: '100%', height: '100%', pointerEvents: 'none' }}
    >
      <polygon
        points={points}
        fill={`${colour}12`}
        stroke={colour}
        strokeWidth={2}
        strokeLinejoin="round"
        vectorEffect="non-scaling-stroke"
      />
      {c.map(([x, y], i) => (
        <circle key={i} cx={x} cy={y} r={0.011} fill={colour} />
      ))}
    </Box>
  )
}

/**
 * useAlignment polls the aiming state.
 *
 * Exported so the overlay and the panel read one reading rather than two polls a fraction of a second
 * apart, which would let the outline and the words disagree about the same frame.
 */
export function useAlignment(enabled: boolean) {
  return useQuery({
    queryKey: ['alignment'],
    queryFn: api.alignment,
    refetchInterval: enabled ? pollMs : false,
    enabled,
  })
}

/**
 * BandMeter shows one measurement against the window it has to land inside.
 *
 * A progress bar is the wrong instrument here, and shipping one was a real mistake: it showed pixels
 * per cell against a floor and turned green above it, so an operator sitting at nearly twice the
 * useful figure was told everything was fine while nothing decoded. The target is a *window* — below
 * it the cells are too small to read, above it the camera resolves the panel's own pixels instead of
 * the frame drawn on them — so the instrument has to draw both edges, not one.
 *
 * The window is scaled to the middle half of the track, leaving room either side to show how far
 * outside it you are. A meter that pins at its end stops being information exactly when the operator
 * most needs to know which way to move.
 */
function BandMeter({ value, lo, hi }: { value: number; lo: number; hi: number }) {
  const span = Math.max(hi - lo, 1)
  const min = lo - span
  const max = hi + span
  const at = (v: number) => Math.min(100, Math.max(0, ((v - min) / (max - min)) * 100))

  const inside = value >= lo && (hi <= 0 || value <= hi)
  const colour = inside ? signal.lock : signal.adjust

  return (
    <Box sx={{ position: 'relative', height: 36 }}>
      <Box sx={{ position: 'absolute', top: 16, left: 0, right: 0, height: 6, bgcolor: '#1e222a', borderRadius: 1 }} />
      <Box
        sx={{
          position: 'absolute',
          top: 16,
          height: 6,
          left: `${at(lo)}%`,
          width: `${at(hi) - at(lo)}%`,
          bgcolor: `${signal.lock}30`,
          borderLeft: `2px solid ${signal.lock}99`,
          borderRight: `2px solid ${signal.lock}99`,
        }}
      />
      <Box
        sx={{
          position: 'absolute',
          top: 8,
          left: `${at(value)}%`,
          width: 3,
          height: 22,
          bgcolor: colour,
          borderRadius: 0.5,
          transform: 'translateX(-1.5px)',
          boxShadow: `0 0 12px ${colour}`,
          transition: 'left 160ms linear, background-color 160ms linear',
        }}
      />
      <Typography
        sx={{
          position: 'absolute',
          top: 0,
          left: `${at(value)}%`,
          transform: 'translateX(-50%)',
          fontFamily: mono,
          fontSize: '0.62rem',
          color: colour,
          whiteSpace: 'nowrap',
        }}
      >
        {value.toFixed(1)}
      </Typography>
    </Box>
  )
}

/** Readout is one labelled figure, set so a row of them lines up under its own digits. */
function Readout({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <Box sx={{ minWidth: 78 }}>
      <Typography variant="caption" sx={{ color: 'text.secondary', display: 'block', lineHeight: 1.7 }}>
        {label}
      </Typography>
      <Typography sx={{ fontFamily: mono, fontSize: '0.95rem', color: tone ?? 'text.primary' }}>
        {value}
      </Typography>
    </Box>
  )
}

/**
 * AlignmentGuide is the readout under the preview.
 *
 * `steadiness` and `blurred` come from the browser rather than the receiver, because they describe
 * frames the receiver never saw. They belong beside the rest anyway: from the operator's side "my hand
 * is shaking" and "I am too far away" are the same question, and answering them in two places means
 * watching two places while trying to hold a phone still.
 */
export function AlignmentGuide({
  alignment,
  steadiness,
  blurred,
}: {
  alignment: AlignmentView | undefined
  steadiness: number
  blurred: number
}) {
  if (!alignment) return null

  const { colour, word, sub } = look(alignment)
  const lo = alignment.required_module_pixels
  const hi = alignment.max_module_pixels
  // Only mentioned once it is costing frames. A figure that is always on screen is one nobody reads.
  const shaky = blurred > 0 && steadiness > 0 && steadiness < 0.85

  return (
    <Paper
      sx={{
        p: 2.5,
        borderColor: `${colour}66`,
        // The panel carries the state as well as stating it, so the verdict is legible in peripheral
        // vision while the operator is looking at the phone rather than at this screen.
        background: `linear-gradient(180deg, ${colour}0d 0%, transparent 55%)`,
        transition: 'border-color 200ms linear',
      }}
    >
      <Stack direction="row" alignItems="baseline" spacing={1.5} flexWrap="wrap" useFlexGap>
        <Typography
          sx={{
            fontFamily: mono,
            fontWeight: 600,
            fontSize: 'clamp(1.6rem, 7vw, 2.4rem)',
            letterSpacing: '-0.04em',
            color: colour,
            lineHeight: 1,
            textShadow: `0 0 24px ${colour}55`,
          }}
        >
          {word}
        </Typography>
        <Typography variant="caption" sx={{ color: 'text.secondary' }}>
          {sub}
        </Typography>
      </Stack>

      <Typography variant="body2" sx={{ color: 'text.secondary', mt: 1.25 }}>
        {alignment.advice}
      </Typography>

      {shaky && (
        <Typography variant="body2" sx={{ mt: 1, color: signal.adjust }}>
          Hold steadier — {blurred} frame{blurred === 1 ? '' : 's'} dropped for movement. Bracing your
          elbows, or resting the phone against something, is worth more here than any setting.
        </Typography>
      )}

      {alignment.locked && (
        <Stack spacing={2.5} sx={{ mt: 2.5 }}>
          {lo > 0 && (
            <Box>
              <Stack direction="row" justifyContent="space-between" sx={{ mb: 0.5 }}>
                <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                  pixels per cell
                </Typography>
                <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                  {hi > 0 ? `aim ${lo.toFixed(0)}–${hi.toFixed(0)}` : `aim ${lo.toFixed(0)}+`}
                </Typography>
              </Stack>
              <BandMeter value={alignment.module_pixels} lo={lo} hi={hi} />
            </Box>
          )}

          <Stack direction="row" spacing={2.5} flexWrap="wrap" useFlexGap>
            <Readout label="fill" value={`${Math.round(alignment.fill * 100)}%`} />
            <Readout
              label="off-square"
              value={`${Math.round(alignment.perspective * 100)}%`}
              tone={alignment.perspective > 0.2 ? signal.adjust : undefined}
            />
            <Readout label="fiducials" value={`${Math.round(alignment.finder_score * 100)}%`} />
            <Readout label="contrast" value={Math.round(alignment.contrast).toString()} />
          </Stack>
        </Stack>
      )}
    </Paper>
  )
}
