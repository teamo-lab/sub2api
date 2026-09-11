import { mount } from '@vue/test-utils'
import { describe,it,expect } from 'vitest'
import ProfileTimeline from '../ProfileTimeline.vue'

describe('ProfileTimeline',()=>{
 it('preserves elapsed proportions and emits the clicked interval',async()=>{
  const segments=[{name:'body_read',start_us:0,duration_us:200},{name:'unattributed',start_us:200,duration_us:800}]
  const wrapper=mount(ProfileTimeline,{props:{segments,total:1000,events:[{kind:'fallback',at_us:500,account_id:2}]}})
  const bars=wrapper.findAll('button');expect(bars[0].attributes('style')).toContain('width: 20%');expect(bars[1].attributes('style')).toContain('left: 20%')
  await bars[0].trigger('click');expect(wrapper.emitted('select')?.[0]).toEqual([segments[0]])
  expect(wrapper.find('span[title]').attributes('style')).toContain('left: 50%')
 })
 it('renders an empty interval without NaN widths',()=>{
  const wrapper=mount(ProfileTimeline,{props:{segments:[],total:0}})
  expect(wrapper.findAll('button')).toHaveLength(0);expect(wrapper.html()).not.toContain('NaN')
 })
 it('distinguishes local admission reselection from upstream fallback',()=>{
  const wrapper=mount(ProfileTimeline,{props:{segments:[],total:1000,events:[{kind:'local_reselect',at_us:200},{kind:'fallback',at_us:500}]}})
  const markers=wrapper.findAll('span[title]');expect(markers[0].attributes('title')).toContain('本机准入重选');expect(markers[0].attributes('style')).not.toEqual(markers[1].attributes('style'))
 })
})

import {profileViewport} from '@/api/admin/requestProfiles'
it('separates background processing after disconnect without changing elapsed time',()=>{
 const result=profileViewport([{name:'body_read',start_us:0,duration_us:200},{name:'response_body',start_us:200,duration_us:800}],1000,600)
 expect(result.map(s=>[s.name,s.duration_us])).toEqual([['body_read',200],['response_body',400],['post_disconnect',400]])
 expect(result.reduce((n,s)=>n+s.duration_us,0)).toBe(1000)
 expect(profileViewport(result,600,600).reduce((n,s)=>n+s.duration_us,0)).toBe(600)
})

it('distinguishes observed reasoning on both sides of first output',async()=>{
 const segments=[{name:'reasoning_observed_before_output',start_us:0,duration_us:600},{name:'reasoning_observed_after_output',start_us:600,duration_us:400}]
 const w=mount(ProfileTimeline,{props:{segments,total:1000}})
 const bars=w.findAll('button');expect(bars[0]!.attributes('aria-label')).toContain('Reasoning · 首有效输出前');expect(bars[1]!.attributes('aria-label')).toContain('Reasoning · 首有效输出后')
 await bars[1]!.trigger('click');expect(w.emitted('select')?.[0]).toEqual([segments[1]]);w.unmount()
})
