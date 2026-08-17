import { createTheme, useTheme as useMuiTheme, type Theme } from '@mui/material'

// The look: a signal instrument, not a dashboard.
//
// This application watches light cross an air gap and reports whether it arrived. That is the work of
// a measurement instrument — a spectrum analyser, a scope, a bench meter — and the interface people
// actually trust for that work looks nothing like a web dashboard. It is dark because the operator is
// looking at a bright panel through a camera and a pale interface beside it ruins their dark
// adaptation. It is dense because every figure here is evidence and hiding evidence behind whitespace
// has cost this project real debugging time. And it is monospaced wherever a number appears, because
// numbers that shift under their own digits cannot be read while they change, which is exactly when
// these numbers matter.
//
// Two faces, both bundled rather than fetched: this runs air-gapped by design, so a stylesheet from a
// CDN would be a font that silently fails to load in the one deployment that matters.
//
//   Martian Mono — every measurement, every identifier, every state. Engineered rather than
//     nostalgic: it reads as instrumentation, where a typewriter face would read as costume.
//   Archivo      — prose. A grotesque with enough width range to set a dense caption and a page
//     title from one family without either looking borrowed.

export const mono = '"Martian Mono", ui-monospace, SFMono-Regular, Menlo, monospace'
export const sans = '"Archivo Variable", "Archivo", system-ui, sans-serif'

// The palette is one dark ground and a small set of signals. Restraint is the point: when almost
// nothing is coloured, a colour means something, and the operator can find the one thing that
// changed without reading. Six accents would make a page that is merely decorated.
export const signalDark = {
  /** Locked. The frame decoded — the only unambiguously good state this system has. */
  lock: '#4ade80',
  /** Adjust. Something is recoverable by moving, and the operator is being asked to move it. */
  adjust: '#fbbf24',
  /** Marginal. Found but not read: one small movement from working, and the state that most needs saying. */
  marginal: '#fde047',
  /** Fault. Nothing to act on optically — a configuration or a service is wrong. */
  fault: '#f87171',
  /** Idle. Searching, waiting, nothing to report. Deliberately quiet. */
  idle: '#64748b',
} as const

// The same signals, re-tuned for a light ground.
//
// Not the dark values on a white page: a colour chosen to glow against #08090b is washed out against #fff,
// and #fde047 — legible as a warning on black — is very nearly invisible. Each one is the same hue at the
// weight that carries on paper-white, so "amber means move" reads identically in either theme.
export const signalLight = {
  lock: '#15803d',
  adjust: '#b45309',
  marginal: '#a16207',
  fault: '#b91c1c',
  // The one signal that is not simply the dark value re-weighted: idle has to work as text *and* as the
  // ground of a filled chip, and those pull opposite ways. #64748b carries white at 4.76:1 — fine on a
  // chip — but sits at 4.44:1 as text on this ground, under the 4.5 an 11px label needs. This is the
  // same slate a step darker, which clears both (5.98 and 5.58) and happens to be exactly the dim text
  // colour below, so "quiet" and "nothing to report" are literally the same colour.
  idle: '#5b6472',
} as const

/**
 * signal is the dark set, and remains the default export for anything outside React.
 *
 * Components should use useSignal() instead, which follows the theme. This is here for the places that
 * cannot — a module-level constant, a canvas draw — and for the camera view, which stays dark whatever the
 * rest of the interface is doing.
 */
export const signal = signalDark

const groundLight = {
  base: '#f6f7f9',
  raised: '#ffffff',
  edge: '#dde1e7',
  text: '#14171c',
  dim: '#5b6472',
}

const ground = {
  base: '#08090b',
  raised: '#101216',
  edge: '#1e222a',
  text: '#e8eaed',
  dim: '#8b93a1',
}

/**
 * onPanel is text drawn on a black surface, whichever theme is in force.
 *
 * A few surfaces here are black in both themes and always will be: the display's own panel, the camera
 * preview, and the mat behind a captured photograph. They are black because of what they hold, not because
 * of the interface around them — a white mat behind a frame being photographed puts light into the shot.
 *
 * Text on those surfaces therefore cannot use `text.secondary`, which follows the theme: on the light one
 * it is a slate chosen for a white page and lands at 3.51:1 against black, under the 4.5 it needs. This is
 * the dark theme's dim, which is what those surfaces were always designed against.
 */
export const onPanel = ground.dim

/**
 * instrument builds the theme, in either ground.
 *
 * Dark remains the default, and the reason it was once the only option still holds: these pages share a
 * room with a bright panel being photographed, and a white interface beside it spills light into the shot.
 * But that argument is about the room, not the person — it says nothing to an operator setting up a
 * transfer at a desk in daylight, and nothing at all to one who cannot comfortably read light-on-dark.
 * So both grounds exist and neither is a tint of the other: every signal colour is re-chosen for its
 * ground rather than reused across both, because a hue tuned to glow on #08090b is washed out on white.
 *
 * Both branches read `g` and `sig`. That is load-bearing — the earlier version selected them and then
 * built the component overrides from the dark constants regardless, so choosing light produced light
 * *text* on dark panels. If a `ground.` or `signalDark` appears below without a mode test beside it,
 * that is the same bug returning.
 */
