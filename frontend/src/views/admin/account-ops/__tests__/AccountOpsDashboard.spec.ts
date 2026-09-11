import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AccountOpsDashboard from '../AccountOpsDashboard.vue'

const replace = vi.fn()
const account = { id: 42, name: 'primary', platform: 'openai', status: 'active' }
const listAccounts = vi.fn()
const getAccount = vi.fn()
const getSnapshot = vi.fn()
const getDistribution = vi.fn()
const getModels = vi.fn()

vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key })
}))
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: { account_id: '42' } }),
  useRouter: () => ({ replace })
}))
vi.mock('@/api', () => ({
  adminAPI: {
    accounts: {
      list: (...args: unknown[]) => listAccounts(...args),
      getById: (...args: unknown[]) => getAccount(...args)
    }
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
  })

  it('loads every dashboard query with the selected account in raw mode', async () => {
    const wrapper = shallowMount(AccountOpsDashboard, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          Select: true,
          Icon: true,
          TokenUsageTrend: true,
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
})
