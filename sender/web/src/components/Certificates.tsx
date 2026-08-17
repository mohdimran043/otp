import { useRef, useState } from 'react'
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
import DownloadIcon from '@mui/icons-material/Download'
import UploadFileIcon from '@mui/icons-material/UploadFile'
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
  const peerFile = useRef<HTMLInputElement>(null)

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
          blurb={`Generated here. Download it and install it on the ${OTHER_SIDE}. Only the public half is downloadable — the private key has no endpoint at all and never leaves this machine.`}
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
                <>
                  {/* An anchor, so it lands in the browser's downloads and can be carried to the other
                      machine on a stick. There is no button for the private key and never will be — it has
                      no endpoint, appears in no response, and never leaves the server. */}
                  <Button
                    size="small"
                    variant="outlined"
                    component="a"
                    href={api.localCertificateUrl()}
                    startIcon={<DownloadIcon />}
                  >
                    Download
                  </Button>
                  <Button size="small" variant="outlined" onClick={copyLocal}>
                    {copied ? 'Copied' : 'Copy PEM'}
                  </Button>
                </>
              )}
            </Stack>
          }
        />

        <CertificateCard
          title={`The ${OTHER_SIDE}'s certificate`}
          blurb={`Upload or paste the ${OTHER_SIDE}'s public certificate. It is public, so it can travel by any means at all — email, a USB stick, read aloud.`}
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
            This {THIS_SIDE}'s public certificate, to install on the {OTHER_SIDE}
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
          Install the {OTHER_SIDE}'s public certificate
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
        <Stack direction="row" spacing={1} sx={{ mt: 1 }} flexWrap="wrap" useFlexGap>
          <Button
            variant="contained"
            size="small"
            onClick={() => install.mutate(pasted.trim())}
            disabled={install.isPending || pasted.trim() === ''}
          >
            {peer ? 'Replace' : 'Install'}
          </Button>

          {/* Uploading a file and pasting text install the same certificate the same way. The file is
              read here rather than posted as multipart, so both paths reach one endpoint and there is
              only one place a malformed PEM can be rejected. */}
          <Button
            size="small"
            variant="outlined"
            startIcon={<UploadFileIcon />}
            onClick={() => peerFile.current?.click()}
            disabled={install.isPending}
          >
            Upload a .pem file
          </Button>
          <input
            ref={peerFile}
            hidden
            type="file"
            accept=".pem,.crt,.cer,application/x-pem-file,application/x-x509-ca-cert,text/plain"
            onChange={async (event) => {
              const file = event.target.files?.[0]
              event.target.value = ''
              if (!file) return
              const text = await file.text()
              // Shown as well as installed, so the operator can see what arrived and compare its
              // fingerprint against the other screen rather than trusting the filename.
              setPasted(text.trim())
              install.mutate(text.trim())
            }}
          />

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
