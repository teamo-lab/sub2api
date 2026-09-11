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
            <label class="block min-w-[190px]">
              <span class="mb-1 block text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.selectGroup') }}</span>
              <Select :model-value="groupId" :options="groupOptions" searchable class="w-full" @change="onGroupChange" />
            </label>

            <label class="block min-w-[280px]">
              <span class="mb-1 block text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.selectAccount') }}</span>
              <Select
                :model-value="accountId"
                :options="accountOptions"
                :placeholder="t('admin.accountOps.selectAccountOptional')"
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
              <Select :model-value="timeRange" :options="timeRangeOptions" @change="onTimeRangeChange" />
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
              :disabled="loading || !hasScope"
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
        <div v-else-if="selectedGroup" class="mt-3 flex flex-wrap items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
          <span class="rounded bg-gray-100 px-2 py-1 font-mono dark:bg-dark-800">#{{ selectedGroup.id }}</span>
          <span>{{ selectedGroup.name }}</span>
          <span>{{ selectedGroup.platform }}</span>
          <span v-if="lastUpdated">{{ t('admin.accountOps.lastUpdated', { time: lastUpdated.toLocaleTimeString() }) }}</span>
        </div>
      </section>

      <div v-if="!hasScope" class="flex min-h-[420px] items-center justify-center border border-dashed border-gray-300 bg-white dark:border-dark-700 dark:bg-dark-900">
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
            :time-range="chartTimeRange"
            :fullscreen="false"
            @open-details="openRequestDetails"
          />
        </div>

        <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
          <TokenUsageTrend :trend-data="tokenTrend" :loading="loading" :show-cost="false" />
          <TrendLineChartCard
            :title="t('admin.accountOps.trends.sla')"
            :labels="trendLabels"
            :series="slaTrendSeries"
            :loading="loading"
            primary-format="percent"
          />
          <TrendLineChartCard
            :title="t('admin.accountOps.trends.pv')"
            :labels="trendLabels"
            :series="pvTrendSeries"
            :loading="loading"
            primary-format="number"
          />
          <TrendLineChartCard
            :title="t('admin.accountOps.trends.ttft')"
            :labels="trendLabels"
            :series="ttftTrendSeries"
            :loading="loading"
            primary-format="milliseconds"
          />
        </div>

        <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
          <OpsErrorDistributionChart :data="errorDistribution" :loading="loading" @open-details="openErrorDetails('request')" />
          <OpsErrorTrendChart
            :points="snapshot?.error_trend?.points ?? []"
            :loading="loading"
            :time-range="chartTimeRange"
            @open-request-errors="openErrorDetails('request')"
            @open-upstream-errors="openErrorDetails('upstream')"
          />
        </div>

        <div>
          <p class="mb-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.alertScopeHint') }}</p>
          <OpsAlertEventsCard
            :account-id="accountId"
            :group-id="groupId"
            :time-range-filter="timeRange"
            :custom-start-time="customStartTime"
            :custom-end-time="customEndTime"
            :refresh-token="refreshToken"
          />
        </div>
      </template>

      <OpsErrorDetailsModal
        :show="showErrorDetails"
        :time-range="timeRange"
        :custom-start-time="customStartTime"
        :custom-end-time="customEndTime"
        :group-id="groupId"
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
        :custom-start-time="customStartTime"
        :custom-end-time="customEndTime"
        :preset="requestPreset"
        :group-id="groupId"
        :account-id="accountId"
        :model="model"
        @open-error-detail="openErrorDetail"
      />

      <BaseDialog :show="showCustomTimeRangeDialog" :title="t('admin.ops.timeRange.custom')" width="narrow" @close="showCustomTimeRangeDialog = false">
        <div class="space-y-4">
          <label class="block">
            <span class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.customTimeRange.startTime') }}</span>
            <input v-model="customStartTimeInput" type="datetime-local" class="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-900 focus:border-blue-500 focus:outline-none focus:ring-1 focus:ring-blue-500 dark:border-dark-600 dark:bg-dark-800 dark:text-white" />
          </label>
          <label class="block">
            <span class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.customTimeRange.endTime') }}</span>
            <input v-model="customEndTimeInput" type="datetime-local" class="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-900 focus:border-blue-500 focus:outline-none focus:ring-1 focus:ring-blue-500 dark:border-dark-600 dark:bg-dark-800 dark:text-white" />
          </label>
          <div class="flex justify-end gap-3 pt-2">
            <button type="button" class="btn btn-secondary" @click="showCustomTimeRangeDialog = false">{{ t('common.cancel') }}</button>
            <button type="button" class="btn btn-primary" :disabled="!customRangeValid" @click="confirmCustomTimeRange">{{ t('common.confirm') }}</button>
          </div>
        </div>
      </BaseDialog>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useDebounceFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import TokenUsageTrend from '@/components/charts/TokenUsageTrend.vue'
