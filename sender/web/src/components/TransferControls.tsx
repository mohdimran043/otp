import { useState } from 'react'
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  IconButton,
  Stack,
  Tooltip,
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
//
// Two presentations, one policy. The detail page has room for labelled buttons and inline notes; a row in
// the transfers list has room for three icons. What must not differ between them is *what stopping means* —
// which queries go stale, what the confirmation says, which statuses can be paused — so that lives in
// useTransferActions and StopDialog below and both presentations are thin.
//
// This matters because the previous arrangement put pause and stop on the detail page only, so acting on a
// transfer meant opening it, while delete sat in the list. The two halves of "manage this transfer" were in
// different places for no reason a user could see.

/** running is whether there is anything to stop. */
export function running(status: string): boolean {
  return ['pending', 'preparing', 'ready', 'transmitting', 'paused'].includes(status)
}

/**
 * useTransferActions owns the three mutations and what they invalidate.
 *
 * The invalidation list is the part worth keeping in one place. A pause changes the transfer, the list it
 * appears in, its chunks, and the display's own settings — miss one and the interface keeps showing a
 * transfer as transmitting after it has stopped.
 */
export function useTransferActions(transmissionId: string) {
  const client = useQueryClient()
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

  return {
    cancel,
    pause,
    resume,
    busy: cancel.isPending || pause.isPending || resume.isPending,
    error: cancel.error ?? pause.error ?? resume.error,
    note,
    clearNote: () => setNote(null),
  }
}

interface StopDialogProps {
  open: boolean
  onClose: () => void
  onStop: () => void
  stopping: boolean
  busy: boolean
  status: string
  ackedChunks: number
  chunkCount: number
}

/** StopDialog is the confirmation, shared so both presentations warn in the same words. */
function StopDialog({
  open,
  onClose,
  onStop,
  stopping,
  busy,
  status,
  ackedChunks,
  chunkCount,
}: StopDialogProps) {
  const percent = chunkCount > 0 ? Math.round((ackedChunks / chunkCount) * 100) : 0

  return (
    <Dialog open={open} onClose={onClose}>
      <DialogTitle>Stop this transfer?</DialogTitle>
      <DialogContent>
        <DialogContentText component="div">
          {ackedChunks} of {chunkCount} chunks have been acknowledged — {percent}% of the file has arrived
          and been confirmed.
          <br />
          <br />
          Stopping cannot be undone: the frames stay rendered, but nothing will display them again. The
          receiver is not told, because there is nothing to tell it — it simply stops seeing frames, which
          is the same event as the sender being switched off.
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
        <Button onClick={onClose}>Keep going</Button>
        <Button color="error" variant="contained" disabled={busy} onClick={onStop}>
          {stopping ? 'Stopping…' : 'Stop the transfer'}
        </Button>
      </DialogActions>
    </Dialog>
  )
}

interface Props {
  transmissionId: string
  status: string
  ackedChunks: number
  chunkCount: number
}

export function TransferControls({ transmissionId, status, ackedChunks, chunkCount }: Props) {
  const [confirming, setConfirming] = useState(false)
  const actions = useTransferActions(transmissionId)

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
            disabled={actions.busy}
            onClick={() => actions.pause.mutate()}
          >
            Pause
          </Button>
        )}
        {status === 'paused' && (
          <Button
            size="small"
            variant="contained"
            startIcon={<PlayArrowIcon />}
            disabled={actions.busy}
            onClick={() => actions.resume.mutate()}
          >
            Resume
          </Button>
        )}
        <Button
          size="small"
          variant="outlined"
          color="error"
          startIcon={<StopIcon />}
          disabled={actions.busy}
          onClick={() => setConfirming(true)}
        >
          Stop
        </Button>
      </Stack>

      <ErrorNotice error={actions.error} />
      {actions.note && (
        <Alert severity="info" variant="outlined" onClose={actions.clearNote}>
          {actions.note}
        </Alert>
      )}

      <StopDialog
        open={confirming}
        onClose={() => setConfirming(false)}
        onStop={() => actions.cancel.mutate(undefined, { onSuccess: () => setConfirming(false) })}
        stopping={actions.cancel.isPending}
        busy={actions.busy}
        status={status}
        ackedChunks={ackedChunks}
        chunkCount={chunkCount}
      />
    </Stack>
  )
}

/**
 * TransferRowControls is the same three actions, sized for a table row.
 *
 * Icons rather than labels because this sits in a cell beside delete, and a row of labelled buttons would
 * dominate the table it is meant to annotate. Every icon carries a tooltip, so nothing depends on the
 * reader already knowing what the glyph means.
 *
 * A failure surfaces on the icon itself rather than as a banner: a row has nowhere to put an alert, and an
 * error that scrolled the table would be worse than one attached to the thing that failed. Notes are
 * dropped here on purpose — "Paused." adds nothing next to a status chip that now reads paused.
 */
export function TransferRowControls({ transmissionId, status, ackedChunks, chunkCount }: Props) {
  const [confirming, setConfirming] = useState(false)
  const actions = useTransferActions(transmissionId)

  if (!running(status)) {
    return null
  }

  const failure = actions.error ? String(actions.error) : null

  return (
    <Stack direction="row" spacing={0} justifyContent="flex-end" sx={{ whiteSpace: 'nowrap' }}>
      {status === 'transmitting' && (
        <Tooltip title={failure ?? 'Pause this transfer'}>
          <IconButton
            size="small"
            color={failure ? 'error' : 'default'}
            disabled={actions.busy}
            aria-label="Pause this transfer"
            onClick={() => actions.pause.mutate()}
          >
            <PauseIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      )}
      {status === 'paused' && (
        <Tooltip title={failure ?? 'Resume this transfer'}>
          <IconButton
            size="small"
            color={failure ? 'error' : 'primary'}
            disabled={actions.busy}
            aria-label="Resume this transfer"
            onClick={() => actions.resume.mutate()}
          >
            <PlayArrowIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      )}
      <Tooltip title={failure ?? 'Stop this transfer'}>
        <IconButton
          size="small"
          color={failure ? 'error' : 'default'}
          disabled={actions.busy}
          aria-label="Stop this transfer"
          onClick={() => setConfirming(true)}
        >
          <StopIcon fontSize="small" />
        </IconButton>
      </Tooltip>

      <StopDialog
        open={confirming}
        onClose={() => setConfirming(false)}
        onStop={() => actions.cancel.mutate(undefined, { onSuccess: () => setConfirming(false) })}
        stopping={actions.cancel.isPending}
        busy={actions.busy}
        status={status}
        ackedChunks={ackedChunks}
        chunkCount={chunkCount}
      />
    </Stack>
  )
}
