<template>
  <div v-if="available" class="grid grid-cols-1 gap-6 lg:grid-cols-2">
    <div class="card p-6">
      <div class="flex items-start justify-between gap-4">
        <div>
          <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('usage.errorStats.title') }}</h3>
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('usage.errorStats.hint') }}</p>
        </div>
        <div class="text-right">
          <div class="text-2xl font-semibold" :class="rateClass" data-testid="failure-rate">
            {{ stats ? formatRate(stats.failure_rate) : '-' }}
          </div>
          <div class="text-xs text-gray-500 dark:text-gray-400">
            {{ t('usage.errorStats.failedOfTotal', { failed: stats?.failed ?? 0, total: stats?.total ?? 0 }) }}
          </div>
        </div>
      </div>

      <div class="mt-5">
        <div class="mb-2 text-xs font-medium text-gray-700 dark:text-gray-300">{{ t('usage.errorStats.byStatus') }}</div>
        <div v-if="loading && !stats" class="py-6 text-center text-sm text-gray-400">{{ t('common.loading') }}</div>
        <div v-else-if="!stats || stats.by_status.length === 0" class="py-6 text-center text-sm text-gray-400">
          {{ t('usage.errorStats.noFailures') }}
        </div>
        <ul v-else class="space-y-2">
          <li v-for="item in stats.by_status" :key="item.status_code" class="flex items-center gap-3 text-sm">
            <span class="w-12 shrink-0 font-mono text-gray-900 dark:text-gray-100">{{ item.status_code }}</span>
            <div class="h-2 flex-1 overflow-hidden rounded bg-gray-100 dark:bg-dark-700">
              <div class="h-full rounded bg-red-400 dark:bg-red-500" :style="{ width: barWidth(item.count) }"></div>
            </div>
            <span class="w-14 shrink-0 text-right tabular-nums text-gray-700 dark:text-gray-300">{{ item.count }}</span>
          </li>
        </ul>
      </div>

      <p v-if="excludedText" class="mt-4 text-xs text-gray-500 dark:text-gray-400">{{ excludedText }}</p>
    </div>

    <TrendLineChartCard
      :title="t('usage.errorStats.trendTitle')"
      :labels="labels"
      :series="series"
      :loading="loading"
      primary-format="number"
      :tooltip-footer="tooltipFooter"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import TrendLineChartCard, { type TrendLineSeries } from '@/components/charts/TrendLineChartCard.vue'
import { usageAPI } from '@/api'
import type { UserErrorStats, UserErrorStatsParams } from '@/api/usage'

const props = defineProps<{
  params: UserErrorStatsParams
}>()

const { t } = useI18n()

const stats = ref<UserErrorStats | null>(null)
const loading = ref(false)
// 监控关闭(接口 404)或时间范围超过上限时整块不显示,避免给出误导性的 0%。
const available = ref(true)
let seq = 0
let controller: AbortController | null = null

const load = async () => {
  const current = ++seq
  controller?.abort()
  controller = new AbortController()
  loading.value = true
  try {
    const res = await usageAPI.getMyErrorStats(props.params, { signal: controller.signal })
    if (current !== seq) return
    stats.value = res
    available.value = true
  } catch (error: any) {
    if (current !== seq || error?.name === 'AbortError' || error?.code === 'ERR_CANCELED') return
    stats.value = null
    available.value = false
  } finally {
    if (current === seq) loading.value = false
  }
}

watch(() => props.params, load, { deep: true, immediate: true })

const formatRate = (rate: number) => `${(rate * 100).toFixed(2)}%`

const rateClass = computed(() => {
  const rate = stats.value?.failure_rate ?? 0
  if (rate >= 0.05) return 'text-red-600 dark:text-red-400'
  if (rate >= 0.01) return 'text-amber-600 dark:text-amber-400'
  return 'text-emerald-600 dark:text-emerald-400'
})

const maxStatusCount = computed(() => Math.max(1, ...(stats.value?.by_status ?? []).map((item) => item.count)))
const barWidth = (count: number) => `${Math.max(2, (count / maxStatusCount.value) * 100)}%`

const excludedText = computed(() => {
  const excluded = stats.value?.excluded
  if (!excluded || (excluded.business_limited === 0 && excluded.client_closed === 0)) return ''
  return t('usage.errorStats.excluded', {
    limited: excluded.business_limited,
    closed: excluded.client_closed
  })
})

const labels = computed(() => (stats.value?.trend ?? []).map((item) => item.bucket))
const series = computed<TrendLineSeries[]>(() => {
  const trend = stats.value?.trend ?? []
  return [
    { label: t('usage.errorStats.failedRequests'), data: trend.map((item) => item.failed), color: '#ef4444', fill: true },
    {
      label: t('usage.errorStats.failureRate'),
      data: trend.map((item) => (item.total > 0 ? (item.failed / item.total) * 100 : 0)),
      color: '#f59e0b',
      dashed: true,
      axis: 'percent',
      format: 'percent'
    }
  ]
})

const tooltipFooter = (dataIndex: number) => {
  const point = stats.value?.trend[dataIndex]
  return point ? t('usage.errorStats.failedOfTotal', { failed: point.failed, total: point.total }) : ''
}
</script>
