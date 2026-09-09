import { mount,flushPromises } from '@vue/test-utils'
import { describe,it,expect,vi,beforeEach } from 'vitest'
import View from '../RequestProfilingView.vue'
import { getRequestProfiles,type ProfileResult } from '@/api/admin/requestProfiles'
vi.mock('@/api/admin/requestProfiles',async(importOriginal)=>({...await importOriginal<typeof import('@/api/admin/requestProfiles')>(),getRequestProfiles:vi.fn()}))
const sample:ProfileResult={page:1,limit:50,summary:{count:1,mean_us:1000,p90_us:1000,truncated:0,retries:1,fallbacks:1,local_reselect:0,stages:[{name:'body_read',mean_us:1000}]},rows:[{id:1,created_at:'2026-09-09T00:00:00Z',request_id:'local-request',client_request_id:'local-client',account_id:2,account_name:'Local QA',group_name:'Local group',status:200,profile:{version:1,evidence:'measured',total_us:1000,spans:[{id:1,name:'body_read',start_us:0,end_us:1000}],events:[{kind:'fallback',at_us:500,account_id:2}],segments:[{name:'body_read',start_us:0,duration_us:1000}],dropped:0,attempts:2,model:'gpt-6-astra',protocol:'sse'}}]}
const create=()=>mount(View,{global:{stubs:{AppLayout:{template:'<div><slot /></div>'}}}})
beforeEach(()=>{vi.mocked(getRequestProfiles).mockReset();vi.mocked(getRequestProfiles).mockResolvedValue(sample)})
describe('request profiling page',()=>{
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
