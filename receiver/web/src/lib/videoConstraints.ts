/**
 * What resolution and camera to ask the browser for.
 *
 * This one choice decides which geometries can be decoded at all, which is not obvious from looking at it. A
 * frame is 132 cells across at a 128 grid and 388 at a 384 grid, and the decoder needs roughly 3.8 camera pixels
 * per cell to resolve a 7-cell finder pattern. Filling a 1080-pixel short side gives 8.2 px/cell at 128 —
 * comfortable — and only 2.8 at 384, below the floor however carefully the camera is aimed.
 *
 * It used to ask for 1080p, so a denser grid was not merely hard but impossible, and the failure was
 * indistinguishable from bad framing: nothing decoded, nothing even stored, and no explanation anywhere. At a
 * 2160-pixel short side the same 384 grid gives 5.6 px/cell and reads fine.
 */

/** What the caller knows about the camera it wants. */
export interface CameraChoice {
  /** A specific device the operator picked from the list, if any. */
  deviceId?: string
  /** Whether to prefer the rear camera, which is the one that can point at a display. Absent means no. */
  rearFacing?: boolean
}

/**
 * videoConstraints builds the `video` half of a getUserMedia request.
 *
 * Resolution is asked for as `ideal`, never `exact`: a preference the browser satisfies as closely as it can, so
 * a camera that cannot manage 4K returns its best instead of failing outright. Demanding a resolution is how you
 * get no camera at all on the hardware that needed the most help.
 */
export function videoConstraints(choice: CameraChoice): MediaTrackConstraints {
  // The sensor's best, because pixels per cell is the constraint that decides everything downstream.
  const resolution = { width: { ideal: 3840 }, height: { ideal: 2160 } }

  // A named device wins outright, and deliberately carries no facingMode: the two can contradict each other —
  // the device the operator picked may be the front one — and an explicit choice should not be second-guessed.
  if (choice.deviceId) {
    return { deviceId: { exact: choice.deviceId }, ...resolution }
  }

  return {
    facingMode: choice.rearFacing ? { ideal: 'environment' } : undefined,
    ...resolution,
  }
}
