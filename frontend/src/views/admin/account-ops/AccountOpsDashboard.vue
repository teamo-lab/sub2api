<template>
  <AppLayout>
    <div class="space-y-6 pb-12">
      <section class="border-b border-gray-200 pb-5 dark:border-dark-700">
        <div class="flex flex-col gap-4 xl:flex-row xl:items-end xl:justify-between">
          <div>
            <h1 class="text-xl font-semibold text-gray-900 dark:text-white">{{ t('admin.accountOps.title') }}</h1>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.description') }}</p>
          </div>

          <div class="flex flex-wrap items-end gap-3">
            <label class="block min-w-[280px]">
              <span class="mb-1 block text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.selectAccount') }}</span>
              <Select
                :model-value="accountId"
                :options="accountOptions"
                :placeholder="t('admin.accountOps.selectAccount')"
                :search-placeholder="t('admin.accountOps.searchAccount')"
                :loading="accountsLoading"
                searchable
                remote
                class="w-full"
                @search="loadAccounts"
                @change="onAccountChange"
              />
            </label>

            <label class="block w-[190px]">
              <span class="mb-1 block text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.model') }}</span>
              <Select :model-value="model" :options="modelOptions" searchable @change="model = String($event || '')" />
            </label>

            <label class="block w-[130px]">
              <span class="mb-1 block text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.timeRange') }}</span>
              <Select :model-value="timeRange" :options="timeRangeOptions" @change="timeRange = String($event || '1h') as TimeRange" />
            </label>

            <label class="flex h-10 items-center gap-2 text-sm text-gray-600 dark:text-gray-300">
              <input v-model="autoRefresh" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
              {{ t('admin.accountOps.autoRefresh') }}
            </label>

            <button
              type="button"
              class="btn btn-secondary flex h-10 w-10 items-center justify-center p-0"
              :title="t('common.refresh')"
              :aria-label="t('common.refresh')"
              :disabled="loading || !accountId"
              @click="refreshAll"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
          </div>
        </div>

        <div v-if="selectedAccount" class="mt-3 flex flex-wrap items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
          <span class="rounded bg-gray-100 px-2 py-1 font-mono dark:bg-dark-800">#{{ selectedAccount.id }}</span>
          <span>{{ selectedAccount.platform }}</span>
          <span>{{ selectedAccount.status }}</span>
          <span v-if="lastUpdated">{{ t('admin.accountOps.lastUpdated', { time: lastUpdated.toLocaleTimeString() }) }}</span>
        </div>
      </section>

      <div v-if="!accountId" class="flex min-h-[420px] items-center justify-center border border-dashed border-gray-300 bg-white dark:border-dark-700 dark:bg-dark-900">
        <div class="text-center">
          <Icon name="chart" size="xl" class="mx-auto text-gray-400" />
          <p class="mt-3 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.selectAccountHint') }}</p>
        </div>
      </div>

      <template v-else>
        <div v-if="errorMessage" class="border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-900/20 dark:text-red-300">
          {{ errorMessage }}
        </div>

        <div class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          <button
            v-for="metric in metrics"
            :key="metric.key"
            type="button"
            :disabled="!metric.action"
            class="card min-h-[136px] p-5 text-left transition-colors"
            :class="metric.action ? 'hover:bg-gray-50 dark:hover:bg-dark-700/70' : 'cursor-default'"
            @click="metric.action?.()"
          >
            <div class="text-xs font-semibold text-gray-500 dark:text-gray-400">{{ metric.label }}</div>
            <div class="mt-3 text-2xl font-bold" :class="metric.valueClass">{{ metric.value }}</div>
            <div class="mt-3 flex flex-wrap gap-x-3 gap-y-1 text-xs text-gray-500 dark:text-gray-400">
              <span>{{ metric.detail }}</span>
              <span v-if="metric.secondary">{{ metric.secondary }}</span>
            </div>
          </button>
        </div>

        <div class="h-[380px]">
          <OpsThroughputTrendChart
            :points="snapshot?.throughput_trend?.points ?? []"
            :loading="loading"
            :time-range="timeRange"
            :fullscreen="false"
            @open-details="openRequestDetails"
          />
        </div>

        <TokenUsageTrend :trend-data="tokenTrend" :loading="loading" :show-cost="false" />

        <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
          <OpsErrorDistributionChart :data="errorDistribution" :loading="loading" @open-details="openErrorDetails('request')" />
          <OpsErrorTrendChart
            :points="snapshot?.error_trend?.points ?? []"
            :loading="loading"
            :time-range="timeRange"
            @open-request-errors="openErrorDetails('request')"
            @open-upstream-errors="openErrorDetails('upstream')"
          />
        </div>

        <div>
          <p class="mb-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.alertScopeHint') }}</p>
          <OpsAlertEventsCard
            :account-id="accountId"
            :time-range-filter="timeRange"
            :refresh-token="refreshToken"
          />
        </div>
      </template>

      <OpsErrorDetailsModal
        :show="showErrorDetails"
        :time-range="timeRange"
        :account-id="accountId"
        :model="model"
        :error-type="errorType"
        @update:show="showErrorDetails = $event"
        @open-error-detail="openErrorDetail"
      />
      <OpsErrorDetailModal v-model:show="showErrorDetail" :error-id="selectedErrorId" :error-type="errorType" />
      <OpsRequestDetailsModal
        v-model="showRequestDetails"
        :time-range="timeRange"
        :preset="requestPreset"
        :account-id="accountId"
        :model="model"
        @open-error-detail="openErrorDetail"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useDebounceFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import TokenUsageTrend from '@/components/charts/TokenUsageTrend.vue'
