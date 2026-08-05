import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import { App } from './App'

// Retries are off for queries.
//
// Every panel here polls on an interval, so a failed request is followed by another one a second later
// anyway — retrying would only mean three requests where one was enough, and an error that flickers
// instead of being shown. Mutations do not retry either: an upload that failed halfway should be
// resubmitted by the operator, not silently repeated behind their back.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: false, refetchOnWindowFocus: true, staleTime: 500 },
    mutations: { retry: false },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
