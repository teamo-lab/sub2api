import { describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'

import AccountTableFilters from '../AccountTableFilters.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

const SelectStub = defineComponent({
  name: 'Select',
  props: ['modelValue', 'options', 'searchable'],
  emits: ['update:modelValue', 'change'],
  template: '<div class="select-stub" />'
})

const mountFilters = (filters: Record<string, unknown>, models: string[]) =>
  mount(AccountTableFilters, {
    props: { searchQuery: '', filters, groups: [], models },
    global: { stubs: { Select: SelectStub, SearchInput: true } }
  })

const modelSelect = (wrapper: ReturnType<typeof mountFilters>) =>
  wrapper.findAllComponents(SelectStub).find(select =>
    (select.props('options') as Array<{ value: string }>).some(option => option.value === 'gemini-3.8-flash')
  )!

describe('AccountTableFilters model filter', () => {
  it('lists mapped models after an "all models" option and emits the chosen model', async () => {
    const wrapper = mountFilters({ platform: 'gemini', model: '' }, ['gemini-3.1-pro', 'gemini-3.8-flash'])
    const select = modelSelect(wrapper)

    expect(select.props('options')).toEqual([
      { value: '', label: 'admin.accounts.allModels' },
      { value: 'gemini-3.1-pro', label: 'gemini-3.1-pro' },
      { value: 'gemini-3.8-flash', label: 'gemini-3.8-flash' }
    ])

    select.vm.$emit('update:modelValue', 'gemini-3.8-flash')
    const emitted = wrapper.emitted('update:filters')
    expect(emitted?.[0]?.[0]).toMatchObject({ platform: 'gemini', model: 'gemini-3.8-flash' })
  })

  it('keeps the current model selectable when it is not in the new option list', () => {
    const wrapper = mountFilters({ platform: 'openai', model: 'gemini-3.8-flash' }, ['gpt-6'])
    const select = modelSelect(wrapper)

    expect((select.props('options') as Array<{ value: string }>).map(option => option.value)).toEqual([
      '',
      'gemini-3.8-flash',
      'gpt-6'
    ])
  })
})
