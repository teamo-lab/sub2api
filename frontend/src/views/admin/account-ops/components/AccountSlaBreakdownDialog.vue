<template>
  <dialog
    ref="dialogRef"
    class="account-sla-dialog rounded-2xl bg-white p-0 text-gray-900 shadow-xl dark:bg-dark-900 dark:text-white"
    :aria-labelledby="titleId"
    v-bind="{ closedby: 'any' }"
    @close="onNativeClose"
  >
    <div class="flex items-start justify-between gap-4 border-b border-gray-200 px-5 py-4 dark:border-dark-700">
      <div class="min-w-0">
        <h2 :id="titleId" class="text-base font-semibold">{{ t('admin.accountOps.breakdown.title') }}</h2>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ scopeLabel }}</p>
      </div>
      <div class="flex shrink-0 items-center gap-2">
        <button
          type="button"
          class="btn btn-secondary flex h-9 w-9 items-center justify-center p-0"
          :title="t('common.refresh')"
          :aria-label="t('common.refresh')"
          :disabled="loading"
          @click="load"
        >
          <Icon name="refresh" size="sm" :class="loading ? 'animate-spin' : ''" />
        </button>
        <form method="dialog">
          <button
            type="submit"
            class="flex h-9 w-9 items-center justify-center rounded-xl text-gray-400 hover:bg-gray-100 hover:text-gray-600 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 dark:hover:bg-dark-700 dark:hover:text-dark-300"
            :aria-label="t('common.close')"
            autofocus
          >
            <Icon name="x" size="md" />
          </button>
        </form>
      </div>
    </div>

    <div class="space-y-3 px-5 py-4">
      <p v-if="errorMessage" class="rounded border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-900/20 dark:text-red-300">
        {{ errorMessage }}
      </p>
      <p v-if="data?.latency_degraded" class="text-xs text-amber-600 dark:text-amber-400">{{ t('admin.accountOps.breakdown.latencyDegraded') }}</p>
      <p v-if="data?.truncated" class="text-xs text-amber-600 dark:text-amber-400">{{ t('admin.accountOps.breakdown.truncated', { count: rows.length }) }}</p>

      <div
        class="account-sla-table-wrapper rounded-lg border border-gray-200 dark:border-dark-700"
        role="region"
        tabindex="0"
        :aria-labelledby="captionId"
      >
        <table class="account-sla-table w-full text-sm">
          <caption :id="captionId" class="px-3 py-2 text-left text-xs text-gray-500 dark:text-gray-400">
            {{ loading && !data ? t('common.loading') : t('admin.accountOps.breakdown.caption', { count: rows.length }) }}
          </caption>
          <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-800 dark:text-gray-400">
            <tr>
              <th
                v-for="column in columns"
                :key="column.key"
                scope="col"
                :class="column.key === 'account' ? 'text-left' : 'text-right'"
                :aria-sort="sortKey === column.key ? sortDirection : undefined"
              >
                <button type="button" class="sort-button" :class="column.key === 'account' ? 'justify-start' : 'justify-end'" @click="toggleSort(column.key)">
                  {{ column.label }}
                  <svg class="sort-icon" viewBox="0 0 10 10" aria-hidden="true" focusable="false">
                    <path class="up" d="M5 1 8.5 4.5h-7z" fill="currentColor" />
                    <path class="down" d="M5 9 1.5 5.5h7z" fill="currentColor" />
                  </svg>
                </button>
              </th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
            <tr v-for="row in sortedRows" :key="row.account_id" class="hover:bg-gray-50 dark:hover:bg-dark-800/60">
              <th scope="row" class="row-header bg-white text-left font-normal dark:bg-dark-900">
                <div class="flex min-w-0 flex-col gap-0.5">
                  <button
                    v-if="row.account_id > 0"
                    type="button"
                    class="account-name text-left font-medium text-primary-600 hover:underline dark:text-primary-400"
                    :title="row.account_name"
                    @click="emit('select-account', row.account_id)"
                  >
                    {{ row.account_name || `#${row.account_id}` }}
                  </button>
                  <span v-else class="account-name font-medium text-gray-700 dark:text-gray-200">{{ t('admin.accountOps.breakdown.unassigned') }}</span>
                  <span class="font-mono text-xs text-gray-400">
                    <template v-if="row.account_id > 0">#{{ row.account_id }}<template v-if="row.status"> · {{ row.status }}</template></template>
                    <template v-else>{{ t('admin.accountOps.breakdown.unassignedHint') }}</template>
                  </span>
                  <span v-if="row.rate_multiplier !== null" class="font-mono text-xs text-gray-500 dark:text-gray-400" data-testid="account-rates">
                    {{ t('admin.accountOps.breakdown.costRate', { rate: formatMultiplier(row.rate_multiplier) }) }}
                    ·
                    {{ row.sell_rate_multiplier !== null
                      ? t('admin.accountOps.breakdown.sellRate', { rate: formatMultiplier(row.sell_rate_multiplier) })
                      : t('admin.accountOps.breakdown.sellRateGroup') }}
                  </span>
                </div>
              </th>
              <td class="text-right font-semibold" :class="slaClass(row.sla)">{{ formatSla(row.sla) }}</td>
              <td class="text-right">
                <div>{{ formatNumber(row.request_count_total) }}</div>
                <div class="text-xs text-gray-400">{{ t('admin.accountOps.breakdown.pvDetail', { ok: formatNumber(row.success_count), err: formatNumber(row.error_count_total) }) }}</div>
              </td>
              <td class="text-right">{{ formatMs(row.ttft_p50_ms) }}</td>
              <td class="text-right">{{ formatMs(row.ttft_p90_ms) }}</td>
              <td class="text-right">{{ formatMs(row.duration_p50_ms) }}</td>
              <td class="text-right">{{ formatMs(row.duration_p90_ms) }}</td>
              <td class="text-right" data-testid="account-cache-rate">{{ formatPercent(cacheRate(row)) }}</td>
              <td class="text-right">
                <div>{{ formatNumber(row.token_consumed) }}</div>
                <div class="text-xs text-gray-400">{{ t('admin.accountOps.breakdown.tokenDetail', { input: formatNumber(row.input_tokens), output: formatNumber(row.output_tokens), cache: formatNumber(row.cache_read_tokens + row.cache_creation_tokens) }) }}</div>
              </td>
            </tr>
            <tr v-if="!loading && data && rows.length === 0">
              <td :colspan="columns.length" class="py-8 text-center text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.breakdown.empty') }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.breakdown.hint') }}</p>
    </div>

    <p class="visually-hidden" role="status">{{ sortAnnouncement }}</p>
  </dialog>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { opsAPI, type OpsDashboardAccountBreakdownItem, type OpsDashboardAccountBreakdownResponse, type OpsDashboardFilterParams } from '@/api/admin/ops'
