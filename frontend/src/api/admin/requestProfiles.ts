import { apiClient } from '../client'
export interface ProfileSpan {turn?:number;id:number;name:string;start_us:number;end_us:number;attempt?:number;account_id?:number;incomplete?:boolean}
export interface ProfileEvent {origin?:string;source_account_id?:number;rejected_account_id?:number;wait_us?:number;turn?:number;at_us:number;kind:string;attempt?:number;account_id?:number;status?:number}
export interface ProfileSegment {turn?:number;name:string;start_us:number;duration_us:number;attempt?:number;account_id?:number}
export interface RequestProfile {concurrent_upstreams?:boolean;client_outcome?:string;client_disconnected_us?:number;downstream_first_output_us?:number;downstream_complete_us?:number;downstream_error_us?:number;downstream_first_write_us?:number;downstream_write_error_us?:number;drain_after_disconnect_us?:number;delivery_observation_supported?:boolean;evidence?:string;notes?:string[];version:number;total_us:number;spans:ProfileSpan[];events:ProfileEvent[];segments:ProfileSegment[];dropped:number;attempts:number;group_id?:number;model?:string;protocol?:string;body_bytes?:number}
export interface ProfileRow {id:number;created_at:string;request_id:string;client_request_id:string;account_id:number;account_name:string;group_name:string;status:number;profile:RequestProfile}
export interface ProfileResult {options?:{kind:string;value:string;label:string}[];recording_group_ids?:number[];retention_hours?:number;recording_enabled?:boolean;rows:ProfileRow[];summary:{count:number;mean_us:number;p90_us:number;truncated:number;retries:number;fallbacks:number;local_reselect:number;stages:{name:string;mean_us:number}[]};page:number;limit:number;health?:{profile_queue_bytes?:number;profile_queue_byte_limit?:number;profile_rejected_count?:number;dropped_count:number;write_failed_count:number;queue_depth:number}}
export async function getRequestProfiles(params:Record<string,string|number>,signal?:AbortSignal):Promise<ProfileResult>{const {data}=await apiClient.get('/admin/ops/request-profiles',{params,signal});return data}
export const stageNames:Record<string,string>={unattributed:'未归因',body_read:'读取正文',body_decompress:'正文解压',json_normalize:'JSON 归一化',json_validate:'JSON 校验',authentication:'鉴权',security_audit:'安全审计',user_queue:'用户排队',account_queue:'账号排队',account_select:'选择账号',prepare:'请求准备',bootstrap:'Bootstrap 检查',protocol_convert:'协议转换',upstream_headers:'等待响应头',connection_wait:'获取连接',dns:'DNS',connect:'建立连接',tls:'TLS 握手',request_write:'发送请求',upstream_wait:'等待 HTTP 首字节',response_body:'响应流处理',retry_backoff:'重试退避'}
export const eventNames:Record<string,string>={retry:'同账号重试',fallback:'切换账号',attempt_start:'开始尝试',upstream_response:'上游响应头',network_error:'网络错误',client_cancelled:'客户端取消',upstream_first_byte:'上游首字节',first_semantic:'首个有效输出',stream_error:'流内错误'}
export function duration(us:number):string {if(us>=1e6)return `${(us/1e6).toFixed(2)} s`;if(us>=1000)return `${(us/1000).toFixed(1)} ms`;return `${Math.round(us)} µs`}
export function stageColor(name:string):string {if(name==='post_disconnect')return '#cbd5e1';if(name==='unattributed'||name.includes('before_audit')||name.includes('before_forward'))return '#94a3b8';if(name==='historical_after_first_output')return '#14b8a6';if(name==='historical_101_first_output')return '#6366f1';if(name==='historical_43_to_101'||name==='historical_first_output_relay')return '#3b82f6';if(name.includes('queue')||name.includes('backoff'))return '#f59e0b';if(name==='response_body_before_output'||name==='response_body_no_output')return '#f59e0b';if(name==='response_body_output_unknown')return '#94a3b8';if(name==='response_body'||name==='response_body_after_output')return '#14b8a6';if(name.includes('upstream'))return '#6366f1';if(['dns','connect','tls','connection_wait','request_write'].includes(name))return '#3b82f6';return '#8b5cf6'}

