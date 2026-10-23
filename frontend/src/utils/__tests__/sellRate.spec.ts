import { describe, expect, it } from 'vitest'
import { normalizeSellRateInput, sellRateCreatePayload, sellRateUpdatePayload } from '../sellRate'

describe('sellRate', () => {
  it('treats empty, invalid and negative input as unset', () => {
    for (const v of [null, undefined, '', 'abc', Number.NaN, Number.POSITIVE_INFINITY, -1]) {
      expect(normalizeSellRateInput(v)).toBeNull()
    }
  })

  it('keeps zero and positive values', () => {
    expect(normalizeSellRateInput(0)).toBe(0)
    expect(normalizeSellRateInput(1.25)).toBe(1.25)
    expect(normalizeSellRateInput('4.2')).toBe(4.2)
  })

  it('omits unset value on create and sends -1 to clear on update', () => {
    expect(sellRateCreatePayload('')).toBeUndefined()
    expect(sellRateCreatePayload(1.1)).toBe(1.1)
    expect(sellRateUpdatePayload('')).toBe(-1)
    expect(sellRateUpdatePayload(null)).toBe(-1)
    expect(sellRateUpdatePayload(0)).toBe(0)
    expect(sellRateUpdatePayload(2)).toBe(2)
  })
})
