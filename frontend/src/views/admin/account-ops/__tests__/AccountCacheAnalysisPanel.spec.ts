import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AccountCacheAnalysisPanel from '../components/AccountCacheAnalysisPanel.vue'
import type { OpsCacheAnalysisResponse } from '@/api/admin/ops'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key })
}))

const data: OpsCacheAnalysisResponse = {
  start_time: '',
  end_time: '',
  requests: 10,
  prompt_tokens: 1000,
  hit_tokens: 41,
  uncached_tokens: 959,
  saved_usd: 379.43,
  hit_rate: 0.041,
  reusable_hit_rate: 0.5,
  causes: [
    { cause: 'hit', requests: 1, tokens: 41 },
    { cause: 'incremental', requests: 0, tokens: 0 },
    { cause: 'single_turn', requests: 6, tokens: 792 },
    { cause: 'new_session', requests: 2, tokens: 73 },
    { cause: 'ttl_expired', requests: 1, tokens: 14 },
    { cause: 'upstream_miss', requests: 1, tokens: 80 }
  ],
  accounts: [
    {
      account_id: 1150,
      account_name: 'hoyorouter argon',
      prompt_tokens: 500,
      hit_tokens: 50,
      causes: [
        { cause: 'upstream_miss', requests: 1, tokens: 80 },
        { cause: 'hit', requests: 1, tokens: 50 },
        { cause: 'ttl_expired', requests: 1, tokens: 14 }
      ]
    }
  ]
}

describe('AccountCacheAnalysisPanel', () => {
  it('lists every non-empty cause with its token share and the worst accounts', async () => {
    const wrapper = mount(AccountCacheAnalysisPanel, { props: { data } })

    const legend = wrapper.get('[data-testid="cache-analysis-legend"]').findAll('li')
    expect(legend).toHaveLength(5) // incremental has 0 tokens and is skipped
    expect(legend[0].text()).toContain('admin.accountOps.cache.causes.hit')
    expect(legend[0].text()).toContain('4.1%')
    expect(legend[1].text()).toContain('79.2%')

    const row = wrapper.get('[data-testid="cache-analysis-accounts"]').get('tbody tr')
    expect(row.text()).toContain('#1150')
    expect(row.text()).toContain('10.0%')
    expect(row.text()).toContain('admin.accountOps.cache.causes.upstream_miss')
    expect(row.text()).not.toContain('admin.accountOps.cache.causes.hit')

    await row.get('button').trigger('click')
    expect(wrapper.emitted('select-account')?.[0]).toEqual([1150])
  })

  it('shows the empty state without data', () => {
    const wrapper = mount(AccountCacheAnalysisPanel, { props: { data: { ...data, prompt_tokens: 0, causes: [], accounts: null } } })
    expect(wrapper.text()).toContain('admin.accountOps.cache.empty')
  })
})
