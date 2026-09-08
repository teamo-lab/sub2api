import { apiClient } from '../client'
export interface ProfileSpan {turn?:number;id:number;name:string;start_us:number;end_us:number;attempt?:number;account_id?:number;incomplete?:boolean}
export interface ProfileEvent {turn?:number;at_us:number;kind:string;attempt?:number;account_id?:number;status?:number}
export interface ProfileSegment {turn?:number;name:string;start_us:number;duration_us:number;attempt?:number;account_id?:number}
export interface RequestProfile {evidence?:string;notes?:string[];version:number;total_us:number;spans:ProfileSpan[];events:ProfileEvent[];segments:ProfileSegment[];dropped:number;attempts:number;group_id?:number;model?:string;protocol?:string;body_bytes?:number}
export interface ProfileRow {id:number;created_at:string;request_id:string;client_request_id:string;account_id:number;account_name:string;group_name:string;status:number;profile:RequestProfile}
export interface ProfileResult {rows:ProfileRow[];summary:{count:number;mean_us:number;p90_us:number;truncated:number;retries:number;fallbacks:number;stages:{name:string;mean_us:number}[]};page:number;limit:number;health?:{dropped_count:number;write_failed_count:number;queue_depth:number}}
export async function getRequestProfiles(params:Record<string,string|number>,signal?:AbortSignal):Promise<ProfileResult>{const {data}=await apiClient.get('/admin/ops/request-profiles',{params,signal});return data}
export const stageNames:Record<string,string>={unattributed:'未归因',body_read:'读取正文',body_decompress:'正文解压',json_normalize:'JSON 归一化',json_validate:'JSON 校验',authentication:'鉴权',security_audit:'安全审计',user_queue:'用户排队',account_queue:'账号排队',account_select:'选择账号',prepare:'请求准备',bootstrap:'Bootstrap 检查',protocol_convert:'协议转换',upstream_headers:'等待响应头',connection_wait:'获取连接',dns:'DNS',connect:'建立连接',tls:'TLS 握手',request_write:'发送请求',upstream_wait:'等待上游首字节',response_body:'接收上游响应',retry_backoff:'重试退避'}
export const eventNames:Record<string,string>={retry:'同账号重试',fallback:'切换账号',attempt_start:'开始尝试',upstream_response:'上游响应头',network_error:'网络错误',client_cancelled:'客户端取消',upstream_first_byte:'上游首字节',first_semantic:'首个有效输出',stream_error:'流内错误'}
export function duration(us:number):string {if(us>=1e6)return `${(us/1e6).toFixed(2)} s`;if(us>=1000)return `${(us/1000).toFixed(1)} ms`;return `${Math.round(us)} µs`}
export function stageColor(name:string):string {if(name==='unattributed'||name.includes('before_audit')||name.includes('before_forward'))return '#94a3b8';if(name==='historical_after_first_output')return '#14b8a6';if(name==='historical_101_first_output')return '#6366f1';if(name==='historical_43_to_101'||name==='historical_first_output_relay')return '#3b82f6';if(name.includes('queue')||name.includes('backoff'))return '#f59e0b';if(name==='response_body')return '#14b8a6';if(name.includes('upstream'))return '#6366f1';if(['dns','connect','tls','connection_wait','request_write'].includes(name))return '#3b82f6';return '#8b5cf6'}

Object.assign(stageNames,{legacy_normalize:"旧协议入口归一化",tool_schema:"工具 Schema 处理",historical_before_audit:"入口到审计前（原因未细分）",historical_before_forward:"审计到转发前（原因未细分）",historical_43_to_101:"43 转发到 101 入口",historical_101_before_audit:"101 入口到审计前",historical_101_before_forward:"101 审计到转发前",historical_101_first_output:"101 转发到首输出",historical_first_output_relay:"首输出回传差",historical_after_first_output:"首输出之后"})

Object.assign(stageNames,{bootstrap_automation:"自动化 Bootstrap",bootstrap_delegation:"委派 Bootstrap"}); Object.assign(eventNames,{upstream_error:"上游错误"})

Object.assign(stageNames,{request_prepare:"转发准备（含未细分处理）"})

Object.assign(stageNames,{websocket_connect:"WebSocket 建连"});Object.assign(eventNames,{turn_start:"开始新一轮请求"})

Object.assign(stageNames,{websocket_turn:"WebSocket 本轮处理"});Object.assign(eventNames,{turn_complete:"本轮完成",turn_attempt_error:"本轮尝试异常"})
