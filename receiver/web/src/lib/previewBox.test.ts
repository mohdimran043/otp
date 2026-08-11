import { describe, expect, it } from 'vitest'

import { previewAspect } from './previewBox'

// The preview box was hard-coded to 16/9 while the stream can be any shape, and a phone streams portrait. With
// object-fit: contain that is technically honest — the whole frame is visible — and practically useless: a 9:16
// stream letterboxed into a 16:9 box becomes a narrow sliver with black bars either side, far too small to judge
// framing on. An operator aiming a camera at a screen then frames against something they cannot really see, which
// is how an entire evening went into photographs of ceilings and keyboards.
//
// Matching the box to the stream is what makes the preview worth having: the same shape as what is posted, using
// the whole width available.

describe('previewAspect', () => {
  it('matches a landscape stream', () => {
    expect(previewAspect(1920, 1080)).toBe('1920 / 1080')
  })

  it('matches a portrait stream, which is what a phone gives', () => {
    expect(previewAspect(1080, 1920)).toBe('1080 / 1920')
  })

  it('matches a square stream', () => {
    expect(previewAspect(1000, 1000)).toBe('1000 / 1000')
  })

  // Before the first frame arrives there are no dimensions, and the box still needs a shape to reserve space
  // rather than collapsing and then jumping.
  it('falls back to 16/9 when the stream has not reported a size yet', () => {
    expect(previewAspect(0, 0)).toBe('16 / 9')
    expect(previewAspect(1920, 0)).toBe('16 / 9')
    expect(previewAspect(0, 1080)).toBe('16 / 9')
  })

  it('ignores nonsense rather than producing an invalid CSS value', () => {
    expect(previewAspect(-1, 100)).toBe('16 / 9')
    expect(previewAspect(Number.NaN, 100)).toBe('16 / 9')
  })
})
