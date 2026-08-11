import { useState } from 'react'
import {
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  MenuItem,
  Paper,
  Stack,
  TextField,
  Typography,
} from '@mui/material'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { api, type Transmission } from '../api/client'
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
  const client = useQueryClient()
  const refreshMs = useUi((state) => state.refreshMs)
  const [filter, setFilter] = useState('')
  const [pendingDelete, setPendingDelete] = useState<Transmission | null>(null)

  const transfers = useQuery({
    queryKey: ['transfers', filter],
    queryFn: () => api.transfers(filter || undefined),
    refetchInterval: refreshMs,
  })

  const deleteTransfer = useMutation({
    mutationFn: (id: string) => api.deleteTransfer(id),
    onSuccess: () => {
      setPendingDelete(null)
      void client.invalidateQueries({ queryKey: ['transfers'] })
    },
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
          <TransferTable transfers={transfers.data ?? []} onDelete={setPendingDelete} />
        )}
      </Paper>

      <Dialog open={pendingDelete !== null} onClose={() => setPendingDelete(null)}>
        <DialogTitle>Delete this transfer?</DialogTitle>
        <DialogContent>
          <ErrorNotice error={deleteTransfer.error} />
          <DialogContentText component="div">
            This removes {pendingDelete?.id.slice(0, 8)} entirely — the row, its chunks, and every
            frame the pipeline wrote for it. There is no undo.
            <br />
            <br />
            {pendingDelete && ['preparing', 'transmitting', 'paused'].includes(pendingDelete.status) ? (
              <>
                It is currently <strong>{pendingDelete.status}</strong>, so the server will refuse this
                until it is cancelled first.
              </>
            ) : (
              'It is safe to delete: nothing is actively using it right now.'
            )}
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setPendingDelete(null)}>Keep it</Button>
          <Button
            color="error"
            variant="contained"
            disabled={deleteTransfer.isPending}
            onClick={() => pendingDelete && deleteTransfer.mutate(pendingDelete.id)}
          >
            {deleteTransfer.isPending ? 'Deleting…' : 'Delete the transfer'}
          </Button>
        </DialogActions>
      </Dialog>
    </Stack>
  )
}
