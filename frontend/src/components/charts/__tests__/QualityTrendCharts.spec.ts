import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import QualityTrendCharts from '../QualityTrendCharts.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => (params ? `${key}:${JSON.stringify(params)}` : key)
    })
  }
})

describe('QualityTrendCharts', () => {
  it('renders duration P50/P90 and SLA percent series from quality points', () => {
    const wrapper = mount(QualityTrendCharts, {
      props: {
        points: [
          { bucket: '2026-10-23', success: 99, failed: 1, sla: 0.99, duration_p50_ms: 1200, duration_p90_ms: 5400 },
          { bucket: '2026-10-24', success: 0, failed: 0, sla: null, duration_p50_ms: null, duration_p90_ms: null }
        ]
      },
      global: { stubs: { TrendLineChartCard: true } }
    })

    const cards = wrapper.findAllComponents({ name: 'TrendLineChartCard' })
    expect(cards).toHaveLength(2)

    const [duration, sla] = cards
    expect(duration.props('title')).toBe('dashboard.qualityTrend.duration')
    expect(duration.props('labels')).toEqual(['2026-10-23', '2026-10-24'])
    expect(duration.props('series').map((s: { label: string; data: Array<number | null> }) => [s.label, s.data])).toEqual([
      ['P50', [1200, null]],
      ['P90', [5400, null]]
    ])

    expect(sla.props('title')).toBe('dashboard.qualityTrend.sla')
    expect(sla.props('primaryFormat')).toBe('percent')
    expect(sla.props('series')[0].data).toEqual([99, null])
    expect(sla.props('tooltipFooter')(0)).toBe('dashboard.qualityTrend.slaFooter:{"success":99,"failed":1}')
  })
})