import { formatNumber } from '@/utils/format'
import { formatMultiplier } from '@/utils/formatters'

type SortKey = 'account' | 'sla' | 'pv' | 'ttft_p50' | 'ttft_p90' | 'duration_p50' | 'duration_p90' | 'cache_rate' | 'tokens'
type SortDirection = 'ascending' | 'descending'

const props = defineProps<{
  show: boolean
  params: OpsDashboardFilterParams
  scopeLabel: string
}>()
const emit = defineEmits<{
  (e: 'update:show', value: boolean): void
  (e: 'select-account', accountId: number): void
}>()

const { t } = useI18n()
const uid = `account-sla-${useId()}`
const titleId = `${uid}-title`
const captionId = `${uid}-caption`

const dialogRef = ref<HTMLDialogElement | null>(null)
const data = ref<OpsDashboardAccountBreakdownResponse | null>(null)
const loading = ref(false)
const errorMessage = ref('')
const sortKey = ref<SortKey>('pv')
const sortDirection = ref<SortDirection>('descending')
const sortAnnouncement = ref('')
let controller: AbortController | null = null

const columns = computed<{ key: SortKey; label: string }[]>(() => [
  { key: 'account', label: t('admin.accountOps.breakdown.columns.account') },
  { key: 'sla', label: 'SLA' },
  { key: 'pv', label: 'PV' },
  { key: 'ttft_p50', label: 'TTFT P50' },
  { key: 'ttft_p90', label: 'TTFT P90' },
  { key: 'duration_p50', label: t('admin.accountOps.breakdown.columns.durationP50') },
  { key: 'duration_p90', label: t('admin.accountOps.breakdown.columns.durationP90') },
  { key: 'cache_rate', label: t('admin.accountOps.breakdown.columns.cacheRate') },
  { key: 'tokens', label: t('admin.accountOps.breakdown.columns.tokens') }
])

