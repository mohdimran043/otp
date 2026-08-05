import { useState } from 'react'
import { MenuItem, Paper, Stack, TextField, Typography } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { TransferTable } from '../components/TransferTable'
import { useUi } from '../store/ui'

const filters = [
  { value: '', label: 'Everything' },
  { value: 'transmitting', label: 'Transmitting' },
  { value: 'preparing,ready', label: 'Preparing' },
  { value: 'completed', label: 'Completed' },
  { value: 'failed,cancelled', label: 'Failed or cancelled' },
]

export function Transfers() {
  const refreshMs = useUi((state) => state.refreshMs)
  const [filter, setFilter] = useState('')

  const transfers = useQuery({
    queryKey: ['transfers', filter],
    queryFn: () => api.transfers(filter || undefined),
    refetchInterval: refreshMs,
  })

  return (
    <Stack spacing={2}>
      <Stack direction="row" alignItems="center" justifyContent="space-between">
        <Typography variant="h5">Transfers</Typography>
        <TextField
          select
          size="small"
          label="Show"
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
          sx={{ minWidth: 220 }}
        >
          {filters.map((option) => (
            <MenuItem key={option.value} value={option.value}>
              {option.label}
            </MenuItem>
          ))}
        </TextField>
      </Stack>

      <ErrorNotice error={transfers.error} />

      <Paper variant="outlined" sx={{ p: 2 }}>
        {(transfers.data ?? []).length === 0 ? (
          <Typography variant="body2" color="text.secondary">
            Nothing matches that filter.
          </Typography>
        ) : (
          <TransferTable transfers={transfers.data ?? []} />
        )}
      </Paper>
    </Stack>
  )
}
