import { create } from 'zustand'
import { persist } from 'zustand/middleware'

// The UI's own state, which is deliberately small.
//
// Everything about transfers lives on the server and is fetched with React Query; putting any of it in a
// client store as well would mean two answers to the same question and a UI that shows a stale one. What
// is here is what only the browser knows: the operator's theme, how fast they want the panels to poll,
// and the profile they last chose for an upload.
interface UiState {
  theme: 'dark' | 'light'
  refreshMs: number
  lastProfile: {
    encoder: string
    compression: string
    fecCodec: string
    callbackUrl: string
  }
  setTheme: (theme: 'dark' | 'light') => void
  setRefreshMs: (ms: number) => void
  rememberProfile: (profile: Partial<UiState['lastProfile']>) => void
}

export const useUi = create<UiState>()(
  persist(
    (set) => ({
      theme: 'dark',
      // A second is fast enough that a transfer looks live and slow enough that a hundred open tabs
      // would not be the reason a sender is busy.
      refreshMs: 1000,
      lastProfile: { encoder: '', compression: '', fecCodec: '', callbackUrl: '' },
      setTheme: (theme) => set({ theme }),
      setRefreshMs: (refreshMs) => set({ refreshMs }),
      rememberProfile: (profile) =>
        set((state) => ({ lastProfile: { ...state.lastProfile, ...profile } })),
    }),
    {
      name: 'otp-sender-ui',
      // The callback URL is remembered because an operator sending a series of files sends them to the
      // same place, and retyping it every time is where mistakes come from.
      partialize: (state) => ({
        theme: state.theme,
        refreshMs: state.refreshMs,
        lastProfile: state.lastProfile,
      }),
    },
  ),
)