import TrendLineChartCard, { type TrendLineSeries } from '@/components/charts/TrendLineChartCard.vue'
import { adminAPI } from '@/api'
import { opsAPI, type OpsDashboardFilterParams, type OpsDashboardSnapshotV2Response, type OpsErrorDistributionResponse } from '@/api/admin/ops'
import type { AccountListItem, TrendDataPoint } from '@/types'
import { formatNumber } from '@/utils/format'
import OpsThroughputTrendChart from '@/views/admin/ops/components/OpsThroughputTrendChart.vue'
import OpsErrorDistributionChart from '@/views/admin/ops/components/OpsErrorDistributionChart.vue'
import OpsErrorTrendChart from '@/views/admin/ops/components/OpsErrorTrendChart.vue'
import OpsAlertEventsCard from '@/views/admin/ops/components/OpsAlertEventsCard.vue'
import OpsErrorDetailsModal from '@/views/admin/ops/components/OpsErrorDetailsModal.vue'
import OpsErrorDetailModal from '@/views/admin/ops/components/OpsErrorDetailModal.vue'
import OpsRequestDetailsModal, { type OpsRequestDetailsPreset } from '@/views/admin/ops/components/OpsRequestDetailsModal.vue'

type TimeRange = '5m' | '30m' | '1h' | '6h' | '24h' | 'custom'
type GroupOption = { id: number; name: string; platform: string }

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const accountId = ref<number | null>(parsePositiveInt(route.query.account_id))
const groupId = ref<number | null>(parsePositiveInt(route.query.group_id))
const model = ref(typeof route.query.model === 'string' ? route.query.model : '')
const customStartTime = ref(parseISOTime(route.query.start_time))
const customEndTime = ref(parseISOTime(route.query.end_time))
const initialTimeRange = isTimeRange(route.query.time_range) ? route.query.time_range : '1h'
const timeRange = ref<TimeRange>(initialTimeRange === 'custom' && (!customStartTime.value || !customEndTime.value) ? '1h' : initialTimeRange)
const accounts = ref<AccountListItem[]>([])
const selectedAccount = ref<AccountListItem | null>(null)
const accountsLoading = ref(false)
const groups = ref<GroupOption[]>([])
const models = ref<string[]>([])
const snapshot = ref<OpsDashboardSnapshotV2Response | null>(null)
const errorDistribution = ref<OpsErrorDistributionResponse | null>(null)
const loading = ref(false)
const errorMessage = ref('')
const lastUpdated = ref<Date | null>(null)
const autoRefresh = ref(true)
const refreshToken = ref(0)
const showCustomTimeRangeDialog = ref(false)
const customStartTimeInput = ref('')
const customEndTimeInput = ref('')
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
  return ['5m', '30m', '1h', '6h', '24h', 'custom'].includes(String(value))
}

function parseISOTime(value: unknown): string | null {
  if (typeof value !== 'string' || !value) return null
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? null : parsed.toISOString()
}

const hasScope = computed(() => Boolean(accountId.value || groupId.value))
const selectedGroup = computed(() => groups.value.find((item) => item.id === groupId.value) ?? null)

const groupOptions = computed(() => [
  { value: null, label: t('admin.accountOps.noGroupFilter') },
  ...groups.value.map((group) => ({ value: group.id, label: `${group.name} · #${group.id} · ${group.platform}` }))
])

