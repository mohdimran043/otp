import { Box, Button, Chip, Paper, Stack, Typography } from '@mui/material'
import DownloadIcon from '@mui/icons-material/Download'

import { api, formatBytes, type MergedView } from '../api/client'

// What arrived, shown rather than described.
//
// A hash and a byte count prove a transfer worked, but they do not let an operator recognise the thing they
// sent. An image drawn on screen does, and it is also the most direct demonstration the platform can offer:
// these pixels were rendered as optical frames, displayed, photographed, decoded, reassembled, and checked
// against the sender's hash — and here they are.
//
// The type is inferred from the filename rather than carried in the manifest. The protocol deliberately does
// not transport a content type: it moves bytes, and a receiver that acted on a sender-supplied MIME type
// would be letting the far side decide how its browser renders something.
//
// What may be rendered at all is the server's decision, not this component's. It asks for `?inline=1` and
// gets a real content type only for the types the shared allowlist permits. SVG is the case that matters:
// it looks like an image and is an XML document that may contain <script>, so rendering one from this origin
// would be script running on the page an operator uses to run the receiver. It downloads instead.
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

export function FilePreview({ transmissionId, merged }: { transmissionId: string; merged: MergedView }) {
  // Nothing unverified is ever rendered or offered. The receiver refuses to serve a file that failed its hash
  // check, and showing one here would be presenting corrupt data as the thing that was sent.
  if (!merged.verified) {
    return (
      <Paper variant="outlined" sx={{ p: 2, borderColor: 'error.main' }}>
        <Typography variant="subtitle2" color="error.main">
          Not shown: this file did not match the hash the sender declared
        </Typography>
        <Typography variant="caption" color="text.secondary">
          {merged.verify_error}
        </Typography>
      </Paper>
    )
  }

  const inline = api.inlineUrl(transmissionId)
  const download = api.downloadUrl(transmissionId)
  const kind = kindOf(merged.filename)
  const archive = isArchive(merged.filename)

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Stack spacing={1.5}>
        <Stack direction="row" spacing={1} alignItems="center" flexWrap="wrap" useFlexGap>
          <Typography variant="subtitle1" sx={{ flexGrow: 1 }}>
            The file that arrived
          </Typography>
          <Chip size="small" color="success" label="verified" />
          <Chip size="small" label={formatBytes(merged.size_bytes)} />
          {archive && <Chip size="small" label="archive" />}
          {/* The download is prominent rather than tucked away, because for an archive it is the only thing
              an operator can do with the file — and an archive is what a large transfer usually is. */}
          <Button size="small" variant="outlined" startIcon={<DownloadIcon />} href={download}>
            Download
          </Button>
        </Stack>

        {kind === 'image' && (
          <Box
            component="img"
            src={inline}
            alt={merged.filename}
            sx={{
              maxWidth: '100%',
              maxHeight: 480,
              objectFit: 'contain',
              // A checkerboard behind it, so a transparent PNG is visibly transparent rather than looking
              // like it lost its background somewhere in the pipeline.
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
            sx={{ maxWidth: '100%', maxHeight: 480, borderRadius: 1, bgcolor: '#000' }}
          />
        )}

        {kind === 'download' && (
          <Box
            sx={{
              p: 3,
              borderRadius: 1,
              border: '1px dashed',
              borderColor: 'divider',
              textAlign: 'center',
            }}
          >
            <Typography variant="body2">{merged.filename}</Typography>
            <Typography variant="caption" color="text.secondary">
              {archive
                ? 'An archive — download it to open it. Its contents crossed the gap as bytes; nothing here has looked inside.'
                : 'This type is not shown in place. Download it to open it.'}
            </Typography>
          </Box>
        )}

        <Box>
          <Typography variant="caption" color="text.secondary" display="block">
            SHA-256 computed over the reassembled file — it matched what the sender declared
          </Typography>
          <Typography variant="body2" sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>
            {merged.sha256}
          </Typography>
        </Box>
      </Stack>
    </Paper>
  )
}
