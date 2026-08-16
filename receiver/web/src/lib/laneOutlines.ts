import type { AlignmentView } from '../api/client'

/**
 * One frame's outline, ready to draw.
 *
 * `points` is an SVG polygon in the 0..1 space the corners arrive in, and `decoded` is that lane's
 * own result — kept apart from the drawing so the caller decides what colour a reading lane is,
 * which is a theme decision rather than a geometric one.
 */
export interface LaneShape {
  points: string
  decoded: boolean
  frameNumber: number
}

/**
 * quad turns four normalised corners into polygon points.
 *
 * Corners arrive top-left, top-right, bottom-left, bottom-right, which is row order; a polygon needs
 * them in perimeter order, so the last two are swapped. Anything that is not four usable points is
 * rejected rather than drawn: a partial quad still renders, as a triangle slashed across the preview,
 * and it looks far more like a detection than like the missing data it is.
 */
function quad(corners: [number, number][] | undefined): string | null {
  if (!Array.isArray(corners) || corners.length !== 4) return null
  const ordered = [corners[0], corners[1], corners[3], corners[2]]
  if (!ordered.every((p) => Array.isArray(p) && p.length === 2 && p.every(Number.isFinite))) {
    return null
  }
  return ordered.map((p) => `${p![0]},${p![1]}`).join(' ')
}

/**
 * laneOutlines is every frame the receiver found in the last capture, as drawable outlines.
 *
 * The overlay drew a single box, taken from the lead lane's corners, which on a tiled display left
 * every other lane unmarked. That is not merely incomplete: an unmarked lane and a lane the receiver
 * cannot see look exactly the same through the preview, and telling those apart is the question the
 * operator is pointing the camera to answer.
 *
 * The count is whatever was found — two, four, sixteen — because nothing here knows or needs to know
 * the tiling. A receiver that does not report lanes at all falls back to the lead corners, so the
 * ordinary single-frame case is the same one box it always was rather than a special case beside it.
 */
export function laneOutlines(alignment: AlignmentView | undefined): LaneShape[] {
  if (!alignment?.live || !alignment.locked) return []

  const lanes = alignment.lanes?.length
    ? alignment.lanes
    : [{ corners: alignment.corners, decoded: alignment.decoded, frame_number: 0 }]

  const shapes: LaneShape[] = []
  for (const lane of lanes) {
    const points = quad(lane?.corners)
    if (points === null) continue
    shapes.push({
      points,
      decoded: Boolean(lane.decoded),
      frameNumber: lane.frame_number ?? 0,
    })
  }
  return shapes
}
