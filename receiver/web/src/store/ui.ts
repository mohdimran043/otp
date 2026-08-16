import { create } from 'zustand'
import { persist } from 'zustand/middleware'

// The UI's own state, which is deliberately small.
//
// Everything about the capture lives on the server and is fetched with React Query; putting any of it in a
// client store as well would mean two answers to the same question and a panel showing the stale one.
// What is here is only what the browser knows: the operator's theme and how fast they want to poll.
interface UiState {
  theme: 'dark' | 'light'
  refreshMs: number
  /** Whether the camera page beeps as chunks decode. Persisted, because a mute that forgets itself on reload
   *  is worse than no mute at all — the operator would have to silence it again every time. */
  scanSound: boolean
  /**
   * How the browser encodes each photograph before posting it.
   *
   * Not a cosmetic choice for a colour payload. JPEG subsamples chroma — colour is stored at half
   * resolution in each direction — and colour8 carries its symbols entirely in colour, so a cell at
   * ten pixels has the thing that distinguishes it recorded at five. Measured on a two-lane display
   * across the marginal band, JPEG at quality 92 roughly doubles the fraction of cells left
   * ambiguous: 0.98% against 2.35% at one operating point, 10.4% against 13.2% at the next. Recovery
   * pays below about 3% ambiguity and stops paying above 5%, so that doubling is the difference
   * between a transfer and a stall.
   *
   * JPEG is still the default because a lossless frame is several megabytes rather than several
   * hundred kilobytes, and a link that cannot carry them drops frames — which costs more than the
   * chroma does. Local rigs should prefer png.
   */
  captureFormat: 'jpeg' | 'png'
  setTheme: (theme: 'dark' | 'light') => void
  setRefreshMs: (ms: number) => void
  setScanSound: (on: boolean) => void
  setCaptureFormat: (format: 'jpeg' | 'png') => void
}

export const useUi = create<UiState>()(
  persist(
    (set) => ({
      theme: 'dark',
      // A second is fast enough that a transfer looks live and slow enough that a hundred open tabs
      // would not be the reason a sender is busy.
      refreshMs: 1000,
      // On by default: the sound is the point of the camera page, and an operator holding a camera at a screen
      // cannot see the counters. They can always silence it.
      scanSound: true,
      captureFormat: 'jpeg',
      setTheme: (theme) => set({ theme }),
      setRefreshMs: (refreshMs) => set({ refreshMs }),
      setScanSound: (scanSound) => set({ scanSound }),
      setCaptureFormat: (captureFormat) => set({ captureFormat }),
    }),
    {
      name: 'otp-receiver-ui',
      partialize: (state) => ({
        theme: state.theme,
        refreshMs: state.refreshMs,
        scanSound: state.scanSound,
        captureFormat: state.captureFormat,
      }),
    },
  ),
)
