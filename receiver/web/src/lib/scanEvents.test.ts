import { describe, expect, it } from 'vitest'

import { type ScanState, scanEvents } from './scanEvents'

// What the camera page should make a noise about.
//
// The rule that shapes all of this: the sender redisplays a chunk until its acknowledgement arrives, so at ten
// frames a second most decodes are duplicates of a chunk already held. A beep per decoded frame would be a
// continuous drone carrying no information. A beep per *new* chunk tracks progress and falls silent the moment
// the camera stops seeing anything it has not already read.

const idle: ScanState = { transmissionId: null, chunksArrived: 0, hasManifest: false, verified: false }

describe('scanEvents', () => {
  it('is silent when nothing has happened', () => {
    expect(scanEvents(idle, idle)).toEqual([])
  })

  it('beeps once for a newly decoded chunk', () => {
    const before: ScanState = { ...idle, transmissionId: 'a', chunksArrived: 1 }
    const after: ScanState = { ...before, chunksArrived: 2 }

    expect(scanEvents(before, after)).toEqual(['chunk'])
  })

  it('stays silent when a chunk arrives again', () => {
    const before: ScanState = { ...idle, transmissionId: 'a', chunksArrived: 3 }

    expect(scanEvents(before, { ...before })).toEqual([])
  })

  // Several chunks can land between two polls. One beep, not five: the point is an audible signal that
  // progress happened, and five beeps 40ms apart is a click, not five beeps.
  it('beeps once even when several chunks arrive between polls', () => {
    const before: ScanState = { ...idle, transmissionId: 'a', chunksArrived: 1 }
    const after: ScanState = { ...before, chunksArrived: 6 }

    expect(scanEvents(before, after)).toEqual(['chunk'])
  })

  it('uses a different tone for the manifest', () => {
    const before: ScanState = { ...idle, transmissionId: 'a' }
    const after: ScanState = { ...before, hasManifest: true }

    expect(scanEvents(before, after)).toEqual(['manifest'])
  })

  // A transmission is only listed once something has arrived for it, so its first sighting may already carry
  // the manifest. It is still news to this page, and staying silent would lose the one tone that says the
  // receiver now knows what it is assembling.
  it('announces a manifest that was already there on first sighting', () => {
    const after: ScanState = { transmissionId: 'a', chunksArrived: 0, hasManifest: true, verified: false }

    expect(scanEvents(idle, after)).toEqual(['manifest'])
  })

  it('chimes when the merged file verifies', () => {
    const before: ScanState = { transmissionId: 'a', chunksArrived: 5, hasManifest: true, verified: false }
    const after: ScanState = { ...before, verified: true }

    expect(scanEvents(before, after)).toEqual(['verified'])
  })

  it('chimes once, not on every poll after verifying', () => {
    const verified: ScanState = { transmissionId: 'a', chunksArrived: 5, hasManifest: true, verified: true }

    expect(scanEvents(verified, { ...verified })).toEqual([])
  })

  // Order matters when a poll catches several things at once: the manifest is what the chunks are for, and the
  // verify chime is the end of the story.
  it('reports the manifest before the chunk and the chime last', () => {
    const before: ScanState = { ...idle, transmissionId: 'a' }
    const after: ScanState = { transmissionId: 'a', chunksArrived: 4, hasManifest: true, verified: true }

    expect(scanEvents(before, after)).toEqual(['manifest', 'chunk', 'verified'])
  })

  // A new transmission starts a new story. Without this, going from 9 chunks to a fresh transmission's 2 looks
  // like a decrease and the new transmission's chunks would never beep.
  it('treats a different transmission as a fresh start', () => {
    const before: ScanState = { transmissionId: 'a', chunksArrived: 9, hasManifest: true, verified: true }
    const after: ScanState = { transmissionId: 'b', chunksArrived: 2, hasManifest: false, verified: false }

    expect(scanEvents(before, after)).toEqual(['chunk'])
  })

  it('is silent when a transmission goes away', () => {
    const before: ScanState = { transmissionId: 'a', chunksArrived: 5, hasManifest: true, verified: true }

    expect(scanEvents(before, idle)).toEqual([])
  })
})