import { adminAPI } from '@/api'
import { opsAPI, type OpsDashboardSnapshotV2Response, type OpsErrorDistributionResponse } from '@/api/admin/ops'
import type { AccountListItem, TrendDataPoint } from '@/types'
import { formatNumber } from '@/utils/format'
import OpsThroughputTrendChart from '@/views/admin/ops/components/OpsThroughputTrendChart.vue'
import OpsErrorDistributionChart from '@/views/admin/ops/components/OpsErrorDistributionChart.vue'
import OpsErrorTrendChart from '@/views/admin/ops/components/OpsErrorTrendChart.vue'
import OpsAlertEventsCard from '@/views/admin/ops/components/OpsAlertEventsCard.vue'
import OpsErrorDetailsModal from '@/views/admin/ops/components/OpsErrorDetailsModal.vue'
import OpsErrorDetailModal from '@/views/admin/ops/components/OpsErrorDetailModal.vue'
import OpsRequestDetailsModal, { type OpsRequestDetailsPreset } from '@/views/admin/ops/components/OpsRequestDetailsModal.vue'

type TimeRange = '5m' | '30m' | '1h' | '6h' | '24h'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const accountId = ref<number | null>(parsePositiveInt(route.query.account_id))
const model = ref(typeof route.query.model === 'string' ? route.query.model : '')
const timeRange = ref<TimeRange>(isTimeRange(route.query.time_range) ? route.query.time_range : '1h')
const accounts = ref<AccountListItem[]>([])
const selectedAccount = ref<AccountListItem | null>(null)
const accountsLoading = ref(false)
const models = ref<string[]>([])
const snapshot = ref<OpsDashboardSnapshotV2Response | null>(null)
const errorDistribution = ref<OpsErrorDistributionResponse | null>(null)
const loading = ref(false)
const errorMessage = ref('')
const lastUpdated = ref<Date | null>(null)
const autoRefresh = ref(true)
const refreshToken = ref(0)
let accountSearchController: AbortController | null = null
let dashboardController: AbortController | null = null
let refreshTimer: number | null = null
let requestSequence = 0

const showErrorDetails = ref(false)
const showErrorDetail = ref(false)
const selectedErrorId = ref<number | null>(null)
const errorType = ref<'request' | 'upstream'>('request')
const showRequestDetails = ref(false)
const requestPreset = ref<OpsRequestDetailsPreset>({ title: '', kind: 'all', sort: 'created_at_desc' })

function parsePositiveInt(value: unknown): number | null {
  const parsed = Number.parseInt(String(value ?? ''), 10)
  return Number.isFinite(parsed) && parsed > 0 ? parsed : null
}

