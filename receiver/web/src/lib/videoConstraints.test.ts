import { describe, expect, it } from 'vitest'

import { videoConstraints } from './videoConstraints'

// The capture resolution decides which geometries can be read at all, and it was pinned to 1080p.
//
// A frame is 132 cells across at a 128 grid and 388 at a 384 grid, and the decoder needs roughly 3.8 camera
// pixels per cell. Filling a 1080-pixel short side gives 8.2 px/cell at 128 — comfortable, and it worked — but
// only 2.8 at 384, which is below the floor however carefully the camera is aimed. So a denser grid was not
// merely hard, it was impossible, and the failure looked identical to bad framing: nothing decoded, nothing even
// stored, no explanation.
//
// Asking for the sensor's best fixes it. At a 2160-pixel short side, 384 gives 5.6 px/cell. `ideal` is a
// preference, so a camera that cannot manage 4K simply returns its closest match rather than failing.

describe('videoConstraints', () => {
  it('asks for the sensor best rather than 1080p', () => {
    const v = videoConstraints({ rearFacing: true }) as MediaTrackConstraints
    expect(v.width).toEqual({ ideal: 3840 })
    expect(v.height).toEqual({ ideal: 2160 })
  })

  // A phone's rear camera is the one that can point at a display. Asked for as a preference, because a laptop
  // has no rear camera and requiring one would fail outright.
  it('prefers the rear camera without demanding it', () => {
    const v = videoConstraints({ rearFacing: true }) as MediaTrackConstraints
    expect(v.facingMode).toEqual({ ideal: 'environment' })
  })

  it('leaves facingMode unset when the front camera is wanted', () => {
    const v = videoConstraints({ rearFacing: false }) as MediaTrackConstraints
    expect(v.facingMode).toBeUndefined()
  })

  // A chosen device is exact — the operator picked it from a list — but it still gets the best resolution, which
  // is the whole point of the change.
  it('pins an explicitly chosen device and still asks for the best resolution', () => {
    const v = videoConstraints({ deviceId: 'cam-7', rearFacing: true }) as MediaTrackConstraints
    expect(v.deviceId).toEqual({ exact: 'cam-7' })
    expect(v.width).toEqual({ ideal: 3840 })
    expect(v.height).toEqual({ ideal: 2160 })
  })

  // facingMode and an exact deviceId together can be contradictory — the named device may be the front one —
  // and the device the operator picked must win.
  it('does not also ask for a facing mode when a device is named', () => {
    const v = videoConstraints({ deviceId: 'cam-7', rearFacing: true }) as MediaTrackConstraints
    expect(v.facingMode).toBeUndefined()
  })
})
