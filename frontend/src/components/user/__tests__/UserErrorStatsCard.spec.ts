import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import UserErrorStatsCard from '../UserErrorStatsCard.vue'

const { getMyErrorStats } = vi.hoisted(() => ({
  getMyErrorStats: vi.fn(),
}))

vi.mock('@/api', () => ({
  usageAPI: { getMyErrorStats },
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const mountCard = () =>
  mount(UserErrorStatsCard, {
    props: { params: { start_date: '2026-10-23', end_date: '2026-10-24', granularity: 'hour' as const } },
    global: { stubs: { TrendLineChartCard: true } },
  })

describe('UserErrorStatsCard', () => {
  beforeEach(() => {
    getMyErrorStats.mockReset()
  })

  it('shows failure rate and status codes from the API', async () => {
    getMyErrorStats.mockResolvedValue({
      start: '',
      end: '',
      granularity: 'hour',
      total: 200,
      success: 194,
      failed: 6,
      failure_rate: 0.03,
      by_status: [
        { status_code: 502, count: 4 },
        { status_code: 400, count: 2 },
      ],
      trend: [],
      excluded: { business_limited: 21, client_closed: 1 },
    })

    const wrapper = mountCard()
    await flushPromises()

    expect(getMyErrorStats).toHaveBeenCalledWith(
      { start_date: '2026-10-23', end_date: '2026-10-24', granularity: 'hour' },
      expect.anything()
    )
    expect(wrapper.get('[data-testid="failure-rate"]').text()).toBe('3.00%')
    const codes = wrapper.findAll('li').map((li) => li.text())
    expect(codes[0]).toContain('502')
    expect(codes[1]).toContain('400')
    expect(wrapper.text()).toContain('usage.errorStats.excluded')
  })

  it('hides itself when the endpoint is unavailable', async () => {
    getMyErrorStats.mockRejectedValue(new Error('Ops monitoring is disabled'))

    const wrapper = mountCard()
    await flushPromises()

    expect(wrapper.html()).toBe('<!--v-if-->')
  })
})