export function instrument(mode: 'dark' | 'light' = 'dark'): Theme {
  const g = mode === 'light' ? groundLight : ground
  const sig = mode === 'light' ? signalLight : signalDark

  return createTheme({
    palette: {
      mode,
      primary: { main: sig.lock },
      success: { main: sig.lock },
      warning: { main: sig.adjust },
      error: { main: sig.fault },
      // Defined rather than left to MUI, which was the one colour on these pages from outside the
      // palette: a status chip reading "ready" came out in the library's default blue, both louder than
      // anything it sat beside and, at 3.86:1 under its own white label, the least legible thing on the
      // page. Idle is what "ready" and "preparing" mean here — waiting, nothing to report.
      info: { main: sig.idle },
      background: { default: g.base, paper: g.raised },
      text: { primary: g.text, secondary: g.dim },
      divider: g.edge,
    },

    shape: { borderRadius: 4 },

    typography: {
      fontFamily: sans,
      // Tight and wide: a title set in a grotesque at normal tracking looks like every other page.
      // Negative tracking on the display sizes and positive on the small caps is what makes the
      // hierarchy read as designed rather than as defaults.
      h4: { fontFamily: mono, fontWeight: 600, letterSpacing: '-0.04em', fontSize: '1.5rem' },
      h5: { fontFamily: mono, fontWeight: 600, letterSpacing: '-0.03em', fontSize: '1.15rem' },
      h6: { fontFamily: mono, fontWeight: 600, letterSpacing: '-0.02em', fontSize: '0.95rem' },
      subtitle1: { fontFamily: mono, fontWeight: 500, fontSize: '0.8rem', letterSpacing: '0.02em' },
      body2: { fontSize: '0.875rem', lineHeight: 1.55 },
      caption: {
        fontFamily: mono,
        fontSize: '0.68rem',
        letterSpacing: '0.08em',
        textTransform: 'uppercase',
      },
      button: { fontFamily: mono, fontWeight: 600, letterSpacing: '0.04em', textTransform: 'none' },
    },

    components: {
      MuiCssBaseline: {
        styleOverrides: {
          body: {
            backgroundColor: g.base,
            // A faint grid, fixed rather than scrolling, so the page reads as something drawn on an
            // instrument's face rather than a document. Low enough contrast to be felt and not seen.
            backgroundImage: `linear-gradient(${g.edge}55 1px, transparent 1px),
                              linear-gradient(90deg, ${g.edge}55 1px, transparent 1px)`,
            backgroundSize: '48px 48px',
            backgroundAttachment: 'fixed',
          },
          // Tabular figures everywhere by default. A readout whose digits change width jitters as it
          // updates, and every number on these pages updates several times a second.
          'code, pre, .tabular': { fontFamily: mono, fontVariantNumeric: 'tabular-nums' },
        },
      },
      MuiPaper: {
        defaultProps: { variant: 'outlined' },
        styleOverrides: {
          root: {
            backgroundImage: 'none',
            backgroundColor: g.raised,
            borderColor: g.edge,
          },
        },
      },
      MuiAppBar: {
        styleOverrides: {
          root: {
            backgroundImage: 'none',
            backgroundColor: `${g.base}e6`,
            backdropFilter: 'blur(12px)',
            borderBottom: `1px solid ${g.edge}`,
          },
        },
      },
      MuiTab: {
        styleOverrides: {
          root: {
            fontFamily: mono,
            fontSize: '0.72rem',
            letterSpacing: '0.1em',
            textTransform: 'uppercase',
            minHeight: 52,
          },
        },
      },
      MuiChip: {
        styleOverrides: {
          root: {
            fontFamily: mono,
            fontSize: '0.7rem',
            letterSpacing: '0.02em',
            fontVariantNumeric: 'tabular-nums',
            borderRadius: 3,
          },
        },
      },
      MuiButton: { styleOverrides: { root: { borderRadius: 3 } } },
      MuiLinearProgress: {
        styleOverrides: { root: { borderRadius: 2, backgroundColor: g.edge } },
      },
      MuiAlert: { styleOverrides: { root: { borderRadius: 3, fontSize: '0.85rem' } } },
      MuiTooltip: {
        styleOverrides: {
          tooltip: {
            fontFamily: mono,
            fontSize: '0.7rem',
            // Tooltips invert: a light-ground tooltip under MUI's white label is an unreadable
            // pale box, so the light theme keeps the dark chip that every other application uses.
            backgroundColor: mode === 'light' ? ground.edge : g.edge,
          },
        },
      },
    },
  })
}

/**
 * useSignal is the signal palette for whichever theme is in force.
 *
 * A hook rather than a mode-aware constant, because a constant read during render is not reactive: it would
 * hold whatever the theme was when the module loaded, and a page toggled to light would keep drawing its
 * outlines in the dark set until something else happened to re-render it.
 *
 * The values stay plain hex strings, which matters more than it looks — several callers build a translucent
 * fill by appending an alpha pair (`${colour}12`), and a CSS custom property would make that `var(--x)12`
 * and silently draw nothing.
 */
export function useSignal(): Record<keyof typeof signalDark, string> {
  return useMuiTheme().palette.mode === 'light' ? signalLight : signalDark
}
