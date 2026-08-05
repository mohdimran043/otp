import { Alert, AlertTitle } from '@mui/material'

import { ApiError } from '../api/client'

// ErrorNotice distinguishes the two failures an operator can do something about from the one they cannot.
//
// A 4xx came from what they asked for, so the message is the useful part and is shown as-is. A network
// failure means this page cannot reach the sender, which is worth saying plainly rather than showing as a
// mysterious empty panel. Anything else is the sender's own fault and says so.
export function ErrorNotice({ error }: { error: unknown }) {
  if (!error) return null

  if (error instanceof ApiError) {
    const client = error.status >= 400 && error.status < 500
    return (
      <Alert severity={client ? 'warning' : 'error'} sx={{ my: 2 }}>
        <AlertTitle>{client ? 'The sender refused this' : `The sender failed (${error.status})`}</AlertTitle>
        {error.message}
      </Alert>
    )
  }

  return (
    <Alert severity="error" sx={{ my: 2 }}>
      <AlertTitle>Cannot reach the sender</AlertTitle>
      {error instanceof Error ? error.message : String(error)}
    </Alert>
  )
}
