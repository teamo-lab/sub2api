# 本机账号容量拒绝后的有界重选

此功能默认关闭。只覆盖 OpenAI Responses 与 Chat Completions 的同步/SSE 请求；只在当前账号本来就要返回本机队列满或槽位等待超时 429 时，尝试现有同模型候选，不改变账号并发、优先级、状态、模型或代理。

## 开关与本轮范围

在原 `Account.extra` 完整对象上合并以下两个字段，不能覆盖其他 extra 字段：

```json
{
  "openai_local_capacity_reselect_enabled": true,
  "openai_local_capacity_reselect_group_ids": [2]
}
```

必须同时是严格的布尔 `true` 和包含当前分组的正整数数组。分组取已认证 API Key 的 `GroupID`，不读取请求自报的分组。缺失、false、字符串 true、非法数组或分组不匹配均保持旧行为。本轮实流计划只给 43 的 #23、group 2 启用；不扩到 group 3。本文没有执行任何配置写入或生产发布。

旧生产基线 `57cca100d` 会忽略这两个新字段。主任务可将开关关闭、恢复写前保存的完整 extra，或通过既有发布控制器撤回新候选流量；不得为回退改变 C/P 或账号状态。

## 请求边界

- 初始账号仍保留原 sticky 等待机会。只有它实际队列满或等待超时，才暂存原 429 并排除该候选。
- 复用原 selector、模型能力和利润准入校验；备用只尝试立即取槽，不开第二个等待窗口。
- 用户配额已通过后继续持有原许可，保留原请求 context/deadline；有既存 error-recovery state 时只消耗其剩余 switch 次数，不新建或重置预算。也受 handler 原有 switch 上限约束。
- 客户端取消、deadline、已有语义输出、已提交错误、用户限额或依赖故障不能转为容量重选。备用取槽和最终准入后再次核对请求仍有效；失败时释放所获槽位。
- 探查时延迟 sticky 写入，避免满载、取消或利润否决的备用覆盖原会话。成功完成准入后按既有绑定策略处理，绑定可能改变，不承诺 cache 完全不变。
- 没有备用时仍返回原账号的原 429。备用成功取槽并开始 Forward 后，真实成功/失败决定结果，旧 429 不覆盖新的上游错误。

## 实验日志与对账

沿用已有结构化请求日志（Ops system logs），不创建上游错误事件来冒充供应商 429：

| event | 含义 | 关键字段 |
|---|---|---|
| `openai.local_capacity_reselect_eligible` | 已启用、可信组匹配、尚未语义输出的本机容量拒绝；每请求最多一次，是否实际重选仍取决于剩余预算 | `origin=local_account_admission`、`source_account_id`、`reason_code`、`original_wait_ms` |
| `openai.local_capacity_reselect` | 消耗一次预算并排除当前满载候选 | 原 `source_account_id`、当前 `rejected_account_id`、`reason_code`、`wait_ms`、`local_reselect_count`、`request_switch_count` |
| `openai.local_capacity_reselect_admitted` | 备用完成槽位/利润准入；还不代表模型请求成功 | 原 `source_account_id`、`selected_account_id`、原拒绝原因与等待、累计本机重选次数 |

日志继承现有 request/client request ID、分组和模型上下文。以逻辑请求去重：原触发账号固定保留 #23，再关联该请求的最终 usage/错误及真正执行账号 B；不能仅按最终 `account_id=23` 作分母，否则会漏掉被救回的请求。`gateway_queue_full` 与 `gateway_concurrency_limit` 分别保留队列满和原槽位等待超时。`original_wait_ms` 只测原账号等待函数实际耗时，不含用户排队或上游推理，更不代表完整 client TTFT。

发布后记录 eligible 数、实际重选次数、救回的真实成功、仍拒绝及后续上游失败，同时观察正常流量的 SLA、等待、TTFT 与已有会话 cache。成功取槽日志不能替代 usage/终态证据，本地测试不能记为生产收益。

## 本地验证边界

已建立真实 handler 红绿对照：两入口、JSON/SSE、P1 满载而同模型 P10 有槽；开关 off/组不匹配、备用全满无二次等待、用户/key 限额、取消/deadline、备用取槽时取消、后续真实上游错误、已输出保护；LoadBatch 开/关均检查失败探查不写入 B 的 session binding。成功路径核对上游调用及 usage repository 提交各一次；这不等同财务数据库扣款验证。

原恢复 state/ctx/deadline 和 switch 额度另有针对性测试。最终部署/启用仍需主任务审查与实际候选二进制门禁；本 PR 不进行生产发布。
