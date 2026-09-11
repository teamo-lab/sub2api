<template>
  <TrendLineChartCard
    :title="t('admin.dashboard.tokenUsageTrend')"
    :labels="labels"
    :series="series"
    :loading="loading"
    primary-format="compact"
    :tooltip-footer="tooltipFooter"
  />
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import TrendLineChartCard, { type TrendLineSeries } from '@/components/charts/TrendLineChartCard.vue'
import type { TrendDataPoint } from '@/types'

const { t } = useI18n()

const props = withDefaults(defineProps<{
  trendData: TrendDataPoint[]
  loading?: boolean
  showCost?: boolean
}>(), {
  showCost: true
})

const labels = computed(() => props.trendData.map((item) => item.date))
const series = computed<TrendLineSeries[]>(() => [
  { label: 'Input', data: props.trendData.map((item) => item.input_tokens), color: '#3b82f6', fill: true },
  { label: 'Output', data: props.trendData.map((item) => item.output_tokens), color: '#10b981', fill: true },
  { label: 'Cache Creation', data: props.trendData.map((item) => item.cache_creation_tokens), color: '#f59e0b', fill: true },
  { label: 'Cache Read', data: props.trendData.map((item) => item.cache_read_tokens), color: '#06b6d4', fill: true },
  {
    label: 'Cache Hit Rate',
    data: props.trendData.map((item) => {
      const totalPromptTokens = item.input_tokens + item.cache_read_tokens + item.cache_creation_tokens
      return totalPromptTokens > 0 ? (item.cache_read_tokens / totalPromptTokens) * 100 : 0
    }),
    color: '#8b5cf6',
    dashed: true,
    axis: 'percent',
    format: 'percent'
  }
])

function tooltipFooter(dataIndex: number): string {
  if (!props.showCost) return ''
  const data = props.trendData[dataIndex]
  return data ? `Actual: $${formatCost(data.actual_cost)} | Standard: $${formatCost(data.cost)}` : ''
}

const formatCost = (value: number): string => {
  if (value >= 1000) {
    return (value / 1000).toFixed(2) + 'K'
  } else if (value >= 1) {
    return value.toFixed(2)
  } else if (value >= 0.01) {
    return value.toFixed(3)
  }
  return value.toFixed(4)
}
</script>
