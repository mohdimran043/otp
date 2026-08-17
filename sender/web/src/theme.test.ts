import { describe, expect, it } from 'vitest'

import { instrument, signalDark, signalLight } from './theme'

// The theme, tested where it actually broke.
//
// The first light theme typechecked, passed every test, and rendered dark. `instrument` selected the ground
// correctly and then built its component overrides from the dark constants regardless, so choosing light
// produced dark panels with light text on them — legible enough in a screenshot of the top bar to look
// finished, and wrong everywhere. Nothing in a type or a render test noticed, because nothing asserted what
// colour anything came out.
//
// So these assert the built theme rather than the source: what MUI would actually paint. That is the only
// form of this test that would have failed on the broken version.

/** luminance is WCAG relative luminance, which is what a contrast ratio is built from. */
function luminance(hex: string): number {
  const h = hex.replace('#', '')
  const channel = (at: number) => {
    const v = parseInt(h.slice(at, at + 2), 16) / 255
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * channel(0) + 0.7152 * channel(2) + 0.0722 * channel(4)
}

/** contrast is the WCAG ratio between two colours, 1 (identical) to 21 (black on white). */
function contrast(a: string, b: string): number {
  const la = luminance(a)
  const lb = luminance(b)
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05)
}

describe('instrument', () => {
  it('defaults to dark, because these pages share a room with a panel being photographed', () => {
    expect(instrument().palette.mode).toBe('dark')
    expect(instrument().palette.background.default).toBe('#08090b')
  })

  // The regression. Every one of these came out dark on the broken version while the mode said 'light'.
  it('paints light surfaces in light mode, not merely light text', () => {
    const light = instrument('light')

    expect(light.palette.mode).toBe('light')
    expect(light.palette.background.default).toBe('#f6f7f9')
    expect(light.palette.background.paper).toBe('#ffffff')

    const paper = light.components?.MuiPaper?.styleOverrides?.root as Record<string, string>
    expect(paper.backgroundColor).toBe('#ffffff')
    expect(paper.borderColor).toBe('#dde1e7')

    const bar = light.components?.MuiAppBar?.styleOverrides?.root as Record<string, string>
    expect(bar.backgroundColor).toContain('#f6f7f9')
    expect(bar.borderBottom).toContain('#dde1e7')

    const body = (light.components?.MuiCssBaseline?.styleOverrides as Record<string, never>)
      ?.body as unknown as Record<string, string>
    expect(body.backgroundColor).toBe('#f6f7f9')
    expect(body.backgroundImage).not.toContain('#1e222a')
  })

  it('keeps the dark ground out of the light theme entirely, except where a tooltip inverts', () => {
    const dark = ['#08090b', '#101216', '#e8eaed', '#8b93a1']
    const serialised = JSON.stringify(instrument('light').components)

    for (const colour of dark) {
      expect(serialised, `${colour} is a dark-ground colour and must not appear in the light theme`)
        .not.toContain(colour)
    }
    // The one deliberate exception: a tooltip stays dark on a light page, as every other application's does.
    const tooltip = instrument('light').components?.MuiTooltip?.styleOverrides?.tooltip as Record<
      string,
      string
    >
    expect(tooltip.backgroundColor).toBe('#1e222a')
  })

  it('carries every signal role in both grounds', () => {
    for (const mode of ['dark', 'light'] as const) {
      const p = instrument(mode).palette
      const sig = mode === 'light' ? signalLight : signalDark
      expect(p.primary.main).toBe(sig.lock)
      expect(p.success.main).toBe(sig.lock)
      expect(p.warning.main).toBe(sig.adjust)
      expect(p.error.main).toBe(sig.fault)
      // info was the gap: undefined here meant MUI's own blue, the one colour from outside this palette.
      expect(p.info.main).toBe(sig.idle)
    }
  })
})

describe('the signal palette', () => {
  it('names the same states in both grounds', () => {
    expect(Object.keys(signalLight).sort()).toEqual(Object.keys(signalDark).sort())
  })

  // A light theme built by reusing the dark hues is the failure this is guarding against: #fde047 reads as
  // a warning on black and is very nearly invisible on white.
  it('does not reuse a dark-ground hue on the light ground', () => {
    for (const key of Object.keys(signalDark) as (keyof typeof signalDark)[]) {
      expect(signalLight[key], `${key} must be re-chosen for the light ground, not reused`)
        .not.toBe(signalDark[key])
    }
  })

  it('reads against its own ground', () => {
    // 3:1 is the large-text and non-text threshold. These are the display verdict, the meter marker and
    // the outlines — all large or graphical, none of them body copy.
    for (const key of Object.keys(signalDark) as (keyof typeof signalDark)[]) {
      expect(contrast(signalDark[key], '#08090b'), `dark ${key}`).toBeGreaterThanOrEqual(3)
      expect(contrast(signalLight[key], '#f6f7f9'), `light ${key}`).toBeGreaterThanOrEqual(3)
    }
  })

  it('carries a white chip label at the size a chip label is set', () => {
    // A filled Chip puts white on palette.info at 11px, where AA wants 4.5:1. This is what sent the light
    // idle a step darker than the dark one; without the check it would have shipped at 4.44:1.
    expect(contrast(signalLight.idle, '#ffffff')).toBeGreaterThanOrEqual(4.5)
    expect(contrast(signalDark.idle, '#ffffff')).toBeGreaterThanOrEqual(4.5)
  })
})
