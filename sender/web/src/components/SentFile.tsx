import { Box, Button, Chip, Paper, Stack, Typography } from '@mui/material'
import DownloadIcon from '@mui/icons-material/Download'

import { api, formatBytes } from '../api/client'

// The file as it was sent, shown on the sender.
//
// The receiver already renders what it reassembled. Showing the same file here is what lets an operator
// compare the two ends rather than take a hash on trust — and on the rare occasion the hashes disagree, the
// two pictures side by side are the diagnosis rather than the first step towards it.
//
// The type is inferred from the filename and the server decides what may be rendered. This page asks for
// `inline=1` and gets it only for the types the shared allowlist permits; anything else arrives as an opaque
// download whatever this component would have liked to do with it. That matters because a transferred file
// came from outside — a caller uploaded it — and an SVG rendered from this origin would be script running on
// the page that controls the sender.
function kindOf(filename: string): 'image' | 'audio' | 'video' | 'download' {
  const extension = filename.toLowerCase().split('.').pop() ?? ''
  if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'avif', 'ico'].includes(extension)) return 'image'
  if (['wav', 'mp3', 'm4a', 'aac', 'flac', 'oga', 'opus'].includes(extension)) return 'audio'
  if (['mp4', 'm4v', 'webm', 'ogv', 'mov'].includes(extension)) return 'video'
  return 'download'
}

function isArchive(filename: string): boolean {
  const name = filename.toLowerCase()
  return [
    '.zip', '.tar', '.tgz', '.tar.gz', '.tar.bz2', '.tar.xz', '.tar.zst',
    '.7z', '.rar', '.gz', '.bz2', '.xz', '.zst', '.iso', '.dmg',
  ].some((suffix) => name.endsWith(suffix))
}

interface Props {
  transmissionId: string
  filename: string
  sizeBytes: number
  sha256: string
}

export function SentFile({ transmissionId, filename, sizeBytes, sha256 }: Props) {
  const inline = api.originalFile(transmissionId, true)
  const download = api.originalFile(transmissionId)
  const kind = kindOf(filename)

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Stack spacing={1.5}>
        <Stack direction="row" spacing={1} alignItems="center" flexWrap="wrap" useFlexGap>
          <Typography variant="subtitle1" sx={{ flexGrow: 1 }}>
            The file as it was sent
          </Typography>
          <Chip size="small" label={formatBytes(sizeBytes)} />
          {isArchive(filename) && <Chip size="small" label="archive" />}
          <Button size="small" startIcon={<DownloadIcon />} href={download}>
            Download
          </Button>
        </Stack>

        {kind === 'image' && (
          <Box
            component="img"
            src={inline}
            alt={filename}
            sx={{
              maxWidth: '100%',
              maxHeight: 420,
              objectFit: 'contain',
              // A checkerboard behind it, so a transparent PNG reads as transparent rather than as one that
              // lost its background somewhere in the pipeline.
              backgroundImage:
                'linear-gradient(45deg, #222 25%, transparent 25%), linear-gradient(-45deg, #222 25%, transparent 25%), linear-gradient(45deg, transparent 75%, #222 75%), linear-gradient(-45deg, transparent 75%, #222 75%)',
              backgroundSize: '16px 16px',
              backgroundPosition: '0 0, 0 8px, 8px -8px, -8px 0px',
              borderRadius: 1,
            }}
          />
        )}

        {kind === 'audio' && <Box component="audio" src={inline} controls sx={{ width: '100%' }} />}

        {kind === 'video' && (
          <Box
            component="video"
            src={inline}
            controls
            preload="metadata"
            sx={{ maxWidth: '100%', maxHeight: 420, borderRadius: 1, bgcolor: '#000' }}
          />
        )}

        {kind === 'download' && (
          <Typography variant="body2" color="text.secondary">
            {filename} — {isArchive(filename) ? 'an archive' : 'this type'} is not shown in place. Download it
            to check it.
          </Typography>
        )}

        <Box>
          <Typography variant="caption" color="text.secondary" display="block">
            SHA-256 declared in the manifest — compare this against the receiver's
          </Typography>
          <Typography variant="body2" sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>
            {sha256}
          </Typography>
        </Box>
      </Stack>
    </Paper>
  )
}
