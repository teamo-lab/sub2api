<template>
  <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
    <TrendLineChartCard
      :title="t('dashboard.qualityTrend.duration')"
      :labels="labels"
      :series="durationSeries"
      :loading="loading"
      primary-format="milliseconds"
    />
    <TrendLineChartCard
      :title="t('dashboard.qualityTrend.sla')"
      :labels="labels"
      :series="slaSeries"
      :loading="loading"
      primary-format="percent"
      :tooltip-footer="slaFooter"
    />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import TrendLineChartCard, { type TrendLineSeries } from '@/components/charts/TrendLineChartCard.vue'
import type { QualityTrendPoint } from '@/types'

const props = withDefaults(defineProps<{
  points: QualityTrendPoint[]
  loading?: boolean
}>(), {
  loading: false
})

const { t } = useI18n()

const labels = computed(() => props.points.map((point) => point.bucket))

const durationSeries = computed<TrendLineSeries[]>(() => [
  {
    label: 'P50',
    data: props.points.map((point) => point.duration_p50_ms ?? null),
    color: '#10b981',
    format: 'milliseconds'
  },
  {
    label: 'P90',
    data: props.points.map((point) => point.duration_p90_ms ?? null),
    color: '#f59e0b',
    format: 'milliseconds'
  }
])

const slaSeries = computed<TrendLineSeries[]>(() => [{
  label: 'SLA',
  data: props.points.map((point) => (point.sla == null ? null : point.sla * 100)),
  color: '#10b981',
  fill: true,
  format: 'percent'
}])

function slaFooter(dataIndex: number): string {
  const point = props.points[dataIndex]
  return point ? t('dashboard.qualityTrend.slaFooter', { success: point.success, failed: point.failed }) : ''
}
</script>
