<template>
  <div class="pt-6">
    <div class="relative h-10 w-full rounded-lg bg-slate-100 dark:bg-slate-800" role="group" :aria-label="label">
      <button v-for="(s,i) in segments" :key="i" type="button" class="absolute inset-y-0 border-r border-white/30 transition hover:z-10 hover:outline hover:outline-2 hover:outline-slate-700 focus:z-10 focus:outline focus:outline-2"
        :style="{left:`${s.start_us/scale*100}%`,width:`${s.duration_us/scale*100}%`,backgroundColor:stageColor(s.name)}"
        :title="`${stageNames[s.name]||s.name} · ${duration(s.duration_us)} · ${duration(s.start_us)} 起${s.attempt?` · 尝试 ${s.attempt}`:''}`"
        :aria-label="`${stageNames[s.name]||s.name} ${duration(s.duration_us)}`" @click="$emit('select',s)" />
      <span v-for="(e,i) in highlights" :key="`event-${i}`" class="absolute -top-5 z-20 -translate-x-1/2 cursor-help text-lg font-bold" :style="{left:`${Math.min(99,e.at_us/scale*100)}%`,color:e.kind==='local_reselect'?'#2563eb':e.kind==='retry'?'#d97706':'#e11d48'}" :title="`${eventNames[e.kind]} · ${duration(e.at_us)}${e.account_id?` · 账号 #${e.account_id}`:''}`">◆</span>
    </div>
    <div class="mt-2 flex justify-between text-xs tabular-nums text-slate-500"><span>0</span><span>{{ duration(total) }}</span></div>
  </div>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { duration,stageNames,eventNames,stageColor,type ProfileSegment,type ProfileEvent } from '@/api/admin/requestProfiles'
const props=withDefaults(defineProps<{segments:ProfileSegment[];total:number;events?:ProfileEvent[];label?:string}>(),{events:()=>[],label:'请求耗时长条'})
defineEmits<{select:[segment:ProfileSegment]}>()
const scale=computed(()=>Math.max(props.total,1))
const highlights=computed(()=>props.events.filter(e=>e.kind==='retry'||e.kind==='fallback'||e.kind==='local_reselect'||e.kind==='client_disconnected'||e.kind==='downstream_write_error'))
</script>
