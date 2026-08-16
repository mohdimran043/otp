// Choosing how many pixels a cell is rendered at.
//
// The instinct is that bigger is better — more pixels a cell is what a camera needs — and it is wrong here,
// because the rendered size is not the displayed size. The display page scales a frame by a whole number and
// draws it with nearest-neighbour, so a frame rendered small and scaled up is pixel-identical to the same
// frame rendered large. What differs is the cost: the encoder draws every cell and PNG compresses every
// pixel, so the work grows with the square of the cell size, for a picture the camera cannot tell apart.
//
// Worse, "largest that fits" — which is what this used to do — is often smaller *on screen*. An 80-cell grid
// at 8 px a cell renders 672 px, and on a 1080-pixel panel that scales by one, because two would be 1344.
// The same grid at 4 px renders 336 and scales by three, to 1008. The bigger cell produced the smaller
// display and four times the encoding work.
//
// So the choice is: reach the largest whole-number-scaled size available, and among the cell sizes that
// reach it, take the cheapest to render.

/** frameEdgePx is a rendered frame's width, including its quiet zone. */
export function frameEdgePx(grid: number, cell: number, quietZone = 2): number {
  return (grid + 2 * quietZone) * cell
}

/**
 * displayedEdgePx is how large that frame ends up on a panel, after whole-number scaling.
 *
 * Whole numbers are not a simplification. A cell resampled across a fractional boundary is a cell the
 * decoder reads wrongly, so the display page scales by an integer or not at all — which means a frame
 * slightly too large for two times is shown at one times, and the "wasted" half of the panel is the reason
 * a larger cell can be the worse choice.
 */
export function displayedEdgePx(grid: number, cell: number, usable: number, quietZone = 2): number {
  const edge = frameEdgePx(grid, cell, quietZone)
  if (edge <= 0 || usable <= 0) return 0
  const scale = Math.floor(usable / edge)
  if (scale < 1) return 0
  return scale * edge
}

/**
 * minRenderedCell is the smallest cell size worth rendering at.
 *
 * Below this the saving is real and the risk is not worth it: the frame is upscaled by a large factor, and
 * everything then rests on the scaling being exactly nearest-neighbour with no resampling anywhere in the
 * browser, the compositor or the panel. Four is where this project's own measurements sit comfortably, and
 * it is what an operator who tuned this by hand arrived at independently.
 */
export const minRenderedCell = 4

/**
 * bestCellFor picks the cell size for a grid: the largest display it can reach, rendered as cheaply as
 * possible.
 *
 * `cells` is the offered sizes, `usable` the space one lane may occupy on the panel. Returns null when
 * nothing fits, which is the caller's cue that the grid is too large for this display rather than a reason
 * to render something that will overflow.
 */
export function bestCellFor(
  grid: number,
  cells: number[],
  usable: number,
  quietZone = 2,
): number | null {
  const fitting = cells.filter((c) => frameEdgePx(grid, c, quietZone) <= usable)
  if (fitting.length === 0) return null

  // The best on-screen size any of them reaches.
  let reach = 0
  for (const c of fitting) {
    reach = Math.max(reach, displayedEdgePx(grid, c, usable, quietZone))
  }

  // Anything within a few percent of that is the same picture to a camera, so among those take the
  // cheapest. The tolerance exists because insisting on the exact maximum would reject a cell that displays
  // at 1008 in favour of one that displays at 1020 and costs four times as much to render.
  const goodEnough = reach * 0.95

  let chosen: number | null = null
  for (const c of fitting) {
    if (displayedEdgePx(grid, c, usable, quietZone) < goodEnough) continue
    if (c < minRenderedCell) continue
    if (chosen === null || c < chosen) chosen = c
  }

  // Nothing at or above the floor reached it — a large grid on a small panel, where the only cell sizes
  // that fit are tiny. Take the largest that fits and let the readability check downstream have its say,
  // rather than silently choosing a geometry from a different grid.
  if (chosen === null) {
    for (const c of fitting) {
      if (chosen === null || c > chosen) chosen = c
    }
  }
  return chosen
}
