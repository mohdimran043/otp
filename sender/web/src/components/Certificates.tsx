import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Paper,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { api, type CertificateView } from '../api/client'

// Managing the two certificates this side holds.
//
// The whole of the trust in this scheme is an operator installing the other machine's certificate here, so
// the fingerprint is shown wherever it can be: it is what they compare against the other screen, and it is
// the only thing standing between a working pair and a pair that will never open a frame.
//
// Nothing on this page is secret except by omission. The private key never leaves the server and is not in
// any response — what is shown is the public certificate, which is meant to be copied.

const THIS_SIDE = 'sender'
const OTHER_SIDE = 'receiver'

/** expiry describes a validity window in the terms an operator cares about: is it still good. */
function expiry(cert: CertificateView): { label: string; expired: boolean } {
  if (!cert.not_after) return { label: 'no expiry recorded', expired: false }
  const until = new Date(cert.not_after)
  const expired = until.getTime() < Date.now()
  return {
    label: expired ? `expired ${until.toLocaleDateString()}` : `valid to ${until.toLocaleDateString()}`,
    expired,
  }
}

/** CertificateCard shows one certificate, or says it is missing. */
function CertificateCard({
  title,
  blurb,
  cert,
  action,
}: {
  title: string
  blurb: string
  cert?: CertificateView
  action: React.ReactNode
}) {
  const state = cert ? expiry(cert) : null

  return (
    <Paper variant="outlined" sx={{ p: 2, flex: 1, minWidth: 300 }}>
      <Stack direction="row" alignItems="center" spacing={1} sx={{ mb: 0.5 }}>
        <Typography variant="subtitle1" sx={{ flexGrow: 1 }}>
          {title}
        </Typography>
        {cert ? (
          <Chip
            size="small"
            color={state?.expired ? 'error' : 'success'}
            variant="outlined"
            label={state?.label}
          />
        ) : (
          <Chip size="small" color="warning" variant="outlined" label="not installed" />
        )}
      </Stack>

      <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
        {blurb}
      </Typography>

      {cert && (
        <Box sx={{ mb: 1.5 }}>
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
            {cert.subject || 'no subject'}
          </Typography>
          <Tooltip title="Compare this against the other side's screen. It is what tells you the right certificate was installed.">
            <Typography sx={{ fontFamily: 'monospace', fontSize: '0.72rem', wordBreak: 'break-all' }}>
              {cert.fingerprint}
            </Typography>
          </Tooltip>
        </Box>
      )}

      {action}
    </Paper>
  )
}

export function Certificates() {
  const queryClient = useQueryClient()
  const [pasted, setPasted] = useState('')
  const [copied, setCopied] = useState(false)

  const status = useQuery({ queryKey: ['certificates'], queryFn: api.certificates })
  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ['certificates'] })
    // The transfer form offers certificate encryption only when both halves are in place, so it has to
    // hear about this too.
    void queryClient.invalidateQueries({ queryKey: ['profiles'] })
  }

  const generate = useMutation({ mutationFn: () => api.generateCertificate(), onSuccess: invalidate })
  const install = useMutation({
    mutationFn: (pem: string) => api.installPeerCertificate(pem),
    onSuccess: () => {
      setPasted('')
      invalidate()
    },
  })
  const remove = useMutation({ mutationFn: () => api.removePeerCertificate(), onSuccess: invalidate })

  const local = status.data?.local
  const peer = status.data?.peer
  const ready = status.data?.ready ?? false
  const error = generate.error ?? install.error ?? remove.error ?? status.error

  const copyLocal = async () => {
    if (!local) return
    try {
      await navigator.clipboard.writeText(local.certificate_pem)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard access is refused on an insecure origin, which is an ordinary way to run this. The PEM
      // is on screen and selectable, so there is nothing to recover from.
    }
  }

  return (
    <Stack spacing={2}>
      <Alert severity={ready ? 'success' : 'info'} variant="outlined">
        {status.data?.note ?? 'Reading certificates…'}
      </Alert>

      {error && (
        <Alert severity="error" variant="outlined">
          {error instanceof Error ? error.message : String(error)}
        </Alert>
      )}

      <Stack direction="row" spacing={2} flexWrap="wrap" useFlexGap>
        <CertificateCard
          title={`This ${THIS_SIDE}'s certificate`}
          blurb={`Generated here. The private half never leaves this machine; the certificate below is public and is what the ${OTHER_SIDE} needs.`}
          cert={local}
          action={
            <Stack direction="row" spacing={1}>
              <Button
                size="small"
                variant={local ? 'outlined' : 'contained'}
                onClick={() => generate.mutate()}
                disabled={generate.isPending}
                startIcon={generate.isPending ? <CircularProgress size={14} /> : undefined}
              >
                {local ? 'Regenerate' : 'Generate certificate'}
              </Button>
              {local && (
                <Button size="small" variant="outlined" onClick={copyLocal}>
                  {copied ? 'Copied' : 'Copy PEM'}
                </Button>
              )}
            </Stack>
          }
        />

        <CertificateCard
          title={`The ${OTHER_SIDE}'s certificate`}
          blurb={`Paste what the ${OTHER_SIDE} shows under its own certificate. It is public, so it can travel by any means at all — email, a USB stick, read aloud.`}
          cert={peer}
          action={
            peer && (
              <Button
                size="small"
                variant="outlined"
                color="warning"
                onClick={() => remove.mutate()}
                disabled={remove.isPending}
              >
                Remove
              </Button>
            )
          }
        />
      </Stack>

      {local && (
        <Paper variant="outlined" sx={{ p: 2 }}>
          <Typography variant="subtitle2" sx={{ mb: 1 }}>
            This {THIS_SIDE}'s certificate, to install on the {OTHER_SIDE}
          </Typography>
          <TextField
            fullWidth
            multiline
            minRows={5}
            maxRows={10}
            value={local.certificate_pem}
            slotProps={{ input: { readOnly: true, sx: { fontFamily: 'monospace', fontSize: '0.7rem' } } }}
          />
        </Paper>
      )}

      <Paper variant="outlined" sx={{ p: 2 }}>
        <Typography variant="subtitle2" sx={{ mb: 1 }}>
          Install the {OTHER_SIDE}'s certificate
        </Typography>
        <TextField
          fullWidth
          multiline
          minRows={5}
          maxRows={10}
          placeholder={'-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----'}
          value={pasted}
          onChange={(event) => setPasted(event.target.value)}
          slotProps={{ input: { sx: { fontFamily: 'monospace', fontSize: '0.7rem' } } }}
        />
        <Stack direction="row" spacing={1} sx={{ mt: 1 }}>
          <Button
            variant="contained"
            size="small"
            onClick={() => install.mutate(pasted.trim())}
            disabled={install.isPending || pasted.trim() === ''}
          >
            {peer ? 'Replace' : 'Install'}
          </Button>
          <Button size="small" onClick={() => setPasted('')} disabled={pasted === ''}>
            Clear
          </Button>
        </Stack>
      </Paper>

      <Typography variant="caption" color="text.secondary">
        Replacing this {THIS_SIDE}'s certificate breaks the pair until the new one is installed on the{' '}
        {OTHER_SIDE}: they are still holding the old one, and nothing will open in between. Regenerate
        deliberately, and copy the new PEM across straight afterwards.
      </Typography>
    </Stack>
  )
}
