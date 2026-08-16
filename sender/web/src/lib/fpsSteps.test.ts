import { describe, expect, it } from 'vitest'

// The frame-rate stepper's arithmetic, which has one property worth holding: it must always move.
//
// The display's rate need not be one of the offered steps — a deployment can set anything in its
// environment — so "next step up" cannot be an index lookup. A stepper that returned the current value for
// an unlisted rate would appear dead on exactly the deployments most likely to be tuning it.

const FPS_STEPS = [0.5, 1, 2, 3, 4, 6, 8, 12]

function nextFps(current: number): number {
  return FPS_STEPS.find((f) => f > current) ?? FPS_STEPS[FPS_STEPS.length - 1]!
}

function previousFps(current: number): number {
  const below = FPS_STEPS.filter((f) => f < current)
  return below.at(-1) ?? FPS_STEPS[0]!
}

describe('frame rate stepping', () => {
  it('moves up and down through the offered rates', () => {
    expect(nextFps(1)).toBe(2)
    expect(previousFps(2)).toBe(1)
    expect(nextFps(4)).toBe(6)
    expect(previousFps(4)).toBe(3)
  })

  it('moves from a rate that is not one of the steps', () => {
    // A deployment setting 1.5 in its environment must still be able to press the buttons.
    expect(nextFps(1.5)).toBe(2)
    expect(previousFps(1.5)).toBe(1)
    expect(nextFps(10)).toBe(12)
    expect(previousFps(10)).toBe(8)
  })

  it('stops at the ends rather than wrapping or overshooting', () => {
    expect(nextFps(12)).toBe(12)
    expect(previousFps(0.5)).toBe(0.5)
    // And a rate beyond either end is brought back to the end rather than left alone.
    expect(nextFps(30)).toBe(12)
    expect(previousFps(0.1)).toBe(0.5)
  })

  it('always changes the rate, for every step', () => {
    for (const step of FPS_STEPS.slice(0, -1)) expect(nextFps(step)).toBeGreaterThan(step)
    for (const step of FPS_STEPS.slice(1)) expect(previousFps(step)).toBeLessThan(step)
  })
})