function isTimeRange(value: unknown): value is TimeRange {
  return ['5m', '30m', '1h', '6h', '24h'].includes(String(value))
}

const accountOptions = computed(() => {
  const items = selectedAccount.value && !accounts.value.some((item) => item.id === selectedAccount.value?.id)
    ? [selectedAccount.value, ...accounts.value]
    : accounts.value
  return items.map((account) => ({
    value: account.id,
    label: `${account.name} · #${account.id} · ${account.platform}`
  }))
})

const modelOptions = computed(() => [
  { value: '', label: t('admin.accountOps.allModels') },
  ...Array.from(new Set([...(model.value ? [model.value] : []), ...models.value])).map((value) => ({ value, label: value }))
])

const timeRangeOptions = computed(() => (['5m', '30m', '1h', '6h', '24h'] as const).map((value) => ({
  value,
  label: t(`admin.ops.timeRange.${value}`)
})))

function formatPercent(value: number | null | undefined): string {
  return `${((value ?? 0) * 100).toFixed(2)}%`
}

function formatMs(value: number | null | undefined): string {
  return typeof value === 'number' ? `${formatNumber(value)} ms` : '-'
}

const metrics = computed(() => {
  const overview = snapshot.value?.overview
  return [
    {
      key: 'requests', label: t('admin.accountOps.metrics.requests'), value: formatNumber(overview?.request_count_total ?? 0), valueClass: 'text-gray-900 dark:text-white',
      detail: t('admin.accountOps.metrics.successes', { count: formatNumber(overview?.success_count ?? 0) }),
      secondary: t('admin.accountOps.metrics.tokens', { count: formatNumber(overview?.token_consumed ?? 0) }), action: openRequestDetails
    },
    {
      key: 'sla', label: t('admin.accountOps.metrics.sla'), value: formatPercent(overview?.sla), valueClass: (overview?.sla ?? 1) >= 0.99 ? 'text-emerald-500' : 'text-red-500',
      detail: t('admin.accountOps.metrics.samples', { count: formatNumber(overview?.request_count_sla ?? 0) }), secondary: '', action: undefined
    },
    {
      key: 'request-errors', label: t('admin.accountOps.metrics.requestErrors'), value: formatPercent(overview?.error_rate), valueClass: (overview?.error_rate ?? 0) > 0.01 ? 'text-red-500' : 'text-emerald-500',
      detail: t('admin.accountOps.metrics.errors', { count: formatNumber(overview?.error_count_sla ?? 0) }), secondary: '', action: () => openErrorDetails('request')
    },
    {
      key: 'duration', label: t('admin.accountOps.metrics.duration'), value: formatMs(overview?.duration?.p99_ms), valueClass: 'text-gray-900 dark:text-white',
      detail: t('admin.accountOps.metrics.percentile', { p95: formatMs(overview?.duration?.p95_ms), avg: formatMs(overview?.duration?.avg_ms) }), secondary: '',
      action: () => openRequestDetails({ title: t('admin.accountOps.metrics.duration'), kind: 'success', sort: 'duration_desc' })
    },
    {
      key: 'ttft', label: t('admin.accountOps.metrics.ttft'), value: formatMs(overview?.ttft?.p99_ms), valueClass: 'text-gray-900 dark:text-white',
      detail: t('admin.accountOps.metrics.percentile', { p95: formatMs(overview?.ttft?.p95_ms), avg: formatMs(overview?.ttft?.avg_ms) }), secondary: '', action: undefined
    },
    {
      key: 'upstream-errors', label: t('admin.accountOps.metrics.upstreamErrors'), value: formatPercent(overview?.upstream_error_rate), valueClass: (overview?.upstream_error_rate ?? 0) > 0.01 ? 'text-red-500' : 'text-emerald-500',
      detail: t('admin.accountOps.metrics.errors', { count: formatNumber(overview?.upstream_error_count_excl_429_529 ?? 0) }),
      secondary: t('admin.accountOps.metrics.excluded'), action: () => openErrorDetails('upstream')
    }
  ]
})

