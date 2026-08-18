import {
  Alert,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  IconButton,
  Link,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material'
import DeleteIcon from '@mui/icons-material/Delete'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link as RouterLink, useNavigate } from 'react-router-dom'

import { api, formatBytes, formatPercent, type ImportSummary, type TransmissionView } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { useUi } from '../store/ui'

export function Transmissions() {
  const refreshMs = useUi((state) => state.refreshMs)
  const client = useQueryClient()
  const navigate = useNavigate()
  const transmissions = useQuery({
    queryKey: ['transmissions'],
    queryFn: api.transmissions,
    refetchInterval: refreshMs,
  })

  // The last import's outcome, kept only in this component: it is worth showing once, not worth
  // persisting anywhere a reload would need to reproduce it.
  const [importResult, setImportResult] = useState<ImportSummary | null>(null)

  const importing = useMutation({
    mutationFn: (file: File) => api.importFrames(file),
    onSuccess: async (summary) => {
      setImportResult(summary)
      await client.invalidateQueries({ queryKey: ['transmissions'] })
      // Only when the archive touched exactly one transfer: an import spanning several leaves
      // the operator on this page to pick one, rather than guessing which one they meant.
      if (summary.transmissions.length === 1) {
        navigate(`/transmissions/${summary.transmissions[0]}`)
      }
    },
  })

  const problemEntries = (importResult?.entries ?? []).filter((e) => e.skipped || e.error)

  // Deleting from the list, behind the same confirmation the detail page uses.
  //
  // It was previously reachable only by opening a transmission, which made clearing out a few failed
  // captures a matter of navigating in and back out for each one. The dialog is owned here rather than by
  // the row so there is one of it rather than one per transmission.
  const [pendingDelete, setPendingDelete] = useState<TransmissionView | null>(null)

  const deleteTransmission = useMutation({
    mutationFn: (id: string) => api.deleteTransmission(id),
    onSuccess: async () => {
      setPendingDelete(null)
      await client.invalidateQueries({ queryKey: ['transmissions'] })
    },
  })

  return (
    <Stack spacing={2}>
      <Stack direction="row" justifyContent="space-between" alignItems="center">
        <Typography variant="h5">Transmissions</Typography>
        <Button component="label" variant="outlined" size="small" disabled={importing.isPending}>
          {importing.isPending ? 'Importing…' : 'Import frames'}
          {/* The list has to track what the import endpoint actually reads, and it had stopped: the
              server grew JPEG, GIF and PDF while this stayed on .zip and .png, so a printed sheet
              photographed on a phone and the sender's own printable export were both greyed out in
              the file dialog by a receiver perfectly able to read them. accept= is advisory — it
              filters the picker and nothing else — which is exactly why it goes wrong quietly. */}
          {/* Deliberately one file, not `multiple`: this handler submits files[0], and a zip or a PDF
              is already a batch. Selecting a folder of photographed sheets is the Scan page's job,
              which posts them one at a time and reports each. */}
          <input
            hidden
            type="file"
            accept="image/*,.pdf,application/pdf,.zip,application/zip"
            onChange={(e) => {
              const f = e.target.files?.[0]
              if (f) importing.mutate(f)
              e.target.value = ''
            }}
          />
        </Button>
      </Stack>
      <ErrorNotice error={transmissions.error} />
      <ErrorNotice error={importing.error} />

      {importResult && (
        <Alert
          severity={problemEntries.length > 0 || importResult.truncated ? 'warning' : 'success'}
          onClose={() => setImportResult(null)}
        >
          Imported {importResult.ingested} frame{importResult.ingested === 1 ? '' : 's'}
          {importResult.skipped > 0 && `, skipped ${importResult.skipped}`}
          {importResult.truncated && ' — the import stopped early'}
          {importResult.transmissions.length > 1 &&
            ` across ${importResult.transmissions.length} transmissions`}
          .
          {problemEntries.length > 0 && (
            <>
              {' '}
              {problemEntries
                .slice(0, 5)
                .map((e) => `${e.name}: ${e.skipped ?? e.error}`)
                .join('; ')}
              {problemEntries.length > 5 && `; and ${problemEntries.length - 5} more`}
            </>
          )}
        </Alert>
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>File</TableCell>
              <TableCell align="right">Size</TableCell>
              <TableCell sx={{ width: 160 }}>Chunks</TableCell>
              <TableCell align="right">Outstanding</TableCell>
              <TableCell align="right">From parity</TableCell>
              <TableCell>Verified</TableCell>
              <TableCell align="right" sx={{ width: 48 }} />
            </TableRow>
          </TableHead>
          <TableBody>
            {(transmissions.data ?? []).map((transmission) => (
              <TableRow key={transmission.transmission_id} hover>
                <TableCell>
                  <Link
                    component={RouterLink}
                    to={`/transmissions/${transmission.transmission_id}`}
                    underline="hover"
                  >
                    {transmission.filename}
                  </Link>
                </TableCell>
                <TableCell align="right">{formatBytes(transmission.original_size)}</TableCell>
                <TableCell>
                  {transmission.chunks_arrived} / {transmission.chunk_count} (
                  {formatPercent(transmission.progress)})
                </TableCell>
                <TableCell align="right">{transmission.missing_count}</TableCell>
                <TableCell align="right">{transmission.chunks_recovered}</TableCell>
                <TableCell>
                  {transmission.merged ? (
                    <Chip
                      size="small"
                      label={transmission.merged.verified ? 'verified' : 'failed'}
                      color={transmission.merged.verified ? 'success' : 'error'}
                    />
                  ) : (
                    <Chip size="small" label="in progress" />
                  )}
                </TableCell>
                <TableCell align="right" sx={{ py: 0.25 }}>
                  <Tooltip title="Delete this transmission">
                    <IconButton
                      size="small"
                      aria-label="Delete this transmission"
                      onClick={() => setPendingDelete(transmission)}
                    >
                      <DeleteIcon fontSize="small" />
                    </IconButton>
                  </Tooltip>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {(transmissions.data ?? []).length === 0 && (
          <Typography variant="body2" color="text.secondary">
            Nothing has arrived yet.
          </Typography>
        )}
      </Paper>

      {/* The same warning as the detail page, including the part that only applies to a transmission still
          arriving: deleting one does not stop the sender, so the next chunk that decodes starts a fresh row
          from nothing. That is the sentence that stops someone deleting a transfer they meant to keep. */}
      <Dialog open={pendingDelete !== null} onClose={() => setPendingDelete(null)}>
        <DialogTitle>Delete this transmission?</DialogTitle>
        <DialogContent>
          <ErrorNotice error={deleteTransmission.error} />
          <DialogContentText component="div">
            This removes {pendingDelete?.filename} entirely — its manifest, every chunk received for it, the
            merged file, and the acknowledgements written back to the sender. There is no undo, and the file
            cannot be downloaded again afterwards.
            {pendingDelete && !pendingDelete.merged && (
              <>
                <br />
                <br />
                Nothing has been merged yet, so frames for this transmission may still be arriving. Deleting
                it does not stop the sender: the next chunk that decodes will simply start a fresh row from
                nothing
                {/* Only when something would actually be lost. At zero the clause read "having lost the 0
                    chunks already here", which is both awkward and the opposite of the reassurance it
                    should give — there is nothing to lose yet, and that is worth saying plainly. */}
                {pendingDelete.chunks_arrived > 0 ? (
                  <>
                    , having lost the {pendingDelete.chunks_arrived} chunk
                    {pendingDelete.chunks_arrived === 1 ? '' : 's'} already here.
                  </>
                ) : (
                  <> — no chunk has arrived yet, so nothing is lost by deleting it.</>
                )}
              </>
            )}
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setPendingDelete(null)}>Keep it</Button>
          <Button
            color="error"
            variant="contained"
            disabled={deleteTransmission.isPending}
            onClick={() => pendingDelete && deleteTransmission.mutate(pendingDelete.transmission_id)}
          >
            {deleteTransmission.isPending ? 'Deleting…' : 'Delete the transmission'}
          </Button>
        </DialogActions>
      </Dialog>
    </Stack>
  )
}
