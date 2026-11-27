import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import OpsErrorDetailModal from '../OpsErrorDetailModal.vue'

const mocks = vi.hoisted(() => ({
  getRequestErrorDetail: vi.fn(),
  listRequestErrorUpstreamErrors: vi.fn()
}))

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    getRequestErrorDetail: mocks.getRequestErrorDetail,
    getUpstreamErrorDetail: vi.fn(),
    listRequestErrorUpstreamErrors: mocks.listRequestErrorUpstreamErrors
  }
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({ showError: vi.fn() })
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

describe('OpsErrorDetailModal', () => {
  beforeEach(() => {
    mocks.getRequestErrorDetail.mockReset()
    mocks.listRequestErrorUpstreamErrors.mockReset()
    mocks.listRequestErrorUpstreamErrors.mockResolvedValue({ items: [] })
  })

  it('prioritizes upstream root cause and deduplicates diagnostic payloads', async () => {
    mocks.getRequestErrorDetail.mockResolvedValue({
      id: 1,
      created_at: '2026-08-19T00:00:00Z',
      phase: 'request',
      type: 'upstream_error',
      error_owner: 'provider',
      error_source: 'gateway',
      severity: 'P1',
      status_code: 502,
      upstream_status_code: 429,
      platform: 'openai',
      model: 'gpt-5.6',
      resolved: false,
      request_id: 'rid-1',
      message: 'All available accounts exhausted',
      error_body: '{"error":"same"}',
      upstream_error_message: 'provider rate limit exhausted',
      upstream_error_detail: '{"error":"same"}',
      upstream_errors: '[]',
      account_name: 'account',
      group_name: 'group',
      is_business_limited: false
    })

    const wrapper = shallowMount(OpsErrorDetailModal, {
      props: { show: true, errorId: 1, errorType: 'request' },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /></div>' },
          Icon: true
        }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('provider rate limit exhausted')
    expect(wrapper.text()).toContain('admin.ops.errorDetail.upstreamStatus')
    expect(wrapper.text()).toContain('429')
    expect(wrapper.findAll('pre')).toHaveLength(2)
    expect(wrapper.text()).not.toContain('admin.ops.errorDetail.payloads.upstream_detail')
  })

  it('shows router correlation ids and every upstream account hop', async () => {
    mocks.getRequestErrorDetail.mockResolvedValue({
      id: 2,
      created_at: '2026-11-27T03:11:21Z',
      phase: 'routing',
      type: 'no_available_accounts',
      error_owner: 'platform',
      error_source: 'gateway',
      severity: 'P1',
      status_code: 503,
      platform: 'gemini',
      model: 'gemini-4-argon',
      resolved: false,
      request_id: '832b79b9-2ace-4bc0-b905-94ecb7ee2452',
      client_request_id: 'd4d9b6ff-d7e1-41e4-b6a2-5603b2b56ee0',
      router_request_id: '7dca1e02-c9a3-4ece-97ba-c49242ea4c6e',
      router_user_id: 'u-2fb74c8f6df14d4d891d8d5bce273bff',
      account_id: 1150,
      account_name: 'hoyorouter argon',
      message: 'Service temporarily unavailable',
      error_body: '',
      upstream_errors: JSON.stringify([
        { at_unix_ms: 1795748000000, account_id: 1149, account_name: 'hoyo 1149', upstream_status_code: 429, upstream_request_id: 'req_abc', kind: 'failover' },
        { at_unix_ms: 1795748001000, account_id: 1150, account_name: 'hoyo 1150', upstream_status_code: 429, kind: 'retry_exhausted', message: 'quota' }
      ]),
      is_business_limited: false
    })

    const wrapper = shallowMount(OpsErrorDetailModal, {
      props: { show: true, errorId: 2, errorType: 'request' },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, Icon: true } }
    })
    await flushPromises()

    const router = wrapper.get('[data-testid="error-detail-router"]')
    expect(router.text()).toContain('u-2fb74c8f6df14d4d891d8d5bce273bff')
    expect(router.text()).toContain('7dca1e02-c9a3-4ece-97ba-c49242ea4c6e')
    expect(router.text()).toContain('d4d9b6ff-d7e1-41e4-b6a2-5603b2b56ee0')
    expect(router.text()).toContain('#1150')
    expect(router.text()).not.toContain('admin.ops.errorDetail.router.missing')
    const rows = wrapper.get('[data-testid="error-detail-attempts"]').findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('#1149')
    expect(rows[0].text()).toContain('req_abc')
    expect(rows[1].text()).toContain('quota')
  })
})
