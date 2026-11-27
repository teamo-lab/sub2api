<template>
  <section class="card p-5" aria-labelledby="account-cache-analysis-title" data-testid="cache-analysis-panel">
    <div class="flex flex-wrap items-baseline justify-between gap-2">
      <h2 id="account-cache-analysis-title" tabindex="-1" class="text-sm font-semibold uppercase tracking-wide text-gray-700 outline-none dark:text-gray-200">
        {{ t('admin.accountOps.cache.title') }}
      </h2>
      <p v-if="data && data.prompt_tokens > 0" class="text-xs text-gray-500 dark:text-gray-400" data-testid="cache-analysis-summary">
        {{ t('admin.accountOps.cache.summary', { tokens: formatCompactNumber(data.uncached_tokens), amount: formatCurrency(data.saved_usd) }) }}
      </p>
    </div>

    <p v-if="errorMessage" class="mt-3 text-sm text-red-600 dark:text-red-400" role="alert">{{ errorMessage }}</p>
    <div v-else-if="loading && !data" class="mt-4 h-3 animate-pulse rounded-full bg-gray-100 dark:bg-dark-700" />
    <p v-else-if="!data || data.prompt_tokens <= 0" class="mt-3 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.cache.empty') }}</p>

    <template v-else>
      <!-- 比例条只是图示；同样的数字在下面的图例列表里完整给出，屏幕阅读器读列表即可。 -->
      <div class="mt-4 flex h-3 w-full overflow-hidden rounded-full bg-gray-100 dark:bg-dark-700" aria-hidden="true">
        <div
          v-for="segment in segments"
          :key="segment.cause"
          class="h-full"
          :class="causeColor[segment.cause]"
          :style="{ width: `${segment.share * 100}%` }"
          :title="`${causeLabel(segment.cause)} ${formatShare(segment.share)}`"
        />
      </div>

      <ul class="mt-3 flex flex-wrap gap-x-4 gap-y-2 text-xs" :aria-label="t('admin.accountOps.cache.barLabel')" data-testid="cache-analysis-legend">
        <li v-for="segment in segments" :key="segment.cause" class="flex items-center gap-1.5" :title="causeHint(segment.cause)">
          <span class="inline-block h-2.5 w-2.5 shrink-0 rounded-sm" :class="causeColor[segment.cause]" aria-hidden="true" />
          <span class="text-gray-700 dark:text-gray-200">{{ causeLabel(segment.cause) }}</span>
          <span class="font-semibold tabular-nums text-gray-900 dark:text-white">{{ formatShare(segment.share) }}</span>
          <span v-if="segment.requests > 0" class="text-gray-400">· {{ t('admin.accountOps.cache.requests', { count: formatNumber(segment.requests) }) }}</span>
        </li>
      </ul>

      <details class="mt-3 text-xs text-gray-500 dark:text-gray-400">
        <summary class="cursor-pointer select-none">{{ t('admin.accountOps.cache.legendTitle') }}</summary>
        <dl class="mt-2 grid grid-cols-1 gap-x-6 gap-y-1 sm:grid-cols-2">
          <div v-for="segment in segments" :key="segment.cause" class="flex gap-2">
            <dt class="shrink-0 font-medium text-gray-700 dark:text-gray-200">{{ causeLabel(segment.cause) }}</dt>
            <dd>{{ causeHint(segment.cause) }}</dd>
          </div>
        </dl>
        <p class="mt-2">{{ t('admin.accountOps.cache.hint') }}</p>
      </details>

      <div v-if="accounts.length" class="mt-5 overflow-x-auto">
        <table class="min-w-full text-left text-sm" data-testid="cache-analysis-accounts">
          <caption class="pb-2 text-left">
            <span class="block text-xs font-semibold uppercase tracking-wide text-gray-700 dark:text-gray-200">{{ t('admin.accountOps.cache.accountsTitle') }}</span>
            <span class="block text-xs font-normal text-gray-500 dark:text-gray-400">{{ t('admin.accountOps.cache.accountsCaption') }}</span>
          </caption>
          <thead class="text-xs text-gray-500 dark:text-gray-400">
            <tr>
              <th scope="col" class="py-1.5 pr-4 font-medium">{{ t('admin.accountOps.cache.accountColumns.account') }}</th>
              <th scope="col" class="py-1.5 pr-4 text-right font-medium">{{ t('admin.accountOps.cache.accountColumns.promptTokens') }}</th>
              <th scope="col" class="py-1.5 pr-4 text-right font-medium">{{ t('admin.accountOps.cache.accountColumns.hitRate') }}</th>
              <th scope="col" class="py-1.5 font-medium">{{ t('admin.accountOps.cache.accountColumns.topCauses') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
            <tr v-for="account in accounts" :key="account.account_id">
              <th scope="row" class="py-2 pr-4 font-normal">
                <button
                  type="button"
                  class="text-left text-primary-600 hover:underline dark:text-primary-400"
                  @click="emit('select-account', account.account_id)"
                >
                  <span class="font-mono text-gray-500 dark:text-gray-400">#{{ account.account_id }}</span>
                  {{ account.account_name }}
                </button>
              </th>
              <td class="py-2 pr-4 text-right tabular-nums">{{ formatCompactNumber(account.prompt_tokens) }}</td>
              <td class="py-2 pr-4 text-right tabular-nums">{{ account.prompt_tokens > 0 ? formatShare(account.hit_tokens / account.prompt_tokens) : '-' }}</td>
              <td class="py-2 text-xs">
                <span v-for="cause in topMissCauses(account)" :key="cause.cause" class="mr-3 inline-flex items-center gap-1">
                  <span class="inline-block h-2 w-2 rounded-sm" :class="causeColor[cause.cause]" aria-hidden="true" />
                  {{ causeLabel(cause.cause) }} {{ formatShare(cause.tokens / account.prompt_tokens) }}
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { OpsCacheAnalysisAccountStat, OpsCacheAnalysisResponse, OpsCacheCause } from '@/api/admin/ops'
import { formatCompactNumber, formatCurrency, formatNumber } from '@/utils/format'

const props = defineProps<{
  data: OpsCacheAnalysisResponse | null
  loading?: boolean
  errorMessage?: string
}>()

const emit = defineEmits<{ (e: 'select-account', id: number): void }>()
const { t } = useI18n()

// 颜色按“谁的问题”分组：绿=命中，灰=请求本身不可缓存，琥珀/橙=客户端或会话侧，紫/红/蓝=路由或上游侧。
const causeColor: Record<OpsCacheCause, string> = {
  hit: 'bg-emerald-500',
  incremental: 'bg-emerald-200 dark:bg-emerald-800',
  single_turn: 'bg-slate-400 dark:bg-slate-500',
  below_min: 'bg-slate-300 dark:bg-slate-600',
  no_breakpoint: 'bg-orange-400',
  new_session: 'bg-amber-500',
  state_lost: 'bg-yellow-400',
  ttl_expired: 'bg-violet-500',
  account_switch: 'bg-rose-500',
  prefix_drift: 'bg-sky-500',
  upstream_miss: 'bg-red-700',
  unanalyzed: 'bg-gray-300 dark:bg-dark-500'
}

const segments = computed(() => {
  const total = props.data?.prompt_tokens ?? 0
  if (!props.data || total <= 0) return []
  return props.data.causes
    .filter((item) => item.tokens > 0)
    .map((item) => ({ cause: item.cause, share: item.tokens / total, requests: item.requests }))
})

const accounts = computed(() => props.data?.accounts ?? [])

function topMissCauses(account: OpsCacheAnalysisAccountStat) {
  return account.causes
    .filter((item) => item.cause !== 'hit' && item.cause !== 'incremental' && item.tokens > 0)
    .slice(0, 3)
}

function causeLabel(cause: OpsCacheCause): string {
  return t(`admin.accountOps.cache.causes.${cause}`)
}

function causeHint(cause: OpsCacheCause): string {
  return t(`admin.accountOps.cache.causeHints.${cause}`)
}

function formatShare(value: number): string {
  if (!Number.isFinite(value)) return '-'
  return `${(value * 100).toFixed(1)}%`
}
</script>
