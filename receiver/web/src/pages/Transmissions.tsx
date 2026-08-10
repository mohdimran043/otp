import {
  Alert,
  Button,
  Chip,
  Link,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link as RouterLink, useNavigate } from 'react-router-dom'

import { api, formatBytes, formatPercent, type ImportSummary } from '../api/client'
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

  return (
    <Stack spacing={2}>
      <Stack direction="row" justifyContent="space-between" alignItems="center">
        <Typography variant="h5">Transmissions</Typography>
        <Button component="label" variant="outlined" size="small" disabled={importing.isPending}>
          {importing.isPending ? 'Importing…' : 'Import frames'}
          <input
            hidden
            type="file"
            accept=".zip,.png"
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
    </Stack>
  )
}
