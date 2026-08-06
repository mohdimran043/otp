import { useState } from 'react'
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  Stack,
} from '@mui/material'
import PauseIcon from '@mui/icons-material/Pause'
import PlayArrowIcon from '@mui/icons-material/PlayArrow'
import StopIcon from '@mui/icons-material/Stop'
import { useMutation, useQueryClient } from '@tanstack/react-query'

import { api } from '../api/client'
import { ErrorNotice } from './ErrorNotice'

// Stopping a transfer.
//
// Cancelling is behind a confirmation and pausing is not, and the asymmetry is the point: a pause is
// undoable and a cancel is not. A transfer that has been running for an hour is an hour of display time,
// and the sender cannot resume it — the chunks are still rendered, but nothing will show them again.
//
// The dialog says how far it got, because that is the number that decides whether cancelling is the right
// call. Ninety per cent acknowledged is worth finishing; five per cent is not.

interface Props {
  transmissionId: string
  status: string
  ackedChunks: number
  chunkCount: number
}

/** running is whether there is anything to stop. */
function running(status: string): boolean {
  return ['pending', 'preparing', 'ready', 'transmitting', 'paused'].includes(status)
}

export function TransferControls({ transmissionId, status, ackedChunks, chunkCount }: Props) {
  const client = useQueryClient()
  const [confirming, setConfirming] = useState(false)
  const [note, setNote] = useState<string | null>(null)

  const refresh = () => {
    void client.invalidateQueries({ queryKey: ['transfer', transmissionId] })
    void client.invalidateQueries({ queryKey: ['transfers'] })
    void client.invalidateQueries({ queryKey: ['chunks', transmissionId] })
    void client.invalidateQueries({ queryKey: ['settings'] })
  }

  const cancel = useMutation({
    mutationFn: () => api.cancel(transmissionId),
    onSuccess: (result) => {
      setConfirming(false)
      setNote(result.note ?? 'Cancelled.')
      refresh()
    },
  })

  const pause = useMutation({
    mutationFn: () => api.pause(transmissionId),
    onSuccess: (result) => {
      setNote(result.note ?? 'Paused.')
      refresh()
    },
  })

  const resume = useMutation({
    mutationFn: () => api.resume(transmissionId),
    onSuccess: (result) => {
      setNote(result.note ?? 'Resumed.')
      refresh()
    },
  })

  const busy = cancel.isPending || pause.isPending || resume.isPending
  const percent = chunkCount > 0 ? Math.round((ackedChunks / chunkCount) * 100) : 0

  if (!running(status)) {
    return null
  }

  return (
    <Stack spacing={1}>
      <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
        {status === 'transmitting' && (
          <Button
            size="small"
            variant="outlined"
            startIcon={<PauseIcon />}
            disabled={busy}
            onClick={() => pause.mutate()}
          >
            Pause
          </Button>
        )}
        {status === 'paused' && (
          <Button
            size="small"
            variant="contained"
            startIcon={<PlayArrowIcon />}
            disabled={busy}
            onClick={() => resume.mutate()}
          >
            Resume
          </Button>
        )}
        <Button
          size="small"
          variant="outlined"
          color="error"
          startIcon={<StopIcon />}
          disabled={busy}
          onClick={() => setConfirming(true)}
        >
          Stop
        </Button>
      </Stack>

      <ErrorNotice error={cancel.error ?? pause.error ?? resume.error} />
      {note && (
        <Alert severity="info" variant="outlined" onClose={() => setNote(null)}>
          {note}
        </Alert>
      )}

      <Dialog open={confirming} onClose={() => setConfirming(false)}>
        <DialogTitle>Stop this transfer?</DialogTitle>
        <DialogContent>
          <DialogContentText component="div">
            {ackedChunks} of {chunkCount} chunks have been acknowledged — {percent}% of the file has
            arrived and been confirmed.
            <br />
            <br />
            Stopping cannot be undone: the frames stay rendered, but nothing will display them again. The
            receiver is not told, because there is nothing to tell it — it simply stops seeing frames,
            which is the same event as the sender being switched off.
            {status === 'transmitting' && (
              <>
                <br />
                <br />
                To stop for now and carry on later, <strong>Pause</strong> instead. Acknowledged chunks are
                kept, so resuming shows only what is still outstanding.
              </>
            )}
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirming(false)}>Keep going</Button>
          <Button color="error" variant="contained" disabled={busy} onClick={() => cancel.mutate()}>
            {cancel.isPending ? 'Stopping…' : 'Stop the transfer'}
          </Button>
        </DialogActions>
      </Dialog>
    </Stack>
  )
}
