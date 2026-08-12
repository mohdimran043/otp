import { describe, expect, it } from 'vitest'

import { dominantFailure, recoveryShare } from './recoveryBuckets'

describe('dominantFailure', () => {
  it('returns null when nothing has failed', () => {
    expect(dominantFailure(undefined)).toBeNull()
    expect(dominantFailure({})).toBeNull()
    expect(dominantFailure({ decoded: 400 })).toBeNull()
  })

  it('excludes decoded frames, which are not a failure', () => {
    // The whole point: a healthy session has far more decoded frames than anything else, so counting them
    // would make "Read" the permanent answer to "what is going wrong".
    const got = dominantFailure({ decoded: 9000, payload_crc: 12 })
    expect(got?.key).toBe('payload_crc')
    expect(got?.count).toBe(12)
  })

  it('picks the largest failing stage and carries its action', () => {
    const got = dominantFailure({ decoded: 100, no_quad: 40, payload_crc: 9 })
    expect(got?.key).toBe('no_quad')
    expect(got?.label).toBe('Corners not found')
    expect(got?.action).toContain('Fill more of the view')
  })

  it('breaks ties by key so the panel does not flicker between equal counts', () => {
    // Both at five. Polling twice a second, an unstable choice would alternate the advice text on screen.
    const first = dominantFailure({ header_crc: 5, payload_crc: 5 })
    const second = dominantFailure({ payload_crc: 5, header_crc: 5 })
    expect(first?.key).toBe(second?.key)
    expect(first?.key).toBe('header_crc')
  })

  it('ignores zero counts', () => {
    expect(dominantFailure({ no_quad: 0, payload_crc: 0 })).toBeNull()
    expect(dominantFailure({ no_quad: 0, payload_crc: 3 })?.key).toBe('payload_crc')
  })

  it('surfaces an unknown stage rather than hiding it', () => {
    // A bucket added on the Go side and not here must still appear, since an unexplained failure is exactly
    // what someone needs told about.
    const got = dominantFailure({ some_new_stage: 7 })
    expect(got?.key).toBe('some_new_stage')
    expect(got?.label).toBe('some_new_stage')
    expect(got?.action).toContain('receiver log')
  })
})

describe('recoveryShare', () => {
  it('is null when nothing was attempted', () => {
    expect(recoveryShare(0, 0)).toBeNull()
  })

  it('is the recovered fraction otherwise', () => {
    expect(recoveryShare(3, 12)).toBe(0.25)
    expect(recoveryShare(0, 12)).toBe(0)
  })
})
