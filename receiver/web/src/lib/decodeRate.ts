/**
 * How the camera is doing *now*, rather than how the session has done since it started.
 *
 * The panel first showed the capture session's lifetime figures, and that actively misled. A session lives as
 * long as the chosen capture source does, so once any transfer has succeeded the numbers read healthy for the
 * rest of the afternoon. An operator aiming a camera that was decoding nothing at all saw "2,565 frames
 * decoded, 70.2%", concluded the camera was working, and went looking for the fault somewhere else. For a panel
 * whose whole job is telling you whether your aim is right, a lifetime average is the wrong statistic.
 */

/** DecodeSample is one reading of the session's cumulative counters. */
export interface DecodeSample {
  /** Milliseconds, from the same clock throughout — only differences are used. */
  at: number
  decoded: number
  failed: number
}

export interface RecentDecode {
  /** Frames decoded within the window. */
  decoded: number
  /** Frames that arrived and could not be read within the window. */
  failed: number
  /**
   * Decoded as a fraction of frames that arrived, or null when none did.
   *
   * Null matters: no frames arriving is a different fault from every frame failing. The first means the camera
   * is not posting — stopped, or the frame does not look like a frame at all. The second means it is posting and
   * the picture cannot be read, which is aim, focus or distance. Reporting 0% for both would send an operator
   * after the wrong one.
   */
  rate: number | null
}

const nothing: RecentDecode = { decoded: 0, failed: 0, rate: null }

/**
 * recentDecode measures what changed across the samples given.
 *
 * The counters are cumulative, so the answer is the difference between the ends of the window; the caller
 * decides how wide that window is by how many samples it keeps.
 */
export function recentDecode(samples: DecodeSample[]): RecentDecode {
  if (samples.length < 2) return nothing

  const first = samples[0]!
  const last = samples[samples.length - 1]!

  const decoded = last.decoded - first.decoded
  const failed = last.failed - first.failed

  // A new capture session restarts the counters, which would otherwise read as a large negative rather than as
  // "this window spans a restart and says nothing".
  if (decoded < 0 || failed < 0) return nothing

  const arrived = decoded + failed
  if (arrived === 0) return nothing

  return { decoded, failed, rate: decoded / arrived }
}