const accountOptions = computed(() => {
  const items = selectedAccount.value && !accounts.value.some((item) => item.id === selectedAccount.value?.id)
    ? [selectedAccount.value, ...accounts.value]
    : accounts.value
  return [{ value: null, label: t('admin.accountOps.noAccountFilter') }, ...items.map((account) => ({
    value: account.id,
    label: `${account.name} · #${account.id} · ${account.platform}`
  }))]
})

const modelOptions = computed(() => [
  { value: '', label: t('admin.accountOps.allModels') },
  ...Array.from(new Set([...(model.value ? [model.value] : []), ...models.value])).map((value) => ({ value, label: value }))
])

const timeRangeOptions = computed(() => [
  ...(['5m', '30m', '1h', '6h', '24h'] as const).map((value) => ({ value, label: t(`admin.ops.timeRange.${value}`) })),
  {
    value: 'custom',
    label: timeRange.value === 'custom' && customStartTime.value && customEndTime.value
      ? `${t('admin.ops.timeRange.custom')} (${formatCustomTimeRangeLabel(customStartTime.value, customEndTime.value)})`
      : t('admin.ops.timeRange.custom')
  }
])

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
  date: formatTrendLabel(point.bucket_start),
  requests: point.request_count,
  input_tokens: point.input_tokens ?? point.token_consumed,
  output_tokens: point.output_tokens ?? 0,
  cache_creation_tokens: point.cache_creation_tokens ?? 0,
  cache_read_tokens: point.cache_read_tokens ?? 0,
  total_tokens: point.token_consumed,
  cost: 0,
  actual_cost: 0
})))

const trendLabels = computed(() => (snapshot.value?.throughput_trend?.points ?? []).map((point) => formatTrendLabel(point.bucket_start)))

const pvTrendSeries = computed<TrendLineSeries[]>(() => [{
  label: t('admin.accountOps.trends.requests'),
  data: (snapshot.value?.throughput_trend?.points ?? []).map((point) => point.request_count),
  color: '#3b82f6',
  fill: true,
  format: 'number'
}])

const ttftTrendSeries = computed<TrendLineSeries[]>(() => [
  {
    label: 'P50',
    data: (snapshot.value?.throughput_trend?.points ?? []).map((point) => point.ttft_p50_ms ?? null),
    color: '#10b981',
    format: 'milliseconds'
  },
  {
    label: 'P90',
    data: (snapshot.value?.throughput_trend?.points ?? []).map((point) => point.ttft_p90_ms ?? null),
    color: '#f59e0b',
    format: 'milliseconds'
  }
])

const slaTrendSeries = computed<TrendLineSeries[]>(() => {
  const errorsByBucket = new Map((snapshot.value?.error_trend?.points ?? []).map((point) => [point.bucket_start, point]))
  return [{
    label: 'SLA',
    data: (snapshot.value?.throughput_trend?.points ?? []).map((point) => {
      const errors = errorsByBucket.get(point.bucket_start)
      const totalErrors = errors?.error_count_total ?? 0
      const slaErrors = errors?.error_count_sla ?? 0
      const successes = Math.max(0, point.request_count - totalErrors)
      const samples = successes + slaErrors
      return samples > 0 ? (successes / samples) * 100 : null
    }),
    color: '#10b981',
    fill: true,
    format: 'percent'
  }]
})

function formatTrendLabel(value: string): string {
  const date = new Date(value)
  const customSpan = customStartTime.value && customEndTime.value
    ? new Date(customEndTime.value).getTime() - new Date(customStartTime.value).getTime()
    : 0
  const includeDate = timeRange.value === '24h' || (timeRange.value === 'custom' && customSpan >= 24 * 60 * 60 * 1000)
  return date.toLocaleString([], includeDate
    ? { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }
    : { hour: '2-digit', minute: '2-digit' })
}

function formatCustomTimeRangeLabel(startTime: string, endTime: string): string {
  const format = (value: string) => new Date(value).toLocaleString([], {
    month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit'
  })
  return `${format(startTime)} ~ ${format(endTime)}`
}

