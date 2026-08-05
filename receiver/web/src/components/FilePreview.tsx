import { Box, Paper, Stack, Typography } from '@mui/material'

import { api, type MergedView } from '../api/client'

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
function kindOf(filename: string): 'image' | 'audio' | 'video' | 'text' | 'other' {
  const extension = filename.toLowerCase().split('.').pop() ?? ''
  if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'svg'].includes(extension)) return 'image'
  if (['wav', 'mp3', 'ogg', 'flac', 'm4a', 'aac'].includes(extension)) return 'audio'
  if (['mp4', 'webm', 'mov'].includes(extension)) return 'video'
  if (['txt', 'json', 'csv', 'log', 'md', 'yaml', 'yml'].includes(extension)) return 'text'
  return 'other'
}

export function FilePreview({ transmissionId, merged }: { transmissionId: string; merged: MergedView }) {
  // Nothing unverified is ever rendered. The receiver refuses to serve a file that failed its hash check,
  // and showing one here would be presenting corrupt data as the thing that was sent.
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

  const url = api.downloadUrl(transmissionId)
  const kind = kindOf(merged.filename)

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Stack spacing={1}>
        <Typography variant="subtitle2">Received file</Typography>

        {kind === 'image' && (
          <Box
            component="img"
            src={url}
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

        {kind === 'audio' && (
          <Box component="audio" src={url} controls sx={{ width: '100%' }} />
        )}

        {kind === 'video' && (
          <Box component="video" src={url} controls sx={{ maxWidth: '100%', maxHeight: 480 }} />
        )}

        {(kind === 'text' || kind === 'other') && (
          <Typography variant="body2" color="text.secondary">
            {merged.filename} — no inline preview for this type. It can be downloaded above.
          </Typography>
        )}
      </Stack>
    </Paper>
  )
}
