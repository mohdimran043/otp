import { useState } from 'react'
import {
  Alert,
  Button,
  IconButton,
  InputAdornment,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableRow,
  TextField,
  Typography,
} from '@mui/material'
import DeleteIcon from '@mui/icons-material/Delete'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { api, formatPercent } from '../api/client'
import { ErrorNotice } from '../components/ErrorNotice'
import { Grid } from '../components/Grid'
import { Stat } from '../components/Stat'
import { useUi } from '../store/ui'

function randomKeyHex(): string {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

// What the decoder is doing, and which of it can be changed without a restart.
//
// The confidence floors are the interesting pair: they are the receiver's own policy rather than the
// protocol's, and they are reloadable precisely because tuning a marginal camera means trying a threshold
// and watching what happens.
//
// Choosing the camera itself is not here — it moved to its own tab, where opening the page is what asks the
// browser for permission. This page is for reading numbers and loading keys, and an operator who came to do
// either should not be answering a hardware prompt on the way.
export function Settings() {
  const { refreshMs, setRefreshMs } = useUi()
  const client = useQueryClient()
  const config = useQuery({ queryKey: ['config'], queryFn: api.config })
  const keys = useQuery({ queryKey: ['keys'], queryFn: api.keys })
  const data = config.data

  const [keyLabel, setKeyLabel] = useState('')
  const [keyHex, setKeyHex] = useState('')
  const keyValid = /^[0-9a-fA-F]{64}$/.test(keyHex.trim())

  const addKey = useMutation({
    mutationFn: () => api.addKey(keyHex.trim(), keyLabel.trim()),
    onSuccess: async () => {
      setKeyLabel('')
      setKeyHex('')
      await client.invalidateQueries({ queryKey: ['keys'] })
    },
  })
  const deleteKey = useMutation({
    mutationFn: (id: number) => api.deleteKey(id),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: ['keys'] })
    },
  })

  // What the "Decryption keys" stat counts: every key loaded through this page, plus one more when
  // the environment carries a configured key — the same key OpenFrame tries first, just not one
  // this page can list or remove.
  const loadedKeyCount = (keys.data?.length ?? 0) + (data?.decoder.encrypted ? 1 : 0)

  return (
    <Stack spacing={3}>
      <Typography variant="h5">Decoder</Typography>
      <ErrorNotice error={config.error} />

      {data && (
        <Grid container spacing={2}>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Protocol version" value={data.protocol_version} />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat label="Capture source" value={data.capture.source} hint={data.capture.dir} />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Decoding at once"
              value={`${data.capture.decode_workers_now} frames`}
              hint={
                data.capture.decode_workers > 0
                  ? 'configured'
                  : 'one per core, less one — set OTP_RECEIVER_CAPTURE_DECODE_WORKERS to override'
              }
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Deepest backlog"
              value={`${data.capture.frames_behind.toLocaleString()} frames`}
              hint="1 means it kept up; a large number means the display is ahead of the decoder"
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Fiducial floor"
              value={formatPercent(data.decoder.min_finder_score)}
              hint="frames below this are discarded unread"
            />
          </Grid>
          <Grid size={{ xs: 12, sm: 6, md: 3 }}>
            <Stat
              label="Decryption keys"
              value={`${loadedKeyCount} loaded`}
              hint={
                loadedKeyCount === 0
                  ? 'payloads arrive in the clear'
                  : 'the sender chooses a key per transfer, tried in order until one opens the frame'
              }
            />
          </Grid>
        </Grid>
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          Decryption keys
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
          The sender chooses a key per transfer and carries it here out of band — this receiver
          cannot know which transfer the display will show next, so every key loaded below is tried
          until one opens the frame. A key is never shown again once it is loaded; only its
          fingerprint is.
        </Typography>
        <ErrorNotice error={keys.error} />

        {(keys.data?.length ?? 0) === 0 ? (
          <Alert severity="info" variant="outlined" sx={{ mb: 2 }}>
            No keys loaded. {data?.decoder.encrypted ? 'The configured key is still tried first.' : ''}
          </Alert>
        ) : (
          <Table size="small" sx={{ mb: 2 }}>
            <TableBody>
              {keys.data!.map((k) => (
                <TableRow key={k.id}>
                  <TableCell>{k.label || '(no label)'}</TableCell>
                  <TableCell sx={{ fontFamily: 'monospace' }}>{k.fingerprint}</TableCell>
                  <TableCell align="right">
                    <IconButton
                      size="small"
                      aria-label={`delete ${k.label || k.fingerprint}`}
                      disabled={deleteKey.isPending}
                      onClick={() => deleteKey.mutate(k.id)}
                    >
                      <DeleteIcon fontSize="small" />
                    </IconButton>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}

        <ErrorNotice error={addKey.error} />
        <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} alignItems="flex-start">
          <TextField
            label="Label"
            value={keyLabel}
            onChange={(event) => setKeyLabel(event.target.value)}
            size="small"
          />
          <TextField
            label="Key (64 hex characters)"
            value={keyHex}
            onChange={(event) => setKeyHex(event.target.value)}
            error={keyHex.length > 0 && !keyValid}
            size="small"
            sx={{ minWidth: 320 }}
            slotProps={{
              input: {
                sx: { fontFamily: 'monospace' },
                endAdornment: (
                  <InputAdornment position="end">
                    <Button size="small" onClick={() => setKeyHex(randomKeyHex())}>
                      Generate
                    </Button>
                  </InputAdornment>
                ),
              },
            }}
          />
          <Button
            variant="contained"
            disabled={!keyValid || addKey.isPending}
            onClick={() => addKey.mutate()}
          >
            Add key
          </Button>
        </Stack>
      </Paper>

      {data && (
        <Paper variant="outlined" sx={{ p: 2 }}>
          <Typography variant="subtitle1" sx={{ mb: 1 }}>
            Callback delivery
          </Typography>
          {data.callback.allow_any_host ? (
            <Alert severity="warning" variant="outlined">
              This receiver will deliver a merged file to any host the sender names. The URL crosses the
              optical channel from outside this machine, so an allowlist is the only thing that stops it
              being used to reach somewhere it should not.
            </Alert>
          ) : (data.callback.allowed_hosts ?? []).length === 0 ? (
            <Alert severity="info" variant="outlined">
              No hosts are allowed, so no merged file will be delivered anywhere. Files are still received,
              verified, and downloadable from here.
            </Alert>
          ) : (
            <Table size="small">
              <TableBody>
                {(data.callback.allowed_hosts ?? []).map((host) => (
                  <TableRow key={host}>
                    <TableCell>{host}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </Paper>
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle1" sx={{ mb: 1 }}>
          This browser
        </Typography>
        <Table size="small">
          <TableBody>
            <TableRow>
              <TableCell sx={{ width: 200 }}>Refresh interval</TableCell>
              <TableCell>
                {[500, 1000, 2000, 5000].map((ms) => (
                  <Typography
                    key={ms}
                    component="span"
                    variant="body2"
                    onClick={() => setRefreshMs(ms)}
                    sx={{
                      mr: 2,
                      cursor: 'pointer',
                      fontWeight: refreshMs === ms ? 700 : 400,
                      textDecoration: refreshMs === ms ? 'underline' : 'none',
                    }}
                  >
                    {ms / 1000}s
                  </Typography>
                ))}
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </Paper>
    </Stack>
  )
}