function toLocalDateTimeInput(value: Date): string {
  return new Date(value.getTime() - value.getTimezoneOffset() * 60_000).toISOString().slice(0, 16)
}

const customRangeValid = computed(() => {
  if (!customStartTimeInput.value || !customEndTimeInput.value) return false
  const start = new Date(customStartTimeInput.value).getTime()
  const end = new Date(customEndTimeInput.value).getTime()
  return Number.isFinite(start) && Number.isFinite(end) && start < end
})

const chartTimeRange = computed(() => {
  if (timeRange.value !== 'custom' || !customStartTime.value || !customEndTime.value) return timeRange.value
  const span = new Date(customEndTime.value).getTime() - new Date(customStartTime.value).getTime()
  return span >= 24 * 60 * 60 * 1000 ? '24h' : '1h'
})

function onTimeRangeChange(value: string | number | boolean | null) {
  const next = String(value || '1h')
  if (next !== 'custom') {
    timeRange.value = isTimeRange(next) ? next : '1h'
    return
  }
  const end = customEndTime.value ? new Date(customEndTime.value) : new Date()
  const start = customStartTime.value ? new Date(customStartTime.value) : new Date(end.getTime() - 60 * 60 * 1000)
  customStartTimeInput.value = toLocalDateTimeInput(start)
  customEndTimeInput.value = toLocalDateTimeInput(end)
  showCustomTimeRangeDialog.value = true
}

function confirmCustomTimeRange() {
  if (!customRangeValid.value) return
  customStartTime.value = new Date(customStartTimeInput.value).toISOString()
  customEndTime.value = new Date(customEndTimeInput.value).toISOString()
  timeRange.value = 'custom'
  showCustomTimeRangeDialog.value = false
}

async function loadAccounts(search = '') {
  accountSearchController?.abort()
  const controller = new AbortController()
  accountSearchController = controller
  accountsLoading.value = true
  try {
    const response = await adminAPI.accounts.list(1, 50, {
      search: search.trim(),
      group: groupId.value ? String(groupId.value) : undefined,
      lite: 'true'
    }, { signal: controller.signal })
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

function onGroupChange(value: string | number | boolean | null) {
  groupId.value = typeof value === 'number' ? value : parsePositiveInt(value)
  accountId.value = null
  selectedAccount.value = null
  model.value = ''
  void loadAccounts()
}

function buildParams(): OpsDashboardFilterParams {
  const params: OpsDashboardFilterParams = {
    account_id: accountId.value,
    group_id: groupId.value,
    model: model.value || undefined,
    mode: 'raw' as const
  }
  if (timeRange.value === 'custom' && customStartTime.value && customEndTime.value) {
    params.start_time = customStartTime.value
    params.end_time = customEndTime.value
  } else {
    params.time_range = timeRange.value === 'custom' ? '1h' : timeRange.value
  }
  return params
}

async function refreshAll() {
  if (!hasScope.value) {
    snapshot.value = null
    errorDistribution.value = null
    return
  }
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
  if (groupId.value) query.group_id = String(groupId.value)
  if (model.value) query.model = model.value
  if (timeRange.value !== '1h') query.time_range = timeRange.value
  if (timeRange.value === 'custom' && customStartTime.value && customEndTime.value) {
    query.start_time = customStartTime.value
    query.end_time = customEndTime.value
  }
  void router.replace({ query })
}, 120)

watch([accountId, groupId, model, timeRange, customStartTime, customEndTime], () => {
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
  await Promise.all([
    loadAccounts(),
    restoreSelectedAccount(),
    adminAPI.groups.getAll().then((items) => {
      groups.value = items.map((item) => ({ id: item.id, name: item.name, platform: item.platform }))
    }).catch((error) => console.error('[AccountOps] group load failed', error))
  ])
  await refreshAll()
  refreshTimer = window.setInterval(() => {
    if (autoRefresh.value && hasScope.value && !document.hidden) void refreshAll()
  }, 30_000)
})

onUnmounted(() => {
  accountSearchController?.abort()
  dashboardController?.abort()
  if (refreshTimer !== null) window.clearInterval(refreshTimer)
})
</script>
