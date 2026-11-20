import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import AccountSlaBreakdownDialog from '../components/AccountSlaBreakdownDialog.vue'

const getBreakdown = vi.fn()

vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key })
}))
vi.mock('@/api/admin/ops', () => ({
  opsAPI: { getDashboardAccountBreakdown: (...args: unknown[]) => getBreakdown(...args) }
}))

function row(overrides: Record<string, unknown>) {
  return {
    account_id: 1, account_name: 'acct', platform: 'gemini', status: 'active',
    success_count: 0, error_count_total: 0, error_count_sla: 0, request_count_total: 0, request_count_sla: 0,
    sla: null, token_consumed: 0, input_tokens: 0, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0,
    ttft_p50_ms: null, ttft_p90_ms: null, duration_p50_ms: null, duration_p90_ms: null,
    ...overrides
  }
}

const items = [
  row({ account_id: 1119, account_name: 'rootflow', request_count_total: 300, success_count: 30, error_count_total: 270, sla: 0.1, ttft_p50_ms: 9000, ttft_p90_ms: 64000, token_consumed: 5000 }),
  row({ account_id: 1128, account_name: 'vertex', request_count_total: 100, success_count: 100, sla: 1, ttft_p50_ms: 800, ttft_p90_ms: 2400, token_consumed: 9000 }),
  row({ account_id: 0, account_name: '', request_count_total: 5, error_count_total: 5, sla: 0 }),
  row({ account_id: 77, account_name: 'limited', request_count_total: 2, error_count_total: 2, sla: null })
]

function names(wrapper: ReturnType<typeof mount>) {
  return wrapper.findAll('tbody th[scope="row"] .account-name').map((cell) => cell.text())
}

describe('AccountSlaBreakdownDialog', () => {
  beforeAll(() => {
    // jsdom does not implement the modal dialog API.
    HTMLDialogElement.prototype.showModal ??= function (this: HTMLDialogElement) { this.setAttribute('open', '') }
    HTMLDialogElement.prototype.close ??= function (this: HTMLDialogElement) {
      this.removeAttribute('open')
      this.dispatchEvent(new Event('close'))
    }
  })

  beforeEach(() => {
    getBreakdown.mockReset()
    getBreakdown.mockResolvedValue({ start_time: '', end_time: '', latency_degraded: false, truncated: false, items })
  })

  afterEach(() => {
    document.body.innerHTML = ''
  })

  it('opens as a modal, loads with the given filter and sorts by PV first', async () => {
    const showModal = vi.spyOn(HTMLDialogElement.prototype, 'showModal')
    const wrapper = mount(AccountSlaBreakdownDialog, {
      props: { show: false, params: { group_id: 80, mode: 'raw', time_range: '1h' }, scopeLabel: 'gemini-vertex · #80' },
      global: { stubs: { Icon: true } },
      attachTo: document.body
    })

    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(showModal).toHaveBeenCalled()
    expect(getBreakdown).toHaveBeenCalledWith(
      { group_id: 80, mode: 'raw', time_range: '1h' },
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    expect(names(wrapper)).toEqual(['rootflow', 'vertex', 'admin.accountOps.breakdown.unassigned', 'limited'])
    expect(wrapper.find('th[aria-sort]').text()).toContain('PV')
    expect(wrapper.find('th[aria-sort]').attributes('aria-sort')).toBe('descending')
    expect(wrapper.text()).toContain('10.00%')
    expect(wrapper.text()).toContain('64K ms')
    wrapper.unmount()
  })

  it('sorts by SLA with one aria-sort header, keeping empty SLA last', async () => {
    const wrapper = mount(AccountSlaBreakdownDialog, {
      props: { show: true, params: { group_id: 80 }, scopeLabel: '' },
      global: { stubs: { Icon: true } },
      attachTo: document.body
    })
    await flushPromises()

    const slaButton = wrapper.findAll('thead button').find((button) => button.text().startsWith('SLA'))!
    await slaButton.trigger('click')
    expect(names(wrapper)).toEqual(['admin.accountOps.breakdown.unassigned', 'rootflow', 'vertex', 'limited'])
    expect(wrapper.findAll('th[aria-sort]')).toHaveLength(1)
    expect(slaButton.element.closest('th')?.getAttribute('aria-sort')).toBe('ascending')
    expect(wrapper.find('[role="status"]').text()).toBe('admin.accountOps.breakdown.sorted')

    await slaButton.trigger('click')
    expect(names(wrapper)).toEqual(['vertex', 'rootflow', 'admin.accountOps.breakdown.unassigned', 'limited'])
    expect(slaButton.element.closest('th')?.getAttribute('aria-sort')).toBe('descending')
    wrapper.unmount()
  })

  it('emits select-account for real accounts only and closes back to the parent', async () => {
    const wrapper = mount(AccountSlaBreakdownDialog, {
      props: { show: true, params: { group_id: 80 }, scopeLabel: '' },
      global: { stubs: { Icon: true } },
      attachTo: document.body
    })
    await flushPromises()

    const rowButtons = wrapper.findAll('tbody th[scope="row"] button')
    expect(rowButtons).toHaveLength(3)
    await rowButtons[1]!.trigger('click')
    expect(wrapper.emitted('select-account')).toEqual([[1128]])

    ;(wrapper.find('dialog').element as HTMLDialogElement).close()
    expect(wrapper.emitted('update:show')).toEqual([[false]])
    wrapper.unmount()
  })

  it('reloads when the parent hands over refreshed params', async () => {
    const wrapper = mount(AccountSlaBreakdownDialog, {
      props: { show: true, params: { group_id: 80, time_range: '1h' }, scopeLabel: '' },
      global: { stubs: { Icon: true } },
      attachTo: document.body
    })
    await flushPromises()
    expect(getBreakdown).toHaveBeenCalledTimes(1)

    await wrapper.setProps({ params: { group_id: 80, time_range: '6h' } })
    await flushPromises()
    expect(getBreakdown).toHaveBeenCalledTimes(2)
    expect(getBreakdown).toHaveBeenLastCalledWith({ group_id: 80, time_range: '6h' }, expect.anything())
    wrapper.unmount()
  })
})
