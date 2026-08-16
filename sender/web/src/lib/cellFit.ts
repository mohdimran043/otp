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

  // Only cell sizes at or above the floor are candidates, and the floor is applied *before* the comparison
  // rather than as a filter on the winner. Applying it afterwards was subtly wrong and showed up at grid 96:
  // the largest displayed size there is reached only by 1 and 2 px cells, both below the floor, so no
  // candidate survived and the code fell through to "largest that fits" — 8 px, which displays at 800 and
  // costs four times what 4 px does to reach the same 800.
  const candidates = fitting.filter((c) => c >= minRenderedCell)

  // Nothing at or above the floor fits at all: a large grid on a small panel, where every cell size that
  // fits is tiny. Take the largest that does and let the readability check downstream have its say, rather
  // than silently choosing a geometry from a different grid.
  if (candidates.length === 0) {
    return fitting.reduce((a, b) => (b > a ? b : a))
  }

  // The best on-screen size reachable at or above the floor.
  let reach = 0
  for (const c of candidates) {
    reach = Math.max(reach, displayedEdgePx(grid, c, usable, quietZone))
  }

  // Anything within a few percent of that is the same picture to a camera, so among those take the
  // cheapest. The tolerance exists because insisting on the exact maximum would reject a cell that displays
  // at 1008 in favour of one that displays at 1020 and costs four times as much to render.
  const goodEnough = reach * 0.95

  let chosen: number | null = null
  for (const c of candidates) {
    if (displayedEdgePx(grid, c, usable, quietZone) < goodEnough) continue
    if (chosen === null || c < chosen) chosen = c
  }
  return chosen
}

/**
 * isColour reports whether an encoder carries more than one bit per cell.
 *
 * An encoder that has not loaded yet counts as colour, and that default is load-bearing rather than
 * cautious. Colour is the constrained case — a cell matched against eight palette entries needs several
 * times the camera pixels a thresholded one does — so the grid ceiling exists for it. Guessing "not colour"
 * removes the ceiling, which is the direction that produces an unreadable geometry: before the profiles
 * request returned, this form treated an empty encoder as monochrome and chose a 512-cell grid for a
 * deployment whose default encoding is colour8.
 */
export function isColour(encoder: string | undefined): boolean {
  return encoder !== 'grayscale'
}

/**
 * chooseGeometry resolves the (grid, cell) pair for whichever of the two was left on Auto.
 *
 * `usable` is the space one lane may occupy on the panel; the caller measures the screen, since a server
 * with no display attached cannot. `ceiling` bounds what Auto will choose — not what an operator may
 * choose deliberately.
 *
 * Grid is not maximised. Grid is capacity and cell size is legibility, and on a camera channel legibility
 * is the binding constraint: the ceiling is where a colour payload stops being readable at a realistic
 * framing, so Auto takes the largest grid *at or under* it rather than the largest that happens to fit the
 * screen. Fitting the screen is not the same question — a 512-cell grid fits a 1080 panel at two pixels a
 * cell and cannot be read at any distance.
 */
export function chooseGeometry(
  grid: 'auto' | number,
  cell: 'auto' | number,
  grids: number[],
  cells: number[],
  usable: number,
  encoder?: string,
  ceiling = Infinity,
): { grid: number; cell: number } {
  // An explicit choice is never second-guessed, whatever the encoder.
  if (grid !== 'auto' && cell !== 'auto') return { grid, cell }

  const allowed = isColour(encoder) ? grids.filter((g) => g <= ceiling) : grids
  const fallbackGrid = allowed[0] ?? grids[0]!

  if (grid !== 'auto') {
    return { grid, cell: bestCellFor(grid, cells, usable) ?? cells[0]! }
  }

  if (cell !== 'auto') {
    const fits = allowed.filter((g) => frameEdgePx(g, cell) <= usable)
    return { grid: fits.at(-1) ?? fallbackGrid, cell }
  }

  let best: { grid: number; cell: number } | null = null
  for (const g of allowed) {
    const c = bestCellFor(g, cells, usable)
    if (c === null) continue
    if (!best || g > best.grid) best = { grid: g, cell: c }
  }
  return best ?? { grid: fallbackGrid, cell: cells[0]! }
}