const tokenTrend = computed<TrendDataPoint[]>(() => (snapshot.value?.throughput_trend?.points ?? []).map((point) => ({
  date: new Date(point.bucket_start).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }),
  requests: point.request_count,
  input_tokens: point.input_tokens ?? point.token_consumed,
  output_tokens: point.output_tokens ?? 0,
  cache_creation_tokens: point.cache_creation_tokens ?? 0,
  cache_read_tokens: point.cache_read_tokens ?? 0,
  total_tokens: point.token_consumed,
  cost: 0,
  actual_cost: 0
})))

async function loadAccounts(search = '') {
  accountSearchController?.abort()
  const controller = new AbortController()
  accountSearchController = controller
  accountsLoading.value = true
  try {
    const response = await adminAPI.accounts.list(1, 50, { search: search.trim(), lite: 'true' }, { signal: controller.signal })
    accounts.value = response.items ?? []
  } catch (error: any) {
    if (error?.name !== 'CanceledError' && error?.name !== 'AbortError') console.error('[AccountOps] account search failed', error)
  } finally {
    if (accountSearchController === controller) accountsLoading.value = false
  }
}

async function restoreSelectedAccount() {
  if (!accountId.value) return
  try {
    selectedAccount.value = await adminAPI.accounts.getById(accountId.value)
  } catch {
    accountId.value = null
  }
}

function onAccountChange(value: string | number | boolean | null) {
  const nextID = typeof value === 'number' ? value : parsePositiveInt(value)
  accountId.value = nextID
  selectedAccount.value = accounts.value.find((item) => item.id === nextID) ?? null
  model.value = ''
  if (nextID && !selectedAccount.value) void restoreSelectedAccount()
}

function buildParams() {
  return {
    time_range: timeRange.value,
    account_id: accountId.value,
    model: model.value || undefined,
    mode: 'raw' as const
  }
}

async function refreshAll() {
  if (!accountId.value) return
  dashboardController?.abort()
  const controller = new AbortController()
  dashboardController = controller
  const sequence = ++requestSequence
  loading.value = true
  errorMessage.value = ''
  try {
    const params = buildParams()
    const [nextSnapshot, nextDistribution, nextModels] = await Promise.all([
      opsAPI.getDashboardSnapshotV2(params, { signal: controller.signal }),
      opsAPI.getErrorDistribution(params, { signal: controller.signal }),
      opsAPI.getDashboardModels(params, { signal: controller.signal })
    ])
    if (sequence !== requestSequence) return
    snapshot.value = nextSnapshot
    errorDistribution.value = nextDistribution
    models.value = nextModels
    lastUpdated.value = new Date()
    refreshToken.value += 1
  } catch (error: any) {
    if (error?.name === 'CanceledError' || error?.name === 'AbortError') return
    if (sequence === requestSequence) errorMessage.value = error?.response?.data?.detail || t('admin.accountOps.loadFailed')
  } finally {
    if (sequence === requestSequence) loading.value = false
  }
}

const refreshDebounced = useDebounceFn(refreshAll, 120)
const syncQuery = useDebounceFn(() => {
  const query: Record<string, string> = {}
  if (accountId.value) query.account_id = String(accountId.value)
  if (model.value) query.model = model.value
  if (timeRange.value !== '1h') query.time_range = timeRange.value
  void router.replace({ query })
}, 120)

watch([accountId, model, timeRange], () => {
  syncQuery()
  refreshDebounced()
})

function openRequestDetails(preset?: OpsRequestDetailsPreset) {
  requestPreset.value = preset ?? { title: t('admin.accountOps.metrics.requests'), kind: 'all', sort: 'created_at_desc' }
  showRequestDetails.value = true
}

function openErrorDetails(type: 'request' | 'upstream') {
  errorType.value = type
  showErrorDetails.value = true
}

function openErrorDetail(id: number) {
  selectedErrorId.value = id
  showErrorDetail.value = true
}

onMounted(async () => {
  await Promise.all([loadAccounts(), restoreSelectedAccount()])
  await refreshAll()
  refreshTimer = window.setInterval(() => {
    if (autoRefresh.value && accountId.value && !document.hidden) void refreshAll()
  }, 30_000)
})

onUnmounted(() => {
  accountSearchController?.abort()
  dashboardController?.abort()
  if (refreshTimer !== null) window.clearInterval(refreshTimer)
})
</script>
