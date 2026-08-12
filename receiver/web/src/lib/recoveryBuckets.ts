/**
 * Which stage frames are failing at, and what an operator should do about it.
 *
 * The recovered-of-attempted figure alone says whether the recovery layer is working. It does not say what to
 * do next, and the two common situations call for opposite actions: the same "recovered 40 of 900" means
 * *move the camera* when the failures sit in no_quad, and *lower the grid density* when they sit in
 * payload_crc. Reporting only the ratio leaves an operator guessing between them.
 *
 * So the panel names the dominant failure stage and the action it implies. One stage rather than all ten,
 * because a table of ten counters on a page someone is reading while holding a phone at a screen is a table
 * nobody reads.
 */

/** A bucket's human name and the action it points to. */
export interface BucketAdvice {
  /** The bucket key as the receiver reports it. */
  key: string
  /** How many frames finished this way. */
  count: number
  /** Short label for the panel. */
  label: string
  /** What to do about it, or null when there is nothing to do. */
  action: string | null
}

/**
 * advice maps each stage to what it means. Keys match receiver/ai/classify's Bucket constants.
 *
 * `decoded` is present so it can be excluded by name rather than by guessing which keys are failures — a new
 * bucket added on the Go side should show up as an unexplained failure, not be silently treated as a success.
 */
const advice: Record<string, { label: string; action: string | null }> = {
  decoded: { label: 'Read', action: null },
  no_quad: {
    label: 'Corners not found',
    action: 'Fill more of the view and get the whole frame in shot — all four corner markers must be visible.',
  },
  degenerate_geometry: {
    label: 'Corners collinear',
    action: 'Square up to the screen; the view is too oblique to fit a geometry.',
  },
  descriptor_crc: {
    label: 'Grid size unreadable',
    action: 'Sharpen the focus, or name the sender grid in the decoder settings so it need not be read.',
  },
  header_crc: { label: 'Header unreadable', action: 'Sharpen the focus and reduce glare.' },
  footer_crc: {
    label: 'Footer unreadable',
    action: 'Sharpen the focus. Nothing can be recovered without the footer — it is what a correction is checked against.',
  },
  payload_crc: {
    label: 'Payload wrong',
    action: 'The frame is found and read but the cells are ambiguous. Move closer, or lower the grid density.',
  },
  below_floors: {
    label: 'Below confidence floor',
    action: 'Aim is marginal. Get squarer and closer, or lower the decoder confidence floors.',
  },
  unsupported_version: {
    label: 'Newer protocol',
    action: 'The sender is running a newer build than this receiver.',
  },
  other: { label: 'Unclassified', action: 'Check the receiver log; this stage has no description yet.' },
}

/**
 * dominantFailure returns the stage most frames failed at, or null when nothing has failed.
 *
 * Ties are broken by the bucket key so the panel does not flicker between two equal counts on every poll.
 */
export function dominantFailure(buckets: Record<string, number> | undefined): BucketAdvice | null {
  if (!buckets) return null

  let best: BucketAdvice | null = null
  for (const [key, count] of Object.entries(buckets)) {
    if (key === 'decoded' || !count) continue
    if (best && (count < best.count || (count === best.count && key > best.key))) continue
    const known = advice[key]
    best = {
      key,
      count,
      label: known?.label ?? key,
      action: known?.action ?? advice.other!.action,
    }
  }
  return best
}

/**
 * recoveryShare is recovered over attempted, or null when nothing was attempted.
 *
 * Null rather than zero for the same reason the decode rate uses null: nothing attempted means the channel is
 * either healthy or silent, and neither is "recovery is failing".
 */
export function recoveryShare(recovered: number, attempted: number): number | null {
  if (!attempted) return null
  return recovered / attempted
}
