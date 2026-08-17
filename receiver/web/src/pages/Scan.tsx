import { useCallback, useRef, useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  LinearProgress,
  Paper,
  Stack,
  Typography,
} from '@mui/material'
import PhotoCameraIcon from '@mui/icons-material/PhotoCamera'
import UploadFileIcon from '@mui/icons-material/UploadFile'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'

import { api, type ImportSummary } from '../api/client'

// Reading frames off paper, one sheet at a time.
//
// The camera page watches a display; this reads pictures of frames that are not on one. That covers the
// case the printable export exists for — print the sheets, photograph them, feed them in — and the case
// where a transfer was captured on a phone that was not running this receiver.
//
// It is the same pipeline either way. An imported image goes through the same decode, the same lane
// search, the same merge and recovery, and the same acknowledgement path a camera's frame does; nothing
// here is a parallel implementation, which is why a file that imports is a file that would have decoded.
//
// The camera button is the point on a phone. `capture` asks the browser to open the camera rather than the
// file picker, so photographing a sheet is one tap rather than take-photo-then-find-it-in-the-gallery. On a
// desktop the attribute is ignored and it behaves as a second file picker, which is harmless.

/** shot is one file's outcome, kept so a batch shows what happened to each rather than only in total. */
interface shot {
  name: string
  status: 'pending' | 'read' | 'skipped' | 'failed'
  detail: string
}

