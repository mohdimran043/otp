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
  setTheme: (theme: 'dark' | 'light') => void
  setRefreshMs: (ms: number) => void
}

export const useUi = create<UiState>()(
  persist(
    (set) => ({
      theme: 'dark',
      // A second is fast enough that a transfer looks live and slow enough that a hundred open tabs
      // would not be the reason a sender is busy.
      refreshMs: 1000,
      setTheme: (theme) => set({ theme }),
      setRefreshMs: (refreshMs) => set({ refreshMs }),
    }),
    {
      name: 'otp-receiver-ui',
      partialize: (state) => ({ theme: state.theme, refreshMs: state.refreshMs }),
    },
  ),
)
