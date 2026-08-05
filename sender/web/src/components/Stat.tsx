import { Box, Paper, Typography } from '@mui/material'
import type { ReactNode } from 'react'

// Stat is the tile every panel is built from.
//
// The value is the largest thing in it and the label the smallest, which is the opposite of how a form is
// laid out and the right way round here: an operator scanning a wall of these is looking for the number
// that has changed, not for the word next to it.
export function Stat({
  label,
  value,
  hint,
  accent,
}: {
  label: string
  value: ReactNode
  hint?: string
  accent?: 'success' | 'warning' | 'error' | 'primary'
}) {
  return (
    <Paper variant="outlined" sx={{ p: 2, height: '100%' }}>
      <Typography variant="caption" color="text.secondary" sx={{ textTransform: 'uppercase', letterSpacing: 0.6 }}>
        {label}
      </Typography>
      <Typography
        variant="h5"
        sx={{ mt: 0.5, fontVariantNumeric: 'tabular-nums', fontWeight: 600 }}
        color={accent ? `${accent}.main` : 'text.primary'}
      >
        {value}
      </Typography>
      {hint && (
        <Box sx={{ mt: 0.5 }}>
          <Typography variant="caption" color="text.secondary">
            {hint}
          </Typography>
        </Box>
      )}
    </Paper>
  )
}