Object.assign(stageNames,{legacy_normalize:"旧协议入口归一化",tool_schema:"工具 Schema 处理",historical_before_audit:"入口到审计前（原因未细分）",historical_before_forward:"审计到转发前（原因未细分）",historical_43_to_101:"43 转发到 101 入口",historical_101_before_audit:"101 入口到审计前",historical_101_before_forward:"101 审计到转发前",historical_101_first_output:"101 转发到首输出",historical_first_output_relay:"首输出回传差",historical_after_first_output:"首输出之后"})

Object.assign(stageNames,{bootstrap_automation:"自动化 Bootstrap",bootstrap_delegation:"委派 Bootstrap"}); Object.assign(eventNames,{upstream_error:"上游错误"})

Object.assign(stageNames,{request_prepare:"转发准备（含未细分处理）"})

Object.assign(stageNames,{websocket_connect:"WebSocket 建连"});Object.assign(eventNames,{turn_start:"开始新一轮请求"})

Object.assign(stageNames,{websocket_turn:"WebSocket 本轮处理"});Object.assign(eventNames,{turn_complete:"本轮完成",turn_attempt_error:"本轮尝试异常"})

Object.assign(eventNames,{local_reselect:"本机准入重选",local_reselect_admitted:"重选后准入成功"})

Object.assign(stageNames,{websocket_write:"WebSocket 请求写入"});Object.assign(eventNames,{websocket_connected:"WebSocket 已连接"})

Object.assign(stageNames,{json_decode:"JSON 解码",json_serialize:"JSON 序列化",json_patch:"JSON 字段调整",request_build:"构造上游请求",credential_load:"凭据准备"})

Object.assign(eventNames,{timeout:"上游超时",upstream_cancelled:"上游请求取消"})

Object.assign(eventNames,{first_semantic:'上游首语义观测（非交付）',client_disconnected:'入口连接取消',downstream_first_write:'下行首次写入（可能是心跳）',downstream_first_flush:'下行首次 flush（可能是心跳）',downstream_first_output:'有效输出已写出',downstream_complete:'完成事件已写出',downstream_error:'错误事件已写出',downstream_write_error:'下行写入失败'})
export const clientOutcomeNames:Record<string,string>={unverified:'交付结果未核验',disconnected:'入口连接已取消',write_failed:'下行写入失败',completion_written:'完成事件已写出',error_written:'错误事件已写出'}

Object.assign(stageNames,{post_disconnect:"断连后后台处理"})

export function profileViewport(segments:ProfileSegment[],total:number,disconnected?:number):ProfileSegment[]{
 const result:ProfileSegment[]=[]
 const add=(segment:ProfileSegment)=>{if(segment.duration_us<=0)return;const last=result[result.length-1];if(segment.name==='post_disconnect'&&last?.name==='post_disconnect'&&last.start_us+last.duration_us===segment.start_us){last.duration_us+=segment.duration_us}else result.push(segment)}
 for(const s of segments){const end=Math.min(total,s.start_us+s.duration_us);if(end<=s.start_us)continue
  if(disconnected!=null&&disconnected<end){const cut=Math.max(s.start_us,disconnected);add({...s,duration_us:cut-s.start_us});add({name:'post_disconnect',start_us:cut,duration_us:end-cut})}
  else add({...s,duration_us:end-s.start_us})
 }
 return result
}
export function bodySize(bytes:number):string {if(bytes<1024)return `${bytes} B`;if(bytes<1e6)return `${(bytes/1024).toFixed(1)} KB`;return `${(bytes/1e6).toFixed(2)} MB`}

Object.assign(stageNames, {response_body_before_output:'响应流 · 首有效输出前',response_body_after_output:'响应流 · 首有效输出后',response_body_no_output:'响应流 · 未观测到有效输出',response_body_output_unknown:'响应体处理 · 输出边界未知'})
