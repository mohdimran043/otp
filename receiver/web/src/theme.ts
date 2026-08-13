import { createTheme, type Theme } from '@mui/material'

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
export const signal = {
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

const ground = {
  base: '#08090b',
  raised: '#101216',
  edge: '#1e222a',
  text: '#e8eaed',
  dim: '#8b93a1',
}

/**
 * instrument builds the theme.
 *
 * Light mode is deliberately not offered as a peer. The camera pages are used in a dim room facing a
 * bright panel, and a light interface there is actively harmful; keeping one dark theme means every
 * colour decision below can be made once and made properly, rather than twice and hedged.
 */
export function instrument(): Theme {
  return createTheme({
    palette: {
      mode: 'dark',
      primary: { main: signal.lock },
      success: { main: signal.lock },
      warning: { main: signal.adjust },
      error: { main: signal.fault },
      background: { default: ground.base, paper: ground.raised },
      text: { primary: ground.text, secondary: ground.dim },
      divider: ground.edge,
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
            backgroundColor: ground.base,
            // A faint grid, fixed rather than scrolling, so the page reads as something drawn on an
            // instrument's face rather than a document. Low enough contrast to be felt and not seen.
            backgroundImage: `linear-gradient(${ground.edge}55 1px, transparent 1px),
                              linear-gradient(90deg, ${ground.edge}55 1px, transparent 1px)`,
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
            backgroundColor: ground.raised,
            borderColor: ground.edge,
          },
        },
      },
      MuiAppBar: {
        styleOverrides: {
          root: {
            backgroundImage: 'none',
            backgroundColor: `${ground.base}e6`,
            backdropFilter: 'blur(12px)',
            borderBottom: `1px solid ${ground.edge}`,
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
        styleOverrides: { root: { borderRadius: 2, backgroundColor: ground.edge } },
      },
      MuiAlert: { styleOverrides: { root: { borderRadius: 3, fontSize: '0.85rem' } } },
      MuiTooltip: {
        styleOverrides: {
          tooltip: { fontFamily: mono, fontSize: '0.7rem', backgroundColor: ground.edge },
        },
      },
    },
  })
}
