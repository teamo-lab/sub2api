import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import type { QualityTrendPoint, QualityTrendResponse } from '@/types'
import DashboardView from '../DashboardView.vue'

const { getDashboardStats, getDashboardTrend, getDashboardModels, getDashboardQualityTrend, getByDateRange, getMyPlatformQuotas } = vi.hoisted(() => ({
  getDashboardStats: vi.fn(),
  getDashboardTrend: vi.fn(),
  getDashboardModels: vi.fn(),
  getDashboardQualityTrend: vi.fn(),
  getByDateRange: vi.fn(),
  getMyPlatformQuotas: vi.fn()
}))

vi.mock('@/api/usage', () => ({
  usageAPI: {
    getDashboardStats,
    getDashboardTrend,
    getDashboardModels,
    getDashboardQualityTrend,
    getByDateRange
  }
}))

vi.mock('@/api/user', () => ({
  getMyPlatformQuotas
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    user: { balance: 0 },
    isSimpleMode: false,
    refreshUser: vi.fn().mockResolvedValue(undefined)
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const point = (bucket: string, sla: number): QualityTrendPoint => ({
  bucket,
  success: 99,
  failed: 1,
  sla,
  duration_p50_ms: 1200,
  duration_p90_ms: 5400
})

const qualityResponse = (points: QualityTrendPoint[]): QualityTrendResponse => ({
  start: '',
  end: '',
  granularity: 'day',
  points
})

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function mountDashboard() {
  return mount(DashboardView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        LoadingSpinner: true,
        UserDashboardStats: true,
        UserDashboardRecentUsage: true,
        UserDashboardQuickActions: true,
        DateRangePicker: true,
        Select: true,
        Doughnut: true,
        TokenUsageTrend: true,
        QualityTrendCharts: true
      }
    }
  })
}

describe('user DashboardView quality trend', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getDashboardStats.mockResolvedValue({})
    getDashboardTrend.mockResolvedValue({ trend: [{ date: '2026-10-24', requests: 1 }] })
    getDashboardModels.mockResolvedValue({ models: [] })
    getByDateRange.mockResolvedValue({ items: [] })
    getMyPlatformQuotas.mockResolvedValue({ platform_quotas: [] })
  })

  it('hides only the quality charts when the quality trend fails', async () => {
    getDashboardQualityTrend.mockRejectedValue({ status: 404, message: 'Ops monitoring is disabled' })
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})

    const wrapper = mountDashboard()
    await flushPromises()

    expect(getDashboardQualityTrend).toHaveBeenCalledTimes(1)
    expect(wrapper.findComponent({ name: 'QualityTrendCharts' }).exists()).toBe(false)
    const tokenTrend = wrapper.findComponent({ name: 'TokenUsageTrend' })
    expect(tokenTrend.exists()).toBe(true)
    expect(tokenTrend.props('trendData')).toHaveLength(1)
    errorSpy.mockRestore()
  })

  it('keeps the newest quality response when an older request settles later', async () => {
    const first = deferred<QualityTrendResponse>()
    const second = deferred<QualityTrendResponse>()
    getDashboardQualityTrend.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})

    const wrapper = mountDashboard()
    await flushPromises()
    // 切换时间范围:再发一次请求,前一次还没回来
    wrapper.findComponent({ name: 'UserDashboardCharts' }).vm.$emit('dateRangeChange')
    await flushPromises()
    expect(getDashboardQualityTrend).toHaveBeenCalledTimes(2)

    second.resolve(qualityResponse([point('2026-10-24', 0.99)]))
    await flushPromises()
    // 旧请求成功晚到:不能覆盖新结果
    first.resolve(qualityResponse([point('2026-10-01', 0.5)]))
    await flushPromises()

    const charts = wrapper.findComponent({ name: 'QualityTrendCharts' })
    expect(charts.exists()).toBe(true)
    expect(charts.props('points')).toEqual([point('2026-10-24', 0.99)])
    expect(charts.props('loading')).toBe(false)

    // 旧请求失败晚到:也不能把已加载的图隐藏
    const third = deferred<QualityTrendResponse>()
    const fourth = deferred<QualityTrendResponse>()
    getDashboardQualityTrend.mockReturnValueOnce(third.promise).mockReturnValueOnce(fourth.promise)
    wrapper.findComponent({ name: 'UserDashboardCharts' }).vm.$emit('dateRangeChange')
    wrapper.findComponent({ name: 'UserDashboardCharts' }).vm.$emit('dateRangeChange')
    await flushPromises()
    fourth.resolve(qualityResponse([point('2026-10-25', 0.98)]))
    await flushPromises()
    third.reject(new Error('stale failure'))
    await flushPromises()

    const latest = wrapper.findComponent({ name: 'QualityTrendCharts' })
    expect(latest.exists()).toBe(true)
    expect(latest.props('points')).toEqual([point('2026-10-25', 0.98)])
    errorSpy.mockRestore()
  })
})
