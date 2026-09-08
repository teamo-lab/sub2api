# 耗时分析页交付与验收

用户授权：基于 teamo-lab 最新 main，开发独立管理页，本地运行真实应用并用近期长尾请求验收，再提交 PR。禁止本任务自行生产上线。PR 提交后才向 01a0811c-44ed-7ab0-abb2-112420b6f2ca 一次性同步上下文；开发及本地验收期间不得再发消息。

基线：2026-09-09 fetch teamo/main = 5552b9f51。独立 worktree request-profiling-panel / codex/request-profiling-panel。

## 必须完成

- [x] 网关入口开始单调计时，到最终完成/失败/取消；HTTP、SSE、WS 明确区分，WS 多响应不可冒充单请求。
- [x] 有界逐次 span/attempt/event，记录账号、错误状态、retry/backoff/fallback；不保存正文、Key 或上游 URL 查询参数。
- [x] 正文读取/解压/校验、bootstrap、审计、鉴权、用户与账号队列、准备/协议转换、连接/发送/上游首字节/语义输出/流尾；没有观测的区间明确未归因。
- [x] 用现有异步运维持久化链路存储每条 profile；丢弃/截断和缺失覆盖明确显示，不能冒充全量。
- [x] 管理员 API 支持时间/模型/分组/账号渠道/错误类型过滤，单请求完整轨迹与服务端聚合；聚合阶段均值可加，总体 P90 单独算，不拼接各阶段 P90。
- [x] 独立导航“耗时分析”：整体与单请求长条、关键 retry/fallback 标记、阶段明细、可定位长尾与证据缺口，加载/空/错误/筛选交互可用。
- [x] 请求语义和响应字节等价验证；计时单调、重叠不重复计数、重试归因、聚合分母/筛选正确性、权限与无秘密字段测试。
- [x] 本地最新 main 配套服务运行，浏览器完整交互与截图验收；近期真实长尾数据只导入已有元数据/区间重建，标记历史证据，缺失不伪造；同时真实发起本地受控 retry/fallback 请求验证新打点。
- [ ] 检查构建、测试、浏览器效果、修复问题后 push 并创建 teamo-lab/sub2api PR。同步协调任务，禁止自行上线。

## 实现方向

requestprofile 独立无业务依赖包，入口 context 贯穿到公共 HTTPUpstream，逐次记录 span 和 HTTP attempt。完成时挂到 http.access extra；运维页面查询持久化记录。使用单调微秒和不重叠 interval partition 构建长条；原始 span 单独保留以解释嵌套和并行。默认记录网关请求，固定上限并显式 truncated，不新增正文扫描。日志写入沿用有界异步 sink；页面同时展示其健康状态。

历史证据：/Users/zhangyiming/sub2api/reports/ttft-goal-20260909/current-phase-reconstruction.json、current-paired-baseline.json。新的入口数据不能用这些历史估算假装实测函数 span。

## 已接收的生产对接信息（开发期间不回复或同步其他任务）

协调任务发来：43 stable57cca100d，Relay=false；容量重选候选0e7b6d6来自PR29合并c5d383f65。`openai.local_capacity_reselect_*` 的 origin 为 local_account_admission，必须作为本机准入重选，不能算成上游429/重试/fallback。继续本地开发与最新main验收。最终提交PR后再统一交接。
