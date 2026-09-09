<template>
 <section class="mt-5" aria-label="按账号尝试分组">
  <div class="flex flex-wrap items-center justify-between gap-2"><h3 class="font-semibold">账号 / 渠道阶段</h3><span class="text-xs text-slate-500">{{ row.profile.attempts }} 次尝试 · 切换次数 {{ count===null?'未知':`${count} 次` }}</span></div>
  <p class="mt-1 text-xs text-slate-500">以每次上游尝试开始为分界；下一次尝试前的退避和准备计入前一段。下方各条分别放大，起止时间均相对请求入口。</p>
  <p v-if="!groups.length" class="mt-3 rounded-lg bg-slate-50 p-3 text-sm text-slate-500">此轨迹缺少完整的串行尝试边界，保留上方原始时间线，不推定账号分段。</p>
  <div v-else>
   <div class="mt-3 flex h-3 overflow-hidden rounded" aria-label="账号阶段总览">
    <div v-for="(g,i) in groups" :key="g.key" class="border-r-2 border-white" :style="{width:`${(g.end-g.start)/Math.max(total,1)*100}%`,backgroundColor:colors[i%colors.length]}" :title="`${g.kind} · ${g.accountName}${g.accountId?` (#${g.accountId})`:''} · ${duration(g.start)} → ${duration(g.end)}`" />
   </div>
   <article v-for="(g,i) in groups" :key="g.key" class="mt-3 rounded-xl border border-slate-200 border-l-4 p-4 dark:border-slate-700" :style="{borderLeftColor:colors[i%colors.length]}" :aria-label="g.attempt?`第 ${g.attempt} 次尝试 · ${g.accountName} · 渠道 ID ${g.accountId}`:'入口准备'">
    <header class="flex flex-wrap justify-between gap-2 text-sm">
     <div><b>{{ g.attempt?`第 ${g.attempt} 次尝试`:'入口准备' }}</b><span v-if="g.attempt" class="ml-2 rounded bg-slate-100 px-2 py-1 text-xs text-slate-600">{{ g.kind }}</span><p v-if="g.accountId" class="mt-2 break-words">{{ g.accountName }} <span class="text-slate-500">· 渠道 ID {{ g.accountId }}</span></p></div>
     <div class="text-right tabular-nums"><b>{{ duration(g.end-g.start) }}</b><p class="mt-1 text-xs text-slate-500">{{ duration(g.start) }} → {{ duration(g.end) }}</p></div>
    </header>
    <ProfileTimeline :segments="g.segments" :total="g.end-g.start" :label="g.attempt?`第 ${g.attempt} 次尝试耗时`:'入口准备耗时'" @select="selectSegment(g.key,g.start,$event)" />
    <p v-if="selection?.key===g.key" class="mt-2 rounded bg-indigo-50 p-2 text-xs text-indigo-800">{{ stageNames[selection.segment.name]||selection.segment.name }} · {{ duration(selection.segment.duration_us) }} · 请求内 {{ duration(selection.segment.start_us) }} → {{ duration(selection.segment.start_us+selection.segment.duration_us) }}</p>
   </article>
  </div>
 </section>
</template>
<script setup lang="ts">
import {computed,ref,watch} from 'vue'
import ProfileTimeline from './ProfileTimeline.vue'
import {stageNames,duration,profileAttemptGroups,profileSwitchCount,type ProfileRow,type ProfileSegment} from '@/api/admin/requestProfiles'
const props=defineProps<{row:ProfileRow;total:number}>()
const emit=defineEmits<{select:[segment:ProfileSegment]}>()
const groups=computed(()=>profileAttemptGroups(props.row,props.total))
const count=computed(()=>profileSwitchCount(props.row.profile))
const selection=ref<{key:string;segment:ProfileSegment}|null>(null)
function selectSegment(key:string,start:number,segment:ProfileSegment){const absolute={...segment,start_us:segment.start_us+start};selection.value={key,segment:absolute};emit('select',absolute)}
watch(()=>[props.row,props.total],()=>{selection.value=null})
const colors=['#94a3b8','#2563eb','#9333ea','#0d9488','#ea580c']
</script>

