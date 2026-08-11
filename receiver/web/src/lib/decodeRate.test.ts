import { describe, expect, it } from 'vitest'

import { type DecodeSample, recentDecode } from './decodeRate'

// Why this exists at all: the panel showed the capture session's lifetime figures. A session lives as long as
// the receiver's chosen source does — hours — so after any successful transfer it reads healthy for ever. An
// operator aiming a camera that is decoding nothing saw "2,565 frames decoded, 70.2%" and reasonably concluded
// the camera was fine and something else was broken. The number has to describe now, not the afternoon.

const at = (seconds: number, decoded: number, failed: number): DecodeSample => ({
  at: seconds * 1000,
  decoded,
  failed,
})

describe('recentDecode', () => {
  it('knows nothing from a single sample', () => {
    expect(recentDecode([at(0, 100, 5)])).toEqual({ decoded: 0, failed: 0, rate: null })
  })

  it('is empty with no samples', () => {
    expect(recentDecode([])).toEqual({ decoded: 0, failed: 0, rate: null })
  })

  // The counters are cumulative, so what happened recently is the difference between the ends of the window.
  it('reports the change across the window, not the totals', () => {
    const got = recentDecode([at(0, 1000, 100), at(5, 1030, 100), at(10, 1060, 100)])

    expect(got).toEqual({ decoded: 60, failed: 0, rate: 1 })
  })

  // The case that misled: a long healthy history, and then nothing but failures.
  it('reports zero when a healthy session stops decoding', () => {
    const got = recentDecode([at(0, 2565, 0), at(5, 2565, 400), at(10, 2565, 800)])

    expect(got).toEqual({ decoded: 0, failed: 800, rate: 0 })
  })

  it('reports a mixed rate', () => {
    const got = recentDecode([at(0, 100, 100), at(10, 130, 110)])

    expect(got.decoded).toBe(30)
    expect(got.failed).toBe(10)
    expect(got.rate).toBeCloseTo(0.75)
  })

  // Nothing arriving at all is not the same as everything failing, and an operator needs to tell them apart:
  // one means the camera is aimed wrong, the other means it is not posting.
  it('has no rate when no frames arrived either way', () => {
    expect(recentDecode([at(0, 500, 20), at(10, 500, 20)])).toEqual({ decoded: 0, failed: 0, rate: null })
  })

  // A new capture session restarts the counters from zero, which would otherwise read as a huge negative.
  it('ignores a window where the counters went backwards', () => {
    expect(recentDecode([at(0, 5000, 300), at(5, 12, 0)])).toEqual({ decoded: 0, failed: 0, rate: null })
  })
})
