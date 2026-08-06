import { Alert, Box, Button, Chip, Paper, Stack, Typography } from '@mui/material'
import DownloadIcon from '@mui/icons-material/Download'
import OpenInNewIcon from '@mui/icons-material/OpenInNew'
import { useQuery } from '@tanstack/react-query'

import { api, formatBytes, type TransmissionView } from '../api/client'

// Comparing what arrived against what was sent, by hand.
//
// The hashes agreeing is the proof, and it is a better proof than any inspection by eye — a single flipped
// bit changes the whole digest, which nothing a person could notice would do. But a proof is not the same as
// being convinced, and an operator moving something they care across an air gap is entitled to look at both
// ends themselves.
//
// So this panel does three things. It puts the two hashes next to each other, character for character, so
// the comparison can be made without trusting the tick. It offers the received bytes for download, so they
// can be diffed against the original with whatever tool the operator trusts. And when the sender's interface
// is reachable it links straight to the same transfer there — both sides address a transfer by the same
// transmission id, which is what makes that possible.

export function Compare({ transmission }: { transmission: TransmissionView }) {
  const config = useQuery({ queryKey: ['config'], queryFn: api.config })
  const merged = transmission.merged
  const senderUI = config.data?.peer?.sender_ui_url?.replace(/\/+$/, '') ?? ''

  const expected = transmission.expected_sha256
  const received = merged?.sha256 ?? ''
  const match = Boolean(received) && received === expected
  const sizeMatch = merged ? merged.size_bytes === transmission.original_size : false

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Stack direction="row" alignItems="center" spacing={1} sx={{ mb: 1.5 }} flexWrap="wrap" useFlexGap>
        <Typography variant="subtitle1" sx={{ flexGrow: 1 }}>
          Compare with what was sent
        </Typography>
        {merged?.verified && <Chip size="small" color="success" label="hashes agree" />}
        {merged && !merged.verified && <Chip size="small" color="error" label="does not match" />}
        {merged && (
          <Button
            size="small"
            variant="outlined"
            startIcon={<DownloadIcon />}
            href={api.downloadUrl(transmission.transmission_id)}
          >
            Download the received file
          </Button>
        )}
        {senderUI && (
          <Button
            size="small"
            startIcon={<OpenInNewIcon />}
            href={`${senderUI}/transfers/${transmission.transmission_id}`}
            target="_blank"
            rel="noreferrer"
          >
            Open on the sender
          </Button>
        )}
      </Stack>

      <Stack spacing={1.5}>
        <Box
          sx={{
            display: 'grid',
            gridTemplateColumns: { xs: '1fr', md: 'auto 1fr' },
            columnGap: 2,
            rowGap: 0.75,
            alignItems: 'baseline',
          }}
        >
          <Typography variant="caption" color="text.secondary">
            Filename
          </Typography>
          <Typography variant="body2">{merged?.filename ?? transmission.filename}</Typography>

          <Typography variant="caption" color="text.secondary">
            Size sent
          </Typography>
          <Typography variant="body2">
            {formatBytes(transmission.original_size)}{' '}
            <Typography component="span" variant="caption" color="text.secondary">
              ({transmission.original_size.toLocaleString()} bytes)
            </Typography>
          </Typography>

          <Typography variant="caption" color="text.secondary">
            Size received
          </Typography>
          <Typography variant="body2" color={merged && !sizeMatch ? 'error.main' : undefined}>
            {merged ? (
              <>
                {formatBytes(merged.size_bytes)}{' '}
                <Typography component="span" variant="caption" color="text.secondary">
                  ({merged.size_bytes.toLocaleString()} bytes)
                </Typography>
              </>
            ) : (
              'not merged yet'
            )}
          </Typography>

          <Typography variant="caption" color="text.secondary">
            SHA-256 declared by the sender
          </Typography>
          <Typography variant="body2" sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>
            {expected}
          </Typography>

          <Typography variant="caption" color="text.secondary">
            SHA-256 computed here
          </Typography>
          <Typography
            variant="body2"
            sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}
            color={received && !match ? 'error.main' : undefined}
          >
            {received || 'not merged yet'}
          </Typography>
        </Box>

        {merged && match && (
          <Alert severity="success" variant="outlined">
            Identical. The digest was computed here over the reassembled file, and the sender's was computed
            before anything was rendered — so this is the file that was sent, not a file that resembles it.
            A single flipped bit anywhere would change every character after it.
          </Alert>
        )}

        {merged && !match && (
          <Alert severity="error" variant="outlined">
            These differ, so the reassembled file is not what was sent. It is kept as evidence and refused on
            the download endpoint rather than handed on. {merged.verify_error}
          </Alert>
        )}

        {!senderUI && (
          <Typography variant="caption" color="text.secondary">
            To compare side by side in one click, set <code>OTP_RECEIVER_PEER_SENDER_UI_URL</code> to the
            sender's address — both sides address a transfer by the same transmission id
            (<code>{transmission.transmission_id}</code>), so the sender's page for this transfer is at{' '}
            <code>/transfers/{transmission.transmission_id}</code>. It is left unset by default because on a
            genuinely air-gapped installation the sender's interface may not be reachable from here, and a
            link that cannot work is worse than none.
          </Typography>
        )}
      </Stack>
    </Paper>
  )
}
