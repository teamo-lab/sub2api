import { mount,flushPromises } from '@vue/test-utils'
import { describe,it,expect,vi,beforeEach } from 'vitest'
import View from '../RequestProfilingView.vue'
import { getRequestProfiles,type ProfileResult } from '@/api/admin/requestProfiles'
vi.mock('@/api/admin/requestProfiles',async(importOriginal)=>({...await importOriginal<typeof import('@/api/admin/requestProfiles')>(),getRequestProfiles:vi.fn()}))
const sample:ProfileResult={page:1,limit:50,summary:{count:1,mean_us:1000,p90_us:1000,truncated:0,retries:1,fallbacks:1,local_reselect:0,stages:[{name:'body_read',mean_us:1000}]},rows:[{id:1,created_at:'2026-09-09T00:00:00Z',request_id:'local-request',client_request_id:'local-client',account_id:2,account_name:'Local QA',group_name:'Local group',status:200,profile:{version:1,evidence:'measured',total_us:1000,spans:[{id:1,name:'body_read',start_us:0,end_us:1000}],events:[{kind:'fallback',at_us:500,account_id:2}],segments:[{name:'body_read',start_us:0,duration_us:1000}],dropped:0,attempts:2,model:'gpt-6-astra',protocol:'sse'}}]}
const create=()=>mount(View,{global:{stubs:{AppLayout:{template:'<div><slot /></div>'}}}})
beforeEach(()=>{vi.mocked(getRequestProfiles).mockReset();vi.mocked(getRequestProfiles).mockResolvedValue(sample)})
describe('request profiling page',()=>{
 it('shows both output phases in aggregate and request detail without promising retry safety',async()=>{
  const segments=[{name:'response_body_before_output',start_us:0,duration_us:600},{name:'response_body_after_output',start_us:600,duration_us:400}]
  vi.mocked(getRequestProfiles).mockResolvedValue({...sample,summary:{...sample.summary,stages:segments.map(s=>({name:s.name,mean_us:s.duration_us}))},rows:[{...sample.rows[0]!,profile:{...sample.rows[0]!.profile,delivery_observation_supported:true,downstream_first_output_us:600,segments}}]})
  const w=create();await flushPromises()
  expect(w.text()).toContain('响应流 · 首有效输出前');expect(w.text()).toContain('响应流 · 首有效输出后')
  expect(w.text()).toContain('仍须满足协议状态、错误类型与重试预算')
  const before=w.findAll('button').find(b=>b.attributes('aria-label')==='响应流 · 首有效输出前 600 µs')!
  await before.trigger('click');expect(w.text()).toContain('每请求平均 600 µs');w.unmount()
 })

 it('filters by multiple names, keeps alternatives, and clears only that dimension',async()=>{
  vi.mocked(getRequestProfiles).mockResolvedValue({...sample,options:[
   {kind:'model',value:'astra',label:'Astra'},{kind:'model',value:'sol',label:'Sol'},
   {kind:'group',value:'38',label:'满智分组'},{kind:'account',value:'72',label:'Flux 渠道'}]})
  const w=create();await flushPromises()
  await w.get('button[aria-label="模型"]').trigger('click')
  const checks=w.findAll('input[type="checkbox"]');await checks[0]!.setValue(true);await flushPromises();await checks[1]!.setValue(true);await flushPromises()
  expect(vi.mocked(getRequestProfiles).mock.lastCall?.[0]).toMatchObject({models:'astra,sol'})
  expect(w.findAll('input[type="checkbox"]')).toHaveLength(2)
  await w.get('button[aria-label="分组"]').trigger('click')
  const group=w.findAll('label').find(l=>l.text()==='满智分组')!;await group.get('input').setValue(true);await flushPromises()
  expect(vi.mocked(getRequestProfiles).mock.lastCall?.[0]).toMatchObject({models:'astra,sol',group_ids:'38'})
  await w.findAll('button').find(b=>b.text()==='清空选择')!.trigger('click');await flushPromises()
  const params=vi.mocked(getRequestProfiles).mock.lastCall![0];expect(params.models).toBeUndefined();expect(params.group_ids).toBe('38');w.unmount()
 })
 it('keeps empty windows unknown rather than claiming zero latency',async()=>{
  vi.mocked(getRequestProfiles).mockResolvedValue({...sample,rows:[],summary:{...sample.summary,count:0,mean_us:0,p90_us:0,stages:[]}})
  const w=create();await flushPromises();expect(w.text()).toContain('没有已记录的轨迹');expect(w.text()).toContain('—');expect(w.text()).not.toContain('÷ 0');w.unmount()
 })
 it('sends event filters with a stable explicit protocol and evidence source',async()=>{
  const w=create();await flushPromises();const label=w.findAll('label').find(l=>l.text().includes('错误 / 重试类型'))!
  await label.find('select').setValue('fallback');await flushPromises()
  expect(vi.mocked(getRequestProfiles).mock.lastCall?.[0]).toMatchObject({error_type:'fallback',protocol:'sse',evidence:'measured',page:1});expect(w.text()).toContain('账号切换');w.unmount()
 })
 it('hides previous results on failed refresh and allows recovery',async()=>{
  const w=create();await flushPromises();vi.mocked(getRequestProfiles).mockRejectedValueOnce(new Error('统计读取失败'))
  await w.findAll('button').find(b=>b.text()==='刷新数据')!.trigger('click');await flushPromises()
  expect(w.find('[role="alert"]').text()).toContain('统计读取失败');expect(w.text()).not.toContain('local-client')
  await w.findAll('button').find(b=>b.text()==='重试加载')!.trigger('click');await flushPromises();expect(w.text()).toContain('local-client');w.unmount()
 })
})
