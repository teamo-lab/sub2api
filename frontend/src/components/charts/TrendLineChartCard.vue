<template>
  <div class="card p-4">
    <h3 class="mb-4 text-sm font-semibold text-gray-900 dark:text-white">{{ title }}</h3>
    <div v-if="loading" class="flex h-48 items-center justify-center">
      <LoadingSpinner />
    </div>
    <div v-else-if="hasData && chartData" class="h-48">
      <Line :data="chartData" :options="lineOptions" />
    </div>
    <div v-else class="flex h-48 items-center justify-center text-sm text-gray-500 dark:text-gray-400">
      {{ emptyText || t('admin.dashboard.noDataAvailable') }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Chart as ChartJS,
  CategoryScale,
  LinearScale,
  PointElement,
  LineElement,
  Tooltip,
  Legend,
  Filler
} from 'chart.js'
import { Line } from 'vue-chartjs'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'

ChartJS.register(CategoryScale, LinearScale, PointElement, LineElement, Tooltip, Legend, Filler)

export type TrendValueFormat = 'compact' | 'number' | 'percent' | 'milliseconds'

export interface TrendLineSeries {
  label: string
  data: Array<number | null>
  color: string
  fill?: boolean
  dashed?: boolean
  axis?: 'primary' | 'percent'
  format?: TrendValueFormat
}

const props = withDefaults(defineProps<{
  title: string
  labels: string[]
  series: TrendLineSeries[]
  loading?: boolean
  emptyText?: string
  primaryFormat?: TrendValueFormat
  tooltipFooter?: (dataIndex: number) => string
}>(), {
  loading: false,
  emptyText: '',
  primaryFormat: 'compact',
  tooltipFooter: undefined
})

const { t } = useI18n()
const isDarkMode = computed(() => document.documentElement.classList.contains('dark'))
const colors = computed(() => ({
  text: isDarkMode.value ? '#e5e7eb' : '#374151',
  grid: isDarkMode.value ? '#374151' : '#e5e7eb'
}))

const hasData = computed(() => props.series.some((item) => item.data.some((value) => value !== null)))
const hasPercentAxis = computed(() => props.series.some((item) => item.axis === 'percent'))

function formatValue(value: number, format: TrendValueFormat): string {
  if (format === 'percent') return `${value.toFixed(2)}%`
  if (format === 'milliseconds') return `${Math.round(value).toLocaleString()} ms`
  if (format === 'number') return value.toLocaleString()
  if (Math.abs(value) >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(2)}B`
  if (Math.abs(value) >= 1_000_000) return `${(value / 1_000_000).toFixed(2)}M`
  if (Math.abs(value) >= 1_000) return `${(value / 1_000).toFixed(2)}K`
  return value.toLocaleString()
}

const chartData = computed(() => ({
  labels: props.labels,
  datasets: props.series.map((item) => ({
    label: item.label,
    data: item.data,
    borderColor: item.color,
    backgroundColor: `${item.color}20`,
    fill: item.fill ?? false,
    borderDash: item.dashed ? [5, 5] : undefined,
    tension: 0.3,
    spanGaps: true,
    yAxisID: item.axis === 'percent' ? 'yPercent' : 'y'
  }))
}))

const lineOptions = computed(() => ({
  responsive: true,
  maintainAspectRatio: false,
  interaction: { intersect: false, mode: 'index' as const },
  plugins: {
    legend: {
      position: 'top' as const,
      labels: { color: colors.value.text, usePointStyle: true, pointStyle: 'circle', padding: 15, font: { size: 11 } }
    },
    tooltip: {
      callbacks: {
        label: (context: any) => {
          const item = props.series[context.datasetIndex]
          if (context.raw == null || !item) return `${context.dataset.label}: -`
          const format = item.format ?? (item.axis === 'percent' ? 'percent' : props.primaryFormat)
          return `${context.dataset.label}: ${formatValue(Number(context.raw), format)}`
        },
        footer: (items: any[]) => {
          const dataIndex = items[0]?.dataIndex
          return typeof dataIndex === 'number' ? props.tooltipFooter?.(dataIndex) || '' : ''
        }
      }
    }
  },
  scales: {
    x: {
      grid: { color: colors.value.grid },
      ticks: { color: colors.value.text, font: { size: 10 }, maxRotation: 0, autoSkip: true }
    },
    y: {
      grid: { color: colors.value.grid },
      ticks: {
        color: colors.value.text,
        font: { size: 10 },
        callback: (value: string | number) => formatValue(Number(value), props.primaryFormat)
      }
    },
    ...(hasPercentAxis.value ? {
      yPercent: {
        position: 'right' as const,
        min: 0,
        max: 100,
        grid: { drawOnChartArea: false },
        ticks: { color: colors.value.text, font: { size: 10 }, callback: (value: string | number) => `${value}%` }
      }
    } : {})
  }
}))
</script>