const rows = computed(() => data.value?.items ?? [])

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

function sortValue(row: OpsDashboardAccountBreakdownItem, key: SortKey): number | string | null {
  switch (key) {
    case 'account': return row.account_name || `#${row.account_id}`
    case 'sla': return row.sla
    case 'pv': return row.request_count_total
    case 'ttft_p50': return row.ttft_p50_ms
    case 'ttft_p90': return row.ttft_p90_ms
    case 'duration_p50': return row.duration_p50_ms
    case 'duration_p90': return row.duration_p90_ms
    case 'cache_rate': return cacheRate(row)
    case 'tokens': return row.token_consumed
  }
}

// Stable sort; empty values (no samples) always go last regardless of direction.
const sortedRows = computed(() => {
  const factor = sortDirection.value === 'ascending' ? 1 : -1
  return [...rows.value].sort((a, b) => {
    const va = sortValue(a, sortKey.value)
    const vb = sortValue(b, sortKey.value)
    if (va === null || vb === null) return va === vb ? 0 : va === null ? 1 : -1
    if (typeof va === 'string' || typeof vb === 'string') return factor * collator.compare(String(va), String(vb))
    return factor * (va - vb)
  })
})

function toggleSort(key: SortKey) {
  sortDirection.value = sortKey.value === key && sortDirection.value === 'ascending' ? 'descending' : 'ascending'
  sortKey.value = key
  const label = columns.value.find((column) => column.key === key)?.label ?? key
  sortAnnouncement.value = t('admin.accountOps.breakdown.sorted', {
    column: label,
    direction: t(`admin.accountOps.breakdown.${sortDirection.value}`),
    count: rows.value.length
  })
}

// Token-level cache rate, same formula as the channel monitor and the token trend chart:
// cache reads / every prompt token (uncached input + cache creation + cache reads).
// null when the account has no prompt tokens in the window.
function cacheRate(row: OpsDashboardAccountBreakdownItem): number | null {
  const promptTokens = row.input_tokens + row.cache_creation_tokens + row.cache_read_tokens
  return promptTokens > 0 ? row.cache_read_tokens / promptTokens : null
}

function formatPercent(value: number | null): string {
  return value === null ? '-' : `${(value * 100).toFixed(2)}%`
}

function formatSla(value: number | null): string {
  return formatPercent(value)
}

function slaClass(value: number | null): string {
  if (value === null) return 'text-gray-400'
  return value >= 0.99 ? 'text-emerald-600 dark:text-emerald-400' : 'text-red-600 dark:text-red-400'
}

function formatMs(value: number | null): string {
  return typeof value === 'number' ? `${formatNumber(value)} ms` : '-'
}

