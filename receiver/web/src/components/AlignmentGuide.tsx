import { Box, Chip, LinearProgress, Paper, Stack, Typography } from '@mui/material'
import CheckCircleIcon from '@mui/icons-material/CheckCircle'
import SearchIcon from '@mui/icons-material/Search'
import ZoomInIcon from '@mui/icons-material/ZoomIn'
import ZoomOutIcon from '@mui/icons-material/ZoomOut'
import ScreenRotationIcon from '@mui/icons-material/ScreenRotation'
import WarningAmberIcon from '@mui/icons-material/WarningAmber'
import { useQuery } from '@tanstack/react-query'

import { api, type AlignmentStatus, type AlignmentView } from '../api/client'

// Live aiming feedback.
//
// Aiming a camera at a display is otherwise done blind: the only signal is a decode rate that
// arrives seconds later and says nothing about which way to move. Everything shown here is measured
// from the frame that was just captured, so moving the camera changes it immediately — which is
// what makes it usable while someone is still moving.
//
// It polls rather than streaming. A poll every 400ms is well inside the rate a person can react to,
// and it costs one small request against a receiver that is already taking ten frames a second from
// the same page.
const pollMs = 400

/** How each verdict is presented. Kept in one place so the colour, the icon and the words agree. */
const presentation: Record<AlignmentStatus, { colour: string; label: string; Icon: typeof SearchIcon }> = {
  searching: { colour: '#78909c', label: 'Looking', Icon: SearchIcon },
  too_far: { colour: '#fb8c00', label: 'Move closer', Icon: ZoomInIcon },
  too_close: { colour: '#fb8c00', label: 'Move back', Icon: ZoomOutIcon },
  off_axis: { colour: '#fb8c00', label: 'Square up', Icon: ScreenRotationIcon },
  marginal: { colour: '#fdd835', label: 'Almost', Icon: WarningAmberIcon },
  good: { colour: '#43a047', label: 'Good', Icon: CheckCircleIcon },
}

/**
 * AlignmentOverlay draws the detected grid over the preview.
 *
 * Positioned as a percentage of the element rather than in pixels, because the corners arrive
 * normalised and the preview's size is a layout decision that has nothing to do with the capture's
 * resolution. An SVG with a viewBox of 0..1 does that conversion for free and stays correct when
 * the element is resized, rotated, or shown on a phone in either orientation.
 */
export function AlignmentOverlay({ alignment }: { alignment: AlignmentView | undefined }) {
  if (!alignment?.live || !alignment.locked || alignment.corners?.length !== 4) return null

  const { colour } = presentation[alignment.status] ?? presentation.searching
  // Corners arrive top-left, top-right, bottom-left, bottom-right; a polygon needs them in
  // perimeter order, so the last two are swapped rather than sorted. Filtered rather than indexed
  // because the length check above is not something the type system carries across.
  const corners = alignment.corners
  const points = [corners[0], corners[1], corners[3], corners[2]]
    .filter((c): c is [number, number] => Array.isArray(c))
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
        fill="none"
        stroke={colour}
        strokeWidth={0.008}
        strokeLinejoin="round"
        vectorEffect="non-scaling-stroke"
      />
      {alignment.corners.map(([x, y], i) => (
        <circle key={i} cx={x} cy={y} r={0.012} fill={colour} />
      ))}
    </Box>
  )
}

/**
 * useAlignment polls the aiming state. Exported so the preview overlay and the panel below it read
 * the same reading rather than two polls a fraction of a second apart, which would let the border
 * and the words disagree.
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
 * AlignmentGuide is the panel of aiming advice shown under the preview.
 *
 * `steadiness` and `blurred` come from the browser rather than the receiver, because they describe frames the
 * receiver never saw: a smeared frame is dropped before it is posted. They belong here anyway — from the
 * operator's side "my hand is shaking" and "I am too far away" are the same question, and answering them in two
 * different places would mean watching two different places while trying to hold a phone still.
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

  const { colour, label, Icon } = presentation[alignment.status] ?? presentation.searching
  // Only worth mentioning once it is actually costing frames. A number that is always on screen is one nobody
  // reads, and a little shake that the gate absorbs without dropping anything is not a problem to report.
  const shaky = blurred > 0 && steadiness > 0 && steadiness < 0.85

  return (
    <Paper variant="outlined" sx={{ p: 2, borderColor: colour, borderWidth: 2 }}>
      <Stack direction="row" spacing={1.5} alignItems="center" sx={{ mb: 1 }}>
        <Icon sx={{ color: colour }} />
        <Typography variant="h6" sx={{ color: colour, fontWeight: 600 }}>
          {label}
        </Typography>
      </Stack>

      <Typography variant="body2" sx={{ mb: alignment.locked || shaky ? 2 : 0 }}>
        {alignment.advice}
      </Typography>

      {shaky && (
        <Typography variant="body2" sx={{ mb: alignment.locked ? 2 : 0, color: '#fb8c00' }}>
          Hold steadier — {blurred} frame{blurred === 1 ? '' : 's'} dropped for movement. Bracing your elbows,
          or resting the phone against something, is worth more here than any setting.
        </Typography>
      )}

      {alignment.locked && (
        <Stack spacing={1.5}>
          {/* Fill is the one figure worth showing as a bar rather than a number: it is the thing
              being adjusted, and a bar shows the target zone as well as the current value. */}
          <Box>
            <Stack direction="row" justifyContent="space-between">
              <Typography variant="caption" color="text.secondary">
                Frame fills the view
              </Typography>
              <Typography variant="caption" color="text.secondary">
                {Math.round(alignment.fill * 100)}%
              </Typography>
            </Stack>
            <LinearProgress
              variant="determinate"
              value={Math.min(100, alignment.fill * 100)}
              sx={{
                height: 8,
                borderRadius: 1,
                '& .MuiLinearProgress-bar': { backgroundColor: colour },
              }}
            />
            <Typography variant="caption" color="text.secondary">
              aim for 35–90%
            </Typography>
          </Box>

          <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
            <Chip
              size="small"
              color={
                alignment.module_pixels >= alignment.required_module_pixels &&
                (alignment.max_module_pixels <= 0 || alignment.module_pixels <= alignment.max_module_pixels)
                  ? 'success'
                  : 'warning'
              }
              label={
                alignment.required_module_pixels > 0
                  ? alignment.max_module_pixels > 0
                    ? `${alignment.module_pixels.toFixed(1)} px per cell (aim ${alignment.required_module_pixels.toFixed(0)}–${alignment.max_module_pixels.toFixed(0)})`
                    : `${alignment.module_pixels.toFixed(1)} / ${alignment.required_module_pixels.toFixed(0)} px per cell`
                  : `${alignment.module_pixels.toFixed(1)} px per cell`
              }
            />
            <Chip size="small" label={`${Math.round(alignment.perspective * 100)}% off-square`} />
            <Chip size="small" label={`fiducials ${Math.round(alignment.finder_score * 100)}%`} />
            <Chip size="small" label={`contrast ${Math.round(alignment.contrast)}`} />
          </Stack>
        </Stack>
      )}
    </Paper>
  )
}