export function Scan() {
  const navigate = useNavigate()
  const client = useQueryClient()

  const [shots, setShots] = useState<shot[]>([])
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null)
  const [dragging, setDragging] = useState(false)
  const cameraInput = useRef<HTMLInputElement>(null)
  const fileInput = useRef<HTMLInputElement>(null)

  const summarise = (name: string, summary: ImportSummary): shot => {
    const entry = summary.entries?.[0]
    if (!entry) return { name, status: 'skipped', detail: 'nothing in it was a frame' }
    if (entry.error) return { name, status: 'failed', detail: entry.error }
    if (entry.skipped) return { name, status: 'skipped', detail: entry.skipped }
    return {
      name,
      status: 'read',
      detail: entry.transmission_id
        ? `${entry.is_manifest ? 'manifest' : `chunk ${entry.chunk_number ?? '?'}`} of ${entry.transmission_id.slice(0, 8)}`
        : 'read',
    }
  }

  const send = useMutation({
    // One at a time rather than all at once. The receiver applies frames through a single-threaded
    // applier, so firing twenty uploads concurrently would queue behind each other anyway — and doing it
    // in order means the progress bar counts sheets rather than jumping about.
    mutationFn: async (files: File[]) => {
      setProgress({ done: 0, total: files.length })
      const results: shot[] = []
      for (const [i, file] of files.entries()) {
        try {
          const summary = await api.importFrames(file)
          results.push(summarise(file.name, summary))
        } catch (err) {
          results.push({
            name: file.name,
            status: 'failed',
            detail: err instanceof Error ? err.message : String(err),
          })
        }
        setProgress({ done: i + 1, total: files.length })
        setShots([...results].reverse())
      }
      return results
    },
    onSuccess: async (results) => {
      setProgress(null)
      await client.invalidateQueries({ queryKey: ['transmissions'] })
      await client.invalidateQueries({ queryKey: ['recent-frames'] })
      // Only when every sheet landed in one transfer: sending an operator to a transfer page when half
      // the batch went somewhere else would hide the half they need to look at.
      const ids = new Set(
        results.filter((r) => r.status === 'read' && r.detail.includes(' of ')).map((r) => r.detail),
      )
      if (results.length > 0 && results.every((r) => r.status === 'read') && ids.size > 0) {
        await client.invalidateQueries({ queryKey: ['transmissions'] })
      }
    },
  })

  const accept = useCallback(
    (list: FileList | null) => {
      if (!list || list.length === 0) return
      const files = Array.from(list)
      setShots(files.map((f) => ({ name: f.name, status: 'pending' as const, detail: 'waiting' })))
      send.mutate(files)
    },
    [send],
  )

  const read = shots.filter((s) => s.status === 'read').length
  const trouble = shots.filter((s) => s.status === 'skipped' || s.status === 'failed').length

  return (
    <Stack spacing={2}>
      <Typography variant="h5">Scan a sheet</Typography>

      <Alert severity="info" variant="outlined">
        Photograph a printed frame, or upload one you already have. Each sheet goes through the same
        decoding a camera's frame does — the same lane search, the same recovery, the same
        acknowledgement — so anything that reads here would have read off a display. A whole archive
        works too: drop the zip from a transfer's <strong>Download frames</strong>, or the PDF from its{' '}
        <strong>Print as PDF</strong> — the frames are read straight out of it, so a sheet never has to be
        printed at all to check that it would decode.
      </Alert>

      <Paper
        variant="outlined"
        onDragOver={(e) => {
          e.preventDefault()
          setDragging(true)
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(e) => {
          e.preventDefault()
          setDragging(false)
          accept(e.dataTransfer.files)
        }}
        sx={{
          p: 4,
          textAlign: 'center',
          borderStyle: 'dashed',
          borderWidth: 2,
          borderColor: dragging ? 'primary.main' : 'divider',
          bgcolor: dragging ? 'action.hover' : 'transparent',
          transition: 'border-color 120ms, background-color 120ms',
        }}
      >
        <Stack spacing={2} alignItems="center">
          <Typography variant="body2" color="text.secondary">
            Drop sheets here, or
          </Typography>

          <Stack direction="row" spacing={1} flexWrap="wrap" justifyContent="center" useFlexGap>
            <Button
              variant="contained"
              startIcon={<PhotoCameraIcon />}
              onClick={() => cameraInput.current?.click()}
              disabled={send.isPending}
            >
              Photograph a sheet
            </Button>
            <Button
              variant="outlined"
              startIcon={<UploadFileIcon />}
              onClick={() => fileInput.current?.click()}
              disabled={send.isPending}
            >
              Choose files
            </Button>
          </Stack>

          <Typography variant="caption" color="text.secondary">
            PNG, JPEG or GIF — a photograph from a phone is JPEG — or a PDF, or a zip of frames.
          </Typography>
        </Stack>

        {/* Two inputs rather than one. `capture` asks a phone to open the camera directly, which is the
            whole point when the sheet is in the operator's other hand; the plain one is for files that
            already exist and allows several at once. */}
        <input
          ref={cameraInput}
          hidden
          type="file"
          accept="image/*"
          capture="environment"
          onChange={(e) => {
            accept(e.target.files)
            e.target.value = ''
          }}
        />
        <input
          ref={fileInput}
          hidden
          multiple
          type="file"
          accept="image/*,.pdf,application/pdf,.zip,application/zip"
          onChange={(e) => {
            accept(e.target.files)
            e.target.value = ''
          }}
        />
      </Paper>

      {progress && (
        <Box>
          <Stack direction="row" justifyContent="space-between" sx={{ mb: 0.5 }}>
            <Typography variant="caption" color="text.secondary">
              reading sheet {progress.done} of {progress.total}
            </Typography>
            {send.isPending && <CircularProgress size={14} />}
          </Stack>
          <LinearProgress variant="determinate" value={(progress.done / progress.total) * 100} />
        </Box>
      )}

      {shots.length > 0 && (
        <Paper variant="outlined" sx={{ p: 2 }}>
          <Stack direction="row" alignItems="baseline" spacing={1.5} sx={{ mb: 1 }} flexWrap="wrap" useFlexGap>
            <Typography variant="subtitle1" sx={{ flexGrow: 1 }}>
              Sheets read
            </Typography>
            <Chip size="small" color={read > 0 ? 'success' : 'default'} label={`${read} read`} />
            {trouble > 0 && <Chip size="small" color="warning" label={`${trouble} not read`} />}
            <Button size="small" onClick={() => navigate('/transmissions')}>
              Go to transfers
            </Button>
          </Stack>

          <Stack spacing={0.5}>
            {shots.map((s, i) => (
              <Stack
                key={`${s.name}-${i}`}
                direction="row"
                spacing={1.5}
                alignItems="center"
                sx={{ px: 1, py: 0.5, borderRadius: 0.5, border: 1, borderColor: 'divider' }}
              >
                <Chip
                  size="small"
                  color={
                    s.status === 'read' ? 'success' : s.status === 'pending' ? 'default' : 'warning'
                  }
                  label={s.status}
                  sx={{ minWidth: 78 }}
                />
                <Typography variant="body2" sx={{ flexGrow: 1, wordBreak: 'break-all' }}>
                  {s.name}
                </Typography>
                <Typography variant="caption" color="text.secondary">
                  {s.detail}
                </Typography>
              </Stack>
            ))}
          </Stack>

          {trouble > 0 && (
            <Alert severity="warning" variant="outlined" sx={{ mt: 1.5 }}>
              A sheet that is not read is usually one the camera could not resolve: too far, at an angle,
              or lit unevenly. Fill the frame with the sheet, hold it flat, and avoid a light source
              reflecting off the paper — a printed frame has no brightness of its own to compete with it.
            </Alert>
          )}
        </Paper>
      )}
    </Stack>
  )
}
