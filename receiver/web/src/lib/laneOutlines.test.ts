import { describe, expect, it } from 'vitest'

import { laneOutlines } from './laneOutlines'
import type { AlignmentView } from '../api/client'

// A square lane, as four normalised corners in the order the receiver sends them.
function corners(x: number, y: number, size: number): [number, number][] {
  return [
    [x, y],
    [x + size, y],
    [x, y + size],
    [x + size, y + size],
  ]
}

function alignment(over: Partial<AlignmentView> = {}): AlignmentView {
  return {
    live: true,
    lanes_found: 0,
    lanes_expected: 0,
    locked: true,
    decoded: true,
    fill: 0.5,
    module_pixels: 8,
    required_module_pixels: 6,
    max_module_pixels: 0,
    achievable_module_pixels: 8,
    max_grid_for_capture: 128,
    geometry_marginal: false,
    perspective: 0,
    finder_score: 1,
    timing_score: 1,
    contrast: 40,
    corners: corners(0.1, 0.1, 0.3),
    lanes: null,
    status: 'good',
    advice: '',
    at: '',
    ...over,
  } as AlignmentView
}

describe('laneOutlines', () => {
  it('draws one outline per lane, so a tiled display is not outlined one frame at a time', () => {
    const a = alignment({
      lanes: [
        { corners: corners(0.05, 0.3, 0.35), decoded: true, frame_number: 11 },
        { corners: corners(0.6, 0.3, 0.35), decoded: true, frame_number: 12 },
      ],
    })

    const shapes = laneOutlines(a)

    expect(shapes).toHaveLength(2)
    expect(shapes[0]!.frameNumber).toBe(11)
    expect(shapes[1]!.frameNumber).toBe(12)
    // Two boxes in two places. Both drawn at the lead lane's corners would look convincing and be wrong.
    expect(shapes[0]!.points).not.toBe(shapes[1]!.points)
  })

  it('scales to four lanes without being told the tiling', () => {
    const a = alignment({
      lanes: [
        { corners: corners(0.05, 0.05, 0.4), decoded: true, frame_number: 1 },
        { corners: corners(0.55, 0.05, 0.4), decoded: true, frame_number: 2 },
        { corners: corners(0.05, 0.55, 0.4), decoded: false, frame_number: 3 },
        { corners: corners(0.55, 0.55, 0.4), decoded: true, frame_number: 4 },
      ],
    })

    const shapes = laneOutlines(a)

    expect(shapes).toHaveLength(4)
    expect(new Set(shapes.map((s) => s.points)).size).toBe(4)
    // The one lane that is not reading is carried through as itself, which is what colours it apart.
    expect(shapes.map((s) => s.decoded)).toEqual([true, true, false, true])
  })

  it('puts the corners in perimeter order, not the row order they arrive in', () => {
    const a = alignment({
      lanes: [{ corners: corners(0, 0, 1), decoded: true, frame_number: 1 }],
    })

    // Row order would cross the polygon into a bow tie: top-left, top-right, bottom-left, bottom-right.
    expect(laneOutlines(a)[0]!.points).toBe('0,0 1,0 1,1 0,1')
  })

  it('falls back to the lead corners when the receiver reports no lanes', () => {
    const shapes = laneOutlines(alignment({ lanes: null }))

    expect(shapes).toHaveLength(1)
    expect(shapes[0]!.points).toBe('0.1,0.1 0.4,0.1 0.4,0.4 0.1,0.4')
  })

  it('draws nothing while searching, rather than a box at the origin', () => {
    expect(laneOutlines(alignment({ locked: false }))).toEqual([])
    expect(laneOutlines(alignment({ live: false }))).toEqual([])
    expect(laneOutlines(undefined)).toEqual([])
  })

  it('skips a lane whose corners are incomplete instead of drawing a triangle across the preview', () => {
    const a = alignment({
      lanes: [
        { corners: [[0.1, 0.1]] as [number, number][], decoded: true, frame_number: 1 },
        { corners: corners(0.5, 0.5, 0.3), decoded: true, frame_number: 2 },
      ],
    })

    const shapes = laneOutlines(a)

    expect(shapes).toHaveLength(1)
    expect(shapes[0]!.frameNumber).toBe(2)
  })
})
