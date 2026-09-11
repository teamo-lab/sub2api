import {mount} from '@vue/test-utils'
import {describe,it,expect} from 'vitest'
import Groups from '../ProfileAttemptGroups.vue'
import {profileAttemptGroups,profileSwitchCount,type ProfileRow} from '@/api/admin/requestProfiles'
const row:ProfileRow={id:1,created_at:'',request_id:'qa',client_request_id:'qa',account_id:23,account_name:'Switchbase',attempt_accounts:{23:'Switchbase',27:'Zero'},group_name:'Harness',status:200,profile:{version:1,total_us:1000,attempts:3,dropped:0,protocol:'sse',evidence:'measured',spans:[],events:[{kind:'attempt_start',at_us:100,attempt:1,account_id:23},{kind:'retry',at_us:400,attempt:2,account_id:23},{kind:'attempt_start',at_us:400,attempt:2,account_id:23},{kind:'fallback',at_us:700,attempt:3,account_id:27},{kind:'attempt_start',at_us:700,attempt:3,account_id:27}],segments:[{name:'body_read',start_us:0,duration_us:100},{name:'response_body_before_output',start_us:100,duration_us:250},{name:'retry_backoff',start_us:350,duration_us:50},{name:'response_body_before_output',start_us:400,duration_us:600}]}}
describe('attempt timeline groups',()=>{
 it('separates same-account retry, preserves preparation and total elapsed',()=>{
  const g=profileAttemptGroups(row,1000)
  expect(g.map(x=>[x.attempt,x.accountName,x.start,x.end])).toEqual([[0,'入口准备',0,100],[1,'Switchbase',100,400],[2,'Switchbase',400,700],[3,'Zero',700,1000]])
  expect(g.flatMap(x=>x.segments).reduce((n,s)=>n+s.duration_us,0)).toBe(1000)
  expect(g[1]!.segments.at(-1)?.name).toBe('retry_backoff')
  expect(profileSwitchCount(row.profile)).toBe(2)
 })
 it('retains instantaneous attempts instead of silently dropping a retry',()=>{
  const p={...row.profile,events:row.profile.events.map(e=>e.attempt===2?{...e,at_us:700}:e)}
  const g=profileAttemptGroups({...row,profile:p},1000)
  expect(g.filter(x=>x.attempt>0)).toHaveLength(3)
  expect(g.find(x=>x.attempt===2)).toMatchObject({start:700,end:700,segments:[]})
 })
 it('clips zoom at the original absolute boundary without losing attribution',()=>{
  const g=profileAttemptGroups(row,550)
  expect(g.at(-1)).toMatchObject({attempt:2,start:400,end:550})
  expect(g.at(-1)!.segments[0]!.duration_us).toBe(150)
 })
 it('keeps incomplete, historical and parallel traces unknown',()=>{
  for(const patch of [{dropped:1},{evidence:'historical'},{concurrent_upstreams:true}]){
   const p={...row.profile,...patch};expect(profileSwitchCount(p)).toBeNull();expect(profileAttemptGroups({...row,profile:p},1000)).toEqual([])
  }
  expect(profileSwitchCount({...row.profile,events:[{kind:'local_reselect',at_us:1}]})).toBe(0)
 })
 it('labels every attempt with name and ID and selects absolute time',async()=>{
  const w=mount(Groups,{props:{row,total:1000}})
  expect(w.findAll('article')).toHaveLength(4);expect(w.text()).toContain('第 2 次尝试');expect(w.text()).toContain('同账号重试');expect(w.text()).toContain('Zero');expect(w.text()).toContain('渠道 ID 27')
  await w.findAll('article')[2]!.get('button').trigger('click')
  expect(w.emitted('select')?.[0]?.[0]).toMatchObject({start_us:400,duration_us:300});w.unmount()
 })
})
