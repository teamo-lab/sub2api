import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AccountOpsDashboard from '../AccountOpsDashboard.vue'
import TrendLineChartCard from '@/components/charts/TrendLineChartCard.vue'

const replace = vi.fn()
const account = { id: 42, name: 'primary', platform: 'openai', status: 'active' }
const listAccounts = vi.fn()
const getAccount = vi.fn()
const getSnapshot = vi.fn()
const getDistribution = vi.fn()
const getModels = vi.fn()
const listGroups = vi.fn()
let routeQuery: Record<string, string> = { account_id: '42' }

vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key })
}))
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: routeQuery }),
  useRouter: () => ({ replace })
}))
vi.mock('@/api', () => ({
  adminAPI: {
    accounts: {
      list: (...args: unknown[]) => listAccounts(...args),
      getById: (...args: unknown[]) => getAccount(...args)
    },
    groups: { getAll: (...args: unknown[]) => listGroups(...args) }
  }
}))
vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    getDashboardSnapshotV2: (...args: unknown[]) => getSnapshot(...args),
    getErrorDistribution: (...args: unknown[]) => getDistribution(...args),
    getDashboardModels: (...args: unknown[]) => getModels(...args)
  }
}))

describe('AccountOpsDashboard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    listAccounts.mockResolvedValue({ items: [account], total: 1 })
    getAccount.mockResolvedValue(account)
    getSnapshot.mockResolvedValue({ overview: {}, throughput_trend: { points: [] }, error_trend: { points: [] } })
    getDistribution.mockResolvedValue({ total: 0, items: [] })
    getModels.mockResolvedValue(['gpt-5'])
    listGroups.mockResolvedValue([{ id: 7, name: 'premium', platform: 'openai' }])
    routeQuery = { account_id: '42' }
  })

  it('loads every dashboard query with the selected account in raw mode', async () => {
    const wrapper = shallowMount(AccountOpsDashboard, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          Select: true,
          Icon: true,
          TokenUsageTrend: true,
          TrendLineChartCard: true,
          BaseDialog: true,
          OpsThroughputTrendChart: true,
          OpsErrorDistributionChart: true,
          OpsErrorTrendChart: true,
          OpsAlertEventsCard: true,
          OpsErrorDetailsModal: true,
          OpsErrorDetailModal: true,
          OpsRequestDetailsModal: true
        }
      }
    })

    await flushPromises()

    expect(getSnapshot).toHaveBeenCalledWith(
      expect.objectContaining({ account_id: 42, mode: 'raw', time_range: '1h' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    expect(getSnapshot.mock.calls[0]?.[0]).not.toHaveProperty('platform')
    expect(getDistribution).toHaveBeenCalledWith(expect.objectContaining({ account_id: 42 }), expect.anything())
    expect(getModels).toHaveBeenCalledWith(expect.objectContaining({ account_id: 42 }), expect.anything())

    wrapper.unmount()
  })

  it('loads a group-only scope without requiring an account', async () => {
    routeQuery = { group_id: '7' }
    const wrapper = shallowMount(AccountOpsDashboard, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' }, Select: true, Icon: true, BaseDialog: true,
          TokenUsageTrend: true, TrendLineChartCard: true, OpsThroughputTrendChart: true,
          OpsErrorDistributionChart: true, OpsErrorTrendChart: true, OpsAlertEventsCard: true,
          OpsErrorDetailsModal: true, OpsErrorDetailModal: true, OpsRequestDetailsModal: true
        }
      }
    })

    await flushPromises()

    expect(getSnapshot).toHaveBeenCalledWith(
      expect.objectContaining({ group_id: 7, account_id: null, mode: 'raw', time_range: '1h' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    expect(getDistribution).toHaveBeenCalledWith(expect.objectContaining({ group_id: 7 }), expect.anything())
    wrapper.unmount()
  })

  it('reloads account options within the selected group', async () => {
    const wrapper = shallowMount(AccountOpsDashboard, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' }, Icon: true, BaseDialog: true,
          TokenUsageTrend: true, TrendLineChartCard: true, OpsThroughputTrendChart: true,
          OpsErrorDistributionChart: true, OpsErrorTrendChart: true, OpsAlertEventsCard: true,
          OpsErrorDetailsModal: true, OpsErrorDetailModal: true, OpsRequestDetailsModal: true
        }
      }
    })

    await flushPromises()
    listAccounts.mockClear()

    await wrapper.findAllComponents({ name: 'Select' })[0]!.vm.$emit('change', 7)
    await flushPromises()

    expect(listAccounts).toHaveBeenCalledWith(
      1,
      50,
      expect.objectContaining({ group: '7', lite: 'true' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    expect(wrapper.findAllComponents({ name: 'Select' })[1]!.props('modelValue')).toBeNull()
    wrapper.unmount()
  })

  it('passes a custom time window to every dashboard query', async () => {
    routeQuery = {
      account_id: '42',
      time_range: 'custom',
      start_time: '2026-09-10T00:00:00.000Z',
      end_time: '2026-09-10T02:00:00.000Z'
    }
    const wrapper = shallowMount(AccountOpsDashboard, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' }, Select: true, Icon: true, BaseDialog: true,
          TokenUsageTrend: true, TrendLineChartCard: true, OpsThroughputTrendChart: true,
          OpsErrorDistributionChart: true, OpsErrorTrendChart: true, OpsAlertEventsCard: true,
          OpsErrorDetailsModal: true, OpsErrorDetailModal: true, OpsRequestDetailsModal: true
        }
      }
    })

    await flushPromises()

    const expectedWindow = {
      start_time: '2026-09-10T00:00:00.000Z',
      end_time: '2026-09-10T02:00:00.000Z'
    }
    expect(getSnapshot).toHaveBeenCalledWith(expect.objectContaining(expectedWindow), expect.anything())
    expect(getDistribution).toHaveBeenCalledWith(expect.objectContaining(expectedWindow), expect.anything())
    expect(getModels).toHaveBeenCalledWith(expect.objectContaining(expectedWindow), expect.anything())
    expect(getSnapshot.mock.calls[0]?.[0]).not.toHaveProperty('time_range')
    wrapper.unmount()
  })

  it('maps SLA, PV, and TTFT bucket values into three shared trend cards', async () => {
    getSnapshot.mockResolvedValue({
      overview: {},
      throughput_trend: {
        points: [{
          bucket_start: '2026-09-10T00:00:00.000Z', request_count: 10, token_consumed: 100,
          input_tokens: 60, output_tokens: 40, qps: 1, tps: 10, ttft_p50_ms: 240, ttft_p90_ms: 810
        }]
      },
      error_trend: {
        points: [{
          bucket_start: '2026-09-10T00:00:00.000Z', error_count_total: 2,
          business_limited_count: 1, error_count_sla: 1,
          upstream_error_count_excl_429_529: 0, upstream_429_count: 0, upstream_529_count: 0
        }]
      }
    })
    const wrapper = shallowMount(AccountOpsDashboard, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' }, Select: true, Icon: true, BaseDialog: true,
          TokenUsageTrend: true, OpsThroughputTrendChart: true, OpsErrorDistributionChart: true,
          OpsErrorTrendChart: true, OpsAlertEventsCard: true, OpsErrorDetailsModal: true,
          OpsErrorDetailModal: true, OpsRequestDetailsModal: true
        }
      }
    })

    await flushPromises()

    const cards = wrapper.findAllComponents(TrendLineChartCard)
    expect(cards).toHaveLength(3)
    expect(cards[0]?.props('series')[0].data[0]).toBeCloseTo((8 / 9) * 100)
    expect(cards[1]?.props('series')[0].data).toEqual([10])
    expect(cards[2]?.props('series').map((item: any) => item.data[0])).toEqual([240, 810])
    wrapper.unmount()
  })
})
