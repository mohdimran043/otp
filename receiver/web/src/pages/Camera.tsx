import { useEffect, useRef, useState } from 'react'
import { Alert, Stack, Typography } from '@mui/material'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { api } from '../api/client'
import { BrowserCamera } from '../components/BrowserCamera'
import { CameraPicker } from '../components/CameraPicker'

// The camera, on its own page.
//
// It used to live inside Settings, which is why nobody ever saw a permission prompt: `getUserMedia` was only
// called from the Start button, and an operator who never pressed Start never triggered it. Settings is also a
// page an operator visits to read numbers, not to grant access to hardware — asking there, even on a button
// press, buried the one interaction that needs a whole page's attention.
//
// So this page asks as soon as it is opened, before either component below is given a chance to. The stream it
// gets is stopped again immediately — this only wants the prompt and the labelled device list that
// `enumerateDevices` cannot produce until permission has been granted once. Actually capturing still waits for
// the Start button inside BrowserCamera, which is the thing that begins posting frames.
export function Camera() {
  const client = useQueryClient()
  const cameras = useQuery({ queryKey: ['cameras'], queryFn: api.cameras })

  const [permissionNote, setPermissionNote] = useState<string | null>(null)
  // Once per mount, not once per render: a ref survives the re-renders `getUserMedia` itself causes without
  // reopening the prompt every time this component happens to redraw.
  const asked = useRef(false)

  useEffect(() => {
    if (asked.current) return
    asked.current = true

    if (!(typeof window !== 'undefined' && window.isSecureContext)) return

    void navigator.mediaDevices
      .getUserMedia({ video: true })
      .then((stream) => {
        // Released at once. The point was the prompt and the device labels, not a running capture — that
        // stays behind the Start button below, where posting frames is an explicit act rather than a side
        // effect of opening this page.
        stream.getTracks().forEach((track) => track.stop())
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err)
        setPermissionNote(
          /denied|dismissed|NotAllowed/i.test(message)
            ? 'Permission to use the camera was declined. Press Start below to ask again, or allow it from ' +
                'the camera icon in the address bar.'
            : `The camera could not be reached: ${message}`,
        )
      })
  }, [])

  const secure = typeof window !== 'undefined' && window.isSecureContext

  return (
    <Stack spacing={3}>
      <Typography variant="h5">Camera</Typography>

      {!secure && (
        <Alert severity="error" variant="outlined">
          <strong>A browser will not grant a camera to an insecure page.</strong> Open this receiver over
          HTTPS, or over <code>localhost</code>, and the prompt will appear. A plain <code>http://</code>{' '}
          address on a LAN is the one case that cannot work, whatever permissions are granted.
        </Alert>
      )}

      {permissionNote && (
        <Alert severity="warning" variant="outlined" onClose={() => setPermissionNote(null)}>
          {permissionNote}
        </Alert>
      )}

      {/* The browser's camera first, because it is the one that can ask permission and the one that works
          without a device passed into the container. The receiver's own camera is below it, for a deployment
          where the camera is attached to the machine the receiver runs on. */}
      <BrowserCamera
        taking={cameras.data?.source === 'browser'}
        onStart={async () => {
          await api.selectCamera({ device: '', source: 'browser' })
          await client.invalidateQueries({ queryKey: ['cameras'] })
          await client.invalidateQueries({ queryKey: ['config'] })
        }}
        onStop={async () => {
          await api.selectCamera({ device: '', source: 'file' })
          await client.invalidateQueries({ queryKey: ['cameras'] })
          await client.invalidateQueries({ queryKey: ['config'] })
        }}
      />

      <CameraPicker />
    </Stack>
  )
}
