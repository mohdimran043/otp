import { describe, expect, it } from 'vitest'

import {
  bestCellFor,
  chooseGeometry,
  displayedEdgePx,
  frameEdgePx,
  minRenderedCell,
  usableFrameArea,
} from './cellFit'

const CELLS = [1, 2, 3, 4, 6, 8]

describe('cell size for a grid', () => {
  it('does not pick the largest cell that fits, because that is often the smaller display', () => {
    // The case that prompted this. On a 1080-pixel panel an 80-cell grid at 8 px renders 672 and scales by
    // one; at 4 px it renders 336 and scales by three, to 1008. Bigger cell, smaller picture, four times
    // the encoding work.
    expect(displayedEdgePx(80, 8, 1080)).toBe(672)
    expect(displayedEdgePx(80, 4, 1080)).toBe(1008)

    expect(bestCellFor(80, CELLS, 1080)).toBe(4)
  })

  it('renders far fewer pixels for the same picture', () => {
    const chosen = bestCellFor(80, CELLS, 1080)!
    const naive = 8 // largest that fits, the old rule

    const chosenPixels = frameEdgePx(80, chosen) ** 2
    const naivePixels = frameEdgePx(80, naive) ** 2

    expect(chosenPixels).toBeLessThan(naivePixels / 3)
    // And the operator sees a larger frame, not a smaller one, which is what makes this free.
    expect(displayedEdgePx(80, chosen, 1080)).toBeGreaterThan(displayedEdgePx(80, naive, 1080))
  })

  it('picks a small cell at 96, where the floor used to be applied too late', () => {
    // The case that prompted this. At grid 96 the largest displayed size is reached only by 1 and 2 px
    // cells, both under the floor — so filtering the *winner* by the floor left nothing, and the code fell
    // through to "largest that fits". That was 8 px: it displays at 800, exactly what 4 px displays at, for
    // four times the pixels to draw and compress.
    expect(displayedEdgePx(96, 8, 1080)).toBe(800)
    expect(displayedEdgePx(96, 4, 1080)).toBe(800)

    expect(bestCellFor(96, CELLS, 1080)).toBe(4)
  })

  it('keeps every offered grid off the expensive end', () => {
    // A cell size of 8 is never the right answer on a panel this size: whatever it reaches, a smaller cell
    // reaches at least as much for a quarter of the work.
    for (const grid of [64, 80, 96, 128, 192, 256]) {
      expect(bestCellFor(grid, CELLS, 1080)).toBeLessThanOrEqual(4)
    }
  })

  it('never renders below the floor, where exact upscaling stops being a safe bet', () => {
    // 1 px a cell also reaches 1008 here, and is not chosen: the whole result would rest on every stage
    // from the browser to the panel scaling by nearest neighbour with no resampling.
    expect(displayedEdgePx(80, 1, 1080)).toBe(1008)
    expect(bestCellFor(80, CELLS, 1080)).toBeGreaterThanOrEqual(minRenderedCell)
  })

  it('falls back to what fits when the grid is large for the panel', () => {
    // A 512 grid on a 1080 panel: (512+4)*2 = 1032 fits, *3 does not. Below the floor, but the alternative
    // is refusing a geometry the operator explicitly chose.
    expect(bestCellFor(512, CELLS, 1080)).toBe(2)
  })

  it('reports nothing fitting rather than overflowing the panel', () => {
    // A 1024 grid needs 1028 px at one pixel a cell, which does not fit a 640-pixel lane.
    expect(bestCellFor(1024, CELLS, 640)).toBeNull()
  })

  it('accounts for lanes, since a lane gets a share of the panel and not all of it', () => {
    // Four lanes on a 1080 panel leave 540 for each. The 80-cell grid at 6 px is 504 and scales by one;
    // at 4 px it is 336 and also scales by one, to 336 — so the larger cell genuinely wins here.
    expect(bestCellFor(80, CELLS, 540)).toBe(6)
  })

  it('is stable across the grids the sender offers', () => {
    for (const grid of [64, 80, 96, 128, 192, 256]) {
      const chosen = bestCellFor(grid, CELLS, 1080)
      expect(chosen).not.toBeNull()
      // Whatever it picks must actually fit, or the display overflows and a lane leaves the shot.
      expect(frameEdgePx(grid, chosen!)).toBeLessThanOrEqual(1080)
      // And it must be at least as large on screen as the old "largest that fits" rule managed.
      const largestThatFits = CELLS.filter((c) => frameEdgePx(grid, c) <= 1080).at(-1)!
      expect(displayedEdgePx(grid, chosen!, 1080)).toBeGreaterThanOrEqual(
        displayedEdgePx(grid, largestThatFits, 1080),
      )
    }
  })
})

// What the upload form defaults to, which is the thing an operator actually sees.
//
// The presets and the ceiling are the page's, repeated here rather than imported, because these tests are
// about the resolved *answer* and not about the constants. If the page changes them, this should be read
// again rather than quietly following along.
const GRIDS = [64, 80, 96, 128, 192, 256, 384, 512, 1024]
const COLOUR_CEILING = 80

