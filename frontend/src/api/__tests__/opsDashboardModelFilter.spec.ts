import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../client'
import { getDashboardModels, getDashboardSnapshotV2 } from '../admin/ops'

afterEach(() => vi.restoreAllMocks())

describe('ops dashboard model filter', () => {
  it('passes the selected model to dashboard queries', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({ data: {} })

    await getDashboardSnapshotV2({ time_range: '1h', model: 'gpt-5.6-sol' })

    expect(get).toHaveBeenCalledWith('/admin/ops/dashboard/snapshot-v2', expect.objectContaining({
      params: { time_range: '1h', model: 'gpt-5.6-sol' }
    }))
  })

  it('loads model choices from the ops dashboard endpoint', async () => {
    vi.spyOn(apiClient, 'get').mockResolvedValue({ data: { models: ['gpt-5.6-sol'] } })

    await expect(getDashboardModels({ time_range: '1h' })).resolves.toEqual(['gpt-5.6-sol'])
  })
})
