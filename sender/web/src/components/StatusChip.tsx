import { Chip } from '@mui/material'

// The colours are a judgement about what needs attention rather than decoration.
//
// Transmitting is the ordinary state and is deliberately unobtrusive. Failed is the only red, because it
// is the only state that will not resolve itself. Paused is amber rather than red: somebody chose it.
const colours: Record<string, 'default' | 'primary' | 'success' | 'warning' | 'error' | 'info'> = {
  pending: 'default',
  preparing: 'info',
  ready: 'info',
  transmitting: 'primary',
  paused: 'warning',
  completed: 'success',
  failed: 'error',
  cancelled: 'default',
}

export function StatusChip({ status }: { status: string }) {
  return <Chip size="small" label={status} color={colours[status] ?? 'default'} variant="filled" />
}
