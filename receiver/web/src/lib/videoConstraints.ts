/**
 * What resolution and camera to ask the browser for.
 *
 * This one choice decides which geometries can be decoded at all. A frame is 132 cells across at a 128 grid and
 * 388 at a 384 grid, and the decoder needs roughly 3.8 camera pixels per cell to resolve a 7-cell finder
 * pattern. Filling a 1080-pixel short side gives 8.2 px/cell at 128 and only 2.8 at 384.
 *
 * So more pixels are better, up to the point where they are not, and that point arrives sooner than the
 * arithmetic suggests. Asking a phone for its full sensor was tried, and on a colour payload it made things
 * worse rather than better: at 4K the sensor resolves the display's own pixel grid, and the two grids beat
 * against each other. The result is moire and visible subpixel striping inside single cells — the cell's colour
 * is no longer one colour — and since a Bayer sensor already carries chroma at half resolution, that is the
 * measurement the palette match depends on being ruined. Every frame that has ever decoded on the handheld rig
 * was 1080p; the 4K captures failed, and downsampling them afterwards did not rescue them, because by then the
 * aliasing is baked in.
 *
 * A 1080p sensor mode bins pixels optically, before the demosaic, which averages the display's subpixels instead
 * of trying to resolve them. That is the correct trade for any grid this side of about 250 cells: below that,
 * 1080p still clears the 3.8 px/cell floor comfortably, and the colour it returns is worth more than the pixels
 * it gives up.
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
  // 1080p, deliberately, and not the sensor's best. See above: past this the camera starts resolving the
  // display's pixel grid rather than the frame drawn on it, and colour is the first casualty.
  const resolution = { width: { ideal: 1920 }, height: { ideal: 1080 } }

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