describe('the geometry Auto resolves to', () => {
  it('defaults to 80 cells at 4 pixels, not a grid nothing can read', () => {
    // The regression this exists for: on a 1080-pixel panel, a 512-cell grid fits at 2 px a cell, so a rule
    // that maximised grid chose 512 — a geometry no camera reads at any distance.
    expect(chooseGeometry('auto', 'auto', GRIDS, CELLS, 1080, 'color8', COLOUR_CEILING)).toEqual({
      grid: 80,
      cell: 4,
    })
  })

  it('still caps the grid before the encoder has loaded', () => {
    // The form renders before the profiles request returns, so the encoder is empty. Treating that as
    // monochrome removed the ceiling and chose 512 — for a deployment whose default encoding is colour.
    expect(chooseGeometry('auto', 'auto', GRIDS, CELLS, 1080, undefined, COLOUR_CEILING).grid).toBe(80)
    expect(chooseGeometry('auto', 'auto', GRIDS, CELLS, 1080, '', COLOUR_CEILING).grid).toBe(80)
  })

  it('lifts the cap for a monochrome payload, which genuinely can carry more', () => {
    // A thresholded cell needs far fewer camera pixels than one matched against a palette, so the ceiling
    // is not its constraint and the grid should climb.
    const mono = chooseGeometry('auto', 'auto', GRIDS, CELLS, 1080, 'grayscale', COLOUR_CEILING)
    expect(mono.grid).toBeGreaterThan(COLOUR_CEILING)
  })

  it('never second-guesses a grid and cell the operator chose', () => {
    expect(chooseGeometry(512, 8, GRIDS, CELLS, 1080, 'color8', COLOUR_CEILING)).toEqual({
      grid: 512,
      cell: 8,
    })
  })

  it('sizes the cell for a grid the operator chose, ceiling or not', () => {
    // Choosing 128 deliberately is allowed — the ceiling bounds Auto, not the operator — and it still gets
    // the cheapest cell that reaches the largest scaled size.
    const chosen = chooseGeometry(128, 'auto', GRIDS, CELLS, 1080, 'color8', COLOUR_CEILING)
    expect(chosen.grid).toBe(128)
    expect(chosen.cell).toBeGreaterThanOrEqual(minRenderedCell)
    expect(frameEdgePx(chosen.grid, chosen.cell)).toBeLessThanOrEqual(1080)
  })

  it('picks the largest capped grid that fits when the cell is pinned', () => {
    expect(chooseGeometry('auto', 8, GRIDS, CELLS, 1080, 'color8', COLOUR_CEILING).grid).toBe(80)
  })

  it('accounts for lanes, where each lane gets a share of the panel', () => {
    // Four lanes on a 1080 panel leave 540 each, and the answer must still fit that share.
    const chosen = chooseGeometry('auto', 'auto', GRIDS, CELLS, 540, 'color8', COLOUR_CEILING)
    expect(frameEdgePx(chosen.grid, chosen.cell)).toBeLessThanOrEqual(540)
  })
})

// Predicting what the Display page can actually show.
//
// The form used to measure the physical screen while the page measures the browser's viewport times the
// room it keeps for a caption. The screen is always larger, so the form could choose a geometry the page
// could not show — and the page will not rescue it by scaling fractionally, because a cell resampled across
// a fractional boundary is a cell the decoder reads wrongly.
describe('the area a frame actually has', () => {
  it('is smaller than the screen, because the page is not the panel', () => {
    const screen = 1080
    expect(usableFrameArea(1920, screen, 1)).toBeLessThan(screen)
  })

  it('stops grid 512 from choosing a frame that overflows every real viewport', () => {
    const usable = usableFrameArea(1920, 1080, 1)
    const cell = bestCellFor(512, CELLS, usable)

    // Whatever it picks must fit the space the page will give it.
    expect(cell).not.toBeNull()
    expect(frameEdgePx(512, cell!)).toBeLessThanOrEqual(usable)

    // The old answer, against the raw screen, was 2 — and 516*2 = 1032 does not fit.
    expect(frameEdgePx(512, 2)).toBe(1032)
    expect(1032).toBeGreaterThan(usable)
  })

  it('leaves every offered grid showable at a whole multiple', () => {
    const usable = usableFrameArea(1920, 1080, 1)
    for (const grid of GRIDS) {
      const chosen = bestCellFor(grid, CELLS, usable)
      if (chosen === null) continue // too large for this panel at any offered cell, which is honest
      expect(displayedEdgePx(grid, chosen, usable)).toBeGreaterThan(0)
      expect(frameEdgePx(grid, chosen)).toBeLessThanOrEqual(usable)
    }
  })

  it('divides the panel between lanes', () => {
    const one = usableFrameArea(1920, 1080, 1)
    const four = usableFrameArea(1920, 1080, 4)
    expect(four).toBeLessThan(one)
    // Four lanes are two across and two down, so each gets about half of each axis.
    expect(four).toBeCloseTo(one / 2, -1)
  })

  it('never returns a negative or absurd area from an odd screen', () => {
    expect(usableFrameArea(320, 100, 1)).toBeGreaterThan(0)
    expect(usableFrameArea(0, 0, 4)).toBeGreaterThanOrEqual(0)
  })
})
