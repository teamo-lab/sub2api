<template>
  <div ref="root" class="relative min-w-0" @keydown.esc.stop.prevent="close">
    <span class="text-sm">{{ label }}</span>
    <button ref="trigger" type="button" class="input mt-1 flex w-full items-center justify-between gap-2 text-left" :aria-label="label" :aria-expanded="open" @click="open = !open">
      <span class="truncate" :class="modelValue.length ? '' : 'text-slate-400'">{{ summary }}</span><span aria-hidden="true">⌄</span>
    </button>
    <div v-if="open" class="absolute left-0 right-0 z-40 mt-2 rounded-xl border border-slate-200 bg-white p-3 shadow-xl dark:border-slate-700 dark:bg-slate-900" :aria-label="`${label}选项`">
      <input v-model="search" class="input w-full" :aria-label="`搜索${label}`" :placeholder="`搜索${label}`" />
      <div class="flex items-center justify-between py-3 text-xs text-slate-500"><span>已选 {{ modelValue.length }} 项</span><button type="button" class="text-teal-700 disabled:opacity-40 dark:text-teal-300" :disabled="!modelValue.length" @click="emit('update:modelValue', [])">清空选择</button></div>
      <div class="max-h-60 space-y-1 overflow-y-auto">
        <label v-for="option in filtered" :key="option.value" class="flex cursor-pointer items-start gap-2 rounded-lg p-2 text-sm hover:bg-slate-50 dark:hover:bg-slate-800">
          <input type="checkbox" class="mt-0.5 accent-teal-600" :checked="modelValue.includes(option.value)" :disabled="!modelValue.includes(option.value) && modelValue.length >= 100" @change="toggle(option.value)" /><span class="break-all">{{ option.label }}</span>
        </label>
        <p v-if="!filtered.length" class="p-3 text-sm text-slate-500">{{ search ? '没有匹配项' : '当前时间及协议范围内暂无选项' }}</p>
      </div>
      <p class="mt-2 text-xs text-slate-400">未选择时包含全部；最多选择 100 项。</p>
    </div>
  </div>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
const props = defineProps<{ label: string; placeholder: string; modelValue: string[]; options: {value:string;label:string}[] }>()
const emit = defineEmits<{ 'update:modelValue': [value: string[]] }>()
const open = ref(false), search = ref(''), root = ref<HTMLElement>(), trigger = ref<HTMLButtonElement>()
const filtered = computed(() => props.options.filter(o => o.label.toLocaleLowerCase().includes(search.value.toLocaleLowerCase())))
const summary = computed(() => {
  if (!props.modelValue.length) return props.placeholder
  const first = props.options.find(o => o.value === props.modelValue[0])?.label || '已选项'
  return props.modelValue.length === 1 ? first : `${first} 等 ${props.modelValue.length} 项`
})
function toggle(value:string) { emit('update:modelValue', props.modelValue.includes(value) ? props.modelValue.filter(v => v !== value) : [...props.modelValue, value]) }
function close() { open.value = false; trigger.value?.focus() }
function outside(e:PointerEvent) { if (!root.value?.contains(e.target as Node)) open.value = false }
onMounted(() => document.addEventListener('pointerdown', outside))
onBeforeUnmount(() => document.removeEventListener('pointerdown', outside))
</script>