async function load() {
  controller?.abort()
  const current = new AbortController()
  controller = current
  loading.value = true
  errorMessage.value = ''
  try {
    const next = await opsAPI.getDashboardAccountBreakdown(props.params, { signal: current.signal })
    if (controller === current) data.value = next
  } catch (error: any) {
    if (error?.name === 'CanceledError' || error?.name === 'AbortError') return
    if (controller === current) errorMessage.value = error?.response?.data?.detail || t('admin.accountOps.breakdown.loadFailed')
  } finally {
    if (controller === current) loading.value = false
  }
}

function openDialog() {
  const dialog = dialogRef.value
  if (!dialog || dialog.open) return
  // showModal() gives the inert page, focus containment, Esc and focus return.
  if (typeof dialog.showModal === 'function') dialog.showModal()
  else dialog.setAttribute('open', '')
}

function closeDialog() {
  const dialog = dialogRef.value
  if (!dialog?.open) return
  if (typeof dialog.close === 'function') dialog.close()
  else dialog.removeAttribute('open')
}

function onNativeClose() {
  controller?.abort()
  if (props.show) emit('update:show', false)
}

// One watcher for both sources: the parent hands over a fresh params object on every
// refresh (auto refresh, filter change) while the dialog is open, so this reloads once per change.
watch(() => [props.show, props.params] as const, async ([show]) => {
  await nextTick()
  if (show) {
    openDialog()
    void load()
  } else {
    closeDialog()
  }
})

onMounted(() => {
  if (props.show) {
    openDialog()
    void load()
  }
})

onUnmounted(() => controller?.abort())
</script>

<style scoped>
.account-sla-dialog {
  inline-size: min(72rem, calc(100vw - 2rem));
  max-block-size: calc(100dvh - 2rem);
  border: 0;
}

.account-sla-dialog:modal {
  /* Keep scroll gestures in the dialog from chaining to the page behind it. */
  overscroll-behavior: contain;
}

.account-sla-dialog::backdrop {
  background: rgb(15 23 42 / 0.5);
  /* ::backdrop needs to be a scroll container for overscroll-behavior to apply. */
  overflow: hidden;
  overscroll-behavior: contain;
}

.account-sla-table-wrapper {
  overflow: auto;
  max-block-size: min(60vh, 40rem);
}

.account-sla-table-wrapper:focus-visible {
  outline: 2px solid rgb(59 130 246);
  outline-offset: 2px;
}

.account-sla-table {
  min-inline-size: 66rem;
  border-collapse: separate;
  border-spacing: 0;
}

.account-sla-table thead {
  position: sticky;
  inset-block-start: 0;
  z-index: 2;
}

.account-sla-table th,
.account-sla-table td {
  padding: 0.625rem 0.75rem;
  white-space: nowrap;
  vertical-align: top;
}

/* The header button supplies the padding of sortable headers. */
.account-sla-table thead th:has(> button) {
  padding: 0;
}

.account-sla-table .row-header {
  position: sticky;
  inset-inline-start: 0;
  z-index: 1;
}

.sort-button {
  display: flex;
  inline-size: 100%;
  min-block-size: 2.25rem;
  align-items: center;
  gap: 0.25rem;
  padding: 0.5rem 0.75rem;
  font-weight: 600;
}

.sort-button:focus-visible {
  outline: 2px solid rgb(59 130 246);
  outline-offset: -2px;
}

.sort-icon {
  inline-size: 0.625rem;
  block-size: 0.625rem;
  flex-shrink: 0;
}

.sort-icon path {
  opacity: 0.3;
}

th[aria-sort='ascending'] .sort-icon .up,
th[aria-sort='descending'] .sort-icon .down {
  opacity: 1;
}

th[aria-sort='ascending'] .sort-icon .down,
th[aria-sort='descending'] .sort-icon .up {
  opacity: 0;
}

.account-name {
  display: block;
  max-inline-size: 16rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.visually-hidden {
  position: absolute;
  inline-size: 1px;
  block-size: 1px;
  overflow: hidden;
  clip-path: inset(50%);
  white-space: nowrap;
}
</style>
