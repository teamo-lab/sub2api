<template>
  <section class="rounded-xl border border-gray-200 bg-gray-50/60 p-4 dark:border-dark-600 dark:bg-dark-800/40" aria-labelledby="cooldown-heading" data-testid="pro-cooldown-policy">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h3 id="cooldown-heading" class="font-semibold text-gray-900 dark:text-white">冷却策略</h3>
        <p class="mt-1 text-sm text-gray-500">P90 持续恶化时临时退出调度，到期恢复；每轮最多冷却一个账号。</p>
      </div>
      <label class="flex shrink-0 items-center gap-2 text-sm">
        <input type="checkbox" :checked="modelValue.enabled" :disabled="isControl || !enrolled" @change="set('enabled', ($event.target as HTMLInputElement).checked)" aria-label="启用冷却策略" class="rounded border-gray-300 text-primary-600" />
        {{ isControl ? '仅观察' : '启用' }}
      </label>
    </div>
    <div class="mt-3 flex flex-wrap gap-2 text-xs">
      <span class="rounded-full bg-white px-3 py-1 text-gray-700 ring-1 ring-gray-200 dark:bg-dark-700 dark:text-gray-200">{{ enrolled ? (isControl ? '对照组 · 不主动冷却' : '实验组 · 按策略冷却') : '尚未加入运行试验' }}</span>
      <span class="rounded-full bg-white px-3 py-1 text-gray-700 ring-1 ring-gray-200 dark:bg-dark-700 dark:text-gray-200">观察窗口 30 分钟</span>
      <span class="rounded-full bg-white px-3 py-1 text-gray-700 ring-1 ring-gray-200 dark:bg-dark-700 dark:text-gray-200">单次冷却 30 分钟</span>
    </div>
    <p class="mt-3 text-xs text-gray-500">运行中的试验在保存后下一轮检查生效（最长约 5 分钟）。调参会记录为新阶段；对照组不主动冷却，试验结束后不再执行。</p>
    <div class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
      <label v-for="field in cooldownFields.slice(0, 8)" :key="field.key" class="block">
        <span class="input-label">{{ field.label }}</span>
        <input :value="modelValue[field.key]" @input="set(field.key, ($event.target as HTMLInputElement).value === '' ? NaN : Number(($event.target as HTMLInputElement).value))" type="number" :min="field.min" :max="field.max" :step="field.step" :aria-label="field.label" class="input" />
      </label>
    </div>
    <details class="mt-4 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
      <summary class="cursor-pointer text-sm font-medium">容量与可用性保护</summary>
      <div class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
        <label v-for="field in cooldownFields.slice(8)" :key="field.key" class="block">
          <span class="input-label">{{ field.label }}</span>
          <input :value="modelValue[field.key]" @input="set(field.key, ($event.target as HTMLInputElement).value === '' ? NaN : Number(($event.target as HTMLInputElement).value))" type="number" :min="field.min" :max="field.max" :step="field.step" :aria-label="field.label" class="input" />
        </label>
      </div>
      <label class="mt-4 flex items-center gap-2 text-sm"><input type="checkbox" :checked="modelValue.require_second_ip_verification" @change="set('require_second_ip_verification', ($event.target as HTMLInputElement).checked)" />要求第二 IP 验证</label>
      <p class="mt-1 text-xs text-gray-500">本次 9 小时试验已获授权省略；开启后将保守停止自动冷却，等待可验证的迁移证据。</p>
    </details>
    <p v-if="error" role="alert" class="mt-3 text-sm text-red-600">{{ error }}</p>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { cooldownFields, validateCooldownPolicy, type CooldownPolicy } from '@/utils/proCooldownPolicy'
const props = defineProps<{ modelValue: CooldownPolicy }>()
const emit = defineEmits<{ 'update:modelValue': [value: CooldownPolicy] }>()
const isControl = computed(() => props.modelValue.arm === 'control')
const enrolled = computed(() => typeof props.modelValue.trial_id === 'string' && !!props.modelValue.trial_id)
const error = computed(() => validateCooldownPolicy(props.modelValue))
function set(key: string, value: unknown) { emit('update:modelValue', { ...props.modelValue, [key]: value }) }
</script>
