import { describe, expect, it } from 'vitest'

import { videoConstraints } from './videoConstraints'

// The capture resolution decides which geometries can be read at all, and more is not simply better.
//
// A frame is 132 cells across at a 128 grid, and the decoder needs roughly 3.8 camera pixels per cell. A
// 1080-pixel short side gives 8.2 px/cell there — comfortable — and it is the resolution every frame that has
// ever decoded on the handheld rig was captured at.
//
// Asking a phone for its full sensor instead was tried, and on a colour payload it was worse: at 4K the sensor
// resolves the display's own pixel grid and the two beat against each other, so a cell is no longer one colour.
// Downsampling afterwards does not undo it. 1080p bins optically, before the demosaic, and averages the
// display's subpixels rather than aliasing against them.
//
// `ideal` is a preference throughout, so a camera that cannot manage the request returns its closest match
// rather than failing — demanding a resolution is how you get no camera at all.

describe('videoConstraints', () => {
  it('asks for 1080p rather than the sensor best, so colour survives', () => {
    const v = videoConstraints({ rearFacing: true }) as MediaTrackConstraints
    expect(v.width).toEqual({ ideal: 1920 })
    expect(v.height).toEqual({ ideal: 1080 })
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

  // A chosen device is exact — the operator picked it from a list — but it gets the same resolution preference,
  // because the reason for that preference is the display being photographed, not which camera is doing it.
  it('pins an explicitly chosen device and still asks for 1080p', () => {
    const v = videoConstraints({ deviceId: 'cam-7', rearFacing: true }) as MediaTrackConstraints
    expect(v.deviceId).toEqual({ exact: 'cam-7' })
    expect(v.width).toEqual({ ideal: 1920 })
    expect(v.height).toEqual({ ideal: 1080 })
  })

  // facingMode and an exact deviceId together can be contradictory — the named device may be the front one —
  // and the device the operator picked must win.
  it('does not also ask for a facing mode when a device is named', () => {
    const v = videoConstraints({ deviceId: 'cam-7', rearFacing: true }) as MediaTrackConstraints
    expect(v.facingMode).toBeUndefined()
  })
})
