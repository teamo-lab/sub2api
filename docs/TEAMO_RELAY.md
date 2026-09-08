# 内置 Teamo Relay

Relay 是 Sub2API 内的请求级 Go 模块，目录为 `backend/internal/relay`。
它不连接业务数据库或 Redis，不创建文件、执行账本、HTTP 服务或后台恢复任务。
HTTP 客户端、连接池、鉴权、代理、协议转换、组内重试、账号调度和客户计费继续由 Sub 的现有实现持有。
模块关闭时保留既有路径；不需要数据库迁移。

## 当前实现

- `CommitGate` 暂存提交前的前缀，最大 8 MiB，以 16 KiB 分段控制小写入的内存开销。
  到达上限返回失败，不强制放行半截工具调用。首次提交即不可逆，部分写入失败也不能重放。
- `ChatObserver` 识别 Chat 的角色前导、工具片段、实际输出、错误和终态。
  usage 是用量证据，不单独作为成功终态；不完整工具不能因 `[DONE]` 变成成功。
  供应商错误是否允许 retry 仍调用 Sub 原有错误策略。
- Native Responses、Responses passthrough 和 Responses→Chat 复用 Sub 的现有 SSE
  修复与语义判定，缓冲改用 Gate。实验路径中，已经发送的 reasoning 也关闭透明重试窗口。
- Chat→Responses、Chat→Messages 在将 chunk 交给原有转换器前应用 Gate。
  提交前失败不泄漏 `response.created` / `message_start`；失败后不合成成功终态。
- 已观察到的 usage 继续返回给现有处理器。失败重试不新增客户扣费；Chat 失败尝试的上游用量
  额外记入既有结构化日志，不能因此认为供应商成本为零。

这里保留了 New API Client / 旧 Rust 原型的提交边界测试序列，没有移植独立服务、journal、
executor identity、RPC、持久容量对账或 receipt outbox。当前 Go 接入主要复用最新 Sub 已有能力，
不是将 New API Client 的转换器重新复制进来。

## 实验开关和观测

`GATEWAY_TEAMO_RELAY_ENABLED` 默认为 `false`。
`GATEWAY_TEAMO_RELAY_GROUP_IDS` 是逗号分隔的已认证 API Key 分组 ID 白名单；空列表不产生曝光。
分组来自鉴权中间件，公网 Header 不能替其他分组开启功能。生产分组 ID 必须在发布前实时核对，
不能复制其他 Sub 实例的同名分组 ID。

入口中间件在排队和账号选择之前写 `teamo_relay.assignment`，结束时写
`teamo_relay.request_end`，实际流处理路径在 `relay_paths` 中列出。
记录带原有请求关联信息和规范化的 `router_trace_id`。不写 prompt、凭据或正文。
这些日志是执行路径证据，HTTP 200 不是成功证明；SLA 仍以 TR 最终客户端结果为准。

蓝绿 staging 支持候选槽独立配置：本机审计发布包装器读取
`SUB2API_RELEASE_RELAY_ENABLED=true|false|inherit` 和
`SUB2API_RELEASE_RELAY_GROUP_IDS=<已核实的正整数ID列表>|inherit`，传递到 stage helper。
省略时继承稳定槽运行容器的配置；新启用但未指定任何分组会被拒绝。
stage helper 生成临时 Compose environment overlay，只重建已确认 0% / 无活动流的候选槽，
保留原始 Compose、公共 runtime.env、稳定槽和 worker。容器启动后必须读回开关和分组一致，
否则不能记录为 staging 成功。临时 overlay 随命令退出清理，结果报告包含配置摘要。
配置读回只能证明变量传递，应用模块的实际生效仍须由路径日志和请求验收证明。
这些参数沿用既有 lease、备份、版本/digest、NAT 和跨机真实 Key 门禁。

实验必须按入口分流归属统计，不能排除未走到流处理器的排队/连接失败。
关联缺失、重复 trace ID、跨槽重试以及跨资源组重试需要显式核对，不能将无法归属的请求
当作正常对照或成功实验请求。仅靠最终一次 Sub request ID 不足以覆盖中间失败尝试。

## 发布和验收门禁

1. 完成真实请求路径、取消、部分写入、协议转换、重试预算及用量验收，之后提交/合并 PR。
2. 43 候选先保持 0%，验证原 Key 的真实 43→101 公网/私网链路及生产模型完整终态。
3. 授权的第一阶段为 43 候选 10%、稳定槽 90%。从 TR 观察 GPT harness 同期 SLA、
   端到端 TTFT P90、请求级 cache；按模型/协议拆分，三个指标均须持平或改善。
4. 至少观察 60 分钟和 12 个完整五分钟桶，每个受测模型每组至少 1,000 请求；报告
   差值和 95% 区间，样本与统计把握不足继续观察。不能用定向冒烟代替真实流量效果验收。
5. 第一阶段通过后，才推进 TR 跨资源组控制的后续发布和最终 5% 端到端实验。
   最新目标以 SLA 收益为主：通过合规 retry/fallback 恢复提交前失败，提高客户端最终成功率；
   端到端 TTFT P90 和请求级 cache 持平即可，原 P90 至少下降 30% 不再作为门禁。

新版本和旧稳定槽若包含不同的非 Relay 补丁，需先处理版本差异和对照设计。
2026-09-08 的只读检查发现 43 处于 `chat-fallback-sla` 回滚后状态；该补丁已在 main。
这不是本次 Relay 已部署的证据，也不能据此省略稳定基线验证。

## 未完成项

- Native Anthropic 数据面及 WebSocket 尚未接入本模块；非流式继续依赖原有完整缓冲和错误处理。
  当前测试通过不代表这些路径已完成移植或可以在收益报告中算作 Relay 覆盖。
- 已完成真实 HTTP 的读取停顿、客户端取消及关闭连接测试；隔离本机 PostgreSQL/Redis/Sub
  的 Responses 与 Chat 上游场景验证了成功、组内恢复、部分工具、提交后失败、30秒无输出超时
  和实际用量/余额一致性。这是受控上游的开发验收，仍需生产真实渠道验证。
- 候选槽配置隔离已完成本地隔离测试；实际发布、TR 端到端关联覆盖率、真实供应商计费
  及生产验收尚未完成。
- 内置模块 PR #20 已合并；尚未部署或开启本实验 10% / 5%，没有线上收益结论。

## 跨层观测关联

Gateway 从自己的入口生成 `X-Teamo-Observation-ID`，每次真实 Sub dispatch 再生成
`X-Teamo-Attempt-ID`。Sub 只接受规范 UUID 作为日志关联字段；这两个 Header 不承担鉴权、
分组选择、Relay 开关或计费去重，也不能独立证明请求来自 Gateway。必须与 Gateway 的
服务端 admission 和 Router dispatch 一对一关联，才能成为实验归属证据。

配置了受测分组名单的实例在鉴权前记录 `teamo_relay.ingress` 和正常返回时的
`teamo_relay.ingress_end`，含真实 deployment slot/version/digest/release ID。
两 ID 缺失/不合法、未配置分组名单或不是所覆盖的 POST 路径时不记录。它不经过 ingress
reject access sampler。鉴权成功后的 assignment 才记录真实 group ID 和启用状态，最终
request_end 继续说明实际流处理路径。所有这些观测事件只进入既有标准日志，明确跳过
Ops 数据库日志 sink，不新增业务持久记录。

panic/崩溃/丢日志产生的不完整尝试必须保留 unknown；特别是不能在外层 Recovery 处理前
把默认 HTTP 200 记录成最终成功。HTTP 状态本身也不代表语义成功。

可信 Relay 对照需要两槽拥有同一份可观测代码、相同目标 group 名单，稳定槽 Relay 关闭，
候选槽开启。先验证 Relay 关闭的共同基线，再开展 off/on 对照，避免混入其它 main 补丁
和观测能力差异。旧稳定槽没有 assignment 时，缺失日志不能被默认为稳定对照请求。

## 单机源码 staging 的二进制来源

默认 `fleet_binary` 保留双机同二进制门禁，已有 NAT 入口仍要求提供另一已验证版本的
`SUB2API_EXPECTED_BINARY_SHA256`。只有明确仅发布一个目标时，才可设置
`SUB2API_SINGLE_HOST_RELEASE_TARGET=<与 SUB2API_CLOUD_HOST 完全相同的目标>`；本机包装器
将该次自定义源码 `stage` 标为 `single_host_image`。不能用于 official mode，双机发布不能
使用它替代 fleet 门禁。

单机路径仍先验证 release phase、候选0%/零活动流、数据库依赖、严格 NAT 入口和资源余量，
创建 PostgreSQL 备份后，按提交源码构建不可变镜像。随后只在该镜像内以无网络、只读根文件
系统执行 `sha256sum /app/sub2api`，不启动应用、不连接业务数据库。若指定外部 expected hash，
必须在重建候选前一致。候选启动后其实际 binary hash 必须与镜像内 reference 一致；失败仍
停留0%，不能进入canary。结果明确报告 `binary_reference=single_host_image` 与 source commit，
不冒充来自另一主机或已完成生产效果验证的二进制。

该适配不更改现有 NAT 规则、稳定槽、单例worker或流量比例；真实Key/协议、默认off与on、
计费、采集覆盖和后验门禁仍需完成。只有本地隔离 host 工具测试通过不代表已部署。

## 同镜像 Relay off/on 配置实验

完成共同代码、Relay 关闭的基线发布后，可通过经审查同步的本机包装器使用专用入口：

```text
sub2api-release stage-relay-config <release-id> <stable-version> <stable-image-digest> <source-commit> <binary-sha256> <group-ids>
```

调用仍须持有匹配 owner/release ID 的 fleet lease，显式设置与 `SUB2API_CLOUD_HOST` 相同的
`SUB2API_SINGLE_HOST_RELEASE_TARGET`；已有 NAT 入口还须明确允许并通过原 strict validator。
包装器只给 `stage-image-release` 传递 `relay-off-on` 模式，不调用 `attest-identical`、canary 或 promote。
没有显式模式时，同镜像 staging 继续被拒绝。

专用模式只允许相同 immutable image、实际 binary SHA256、嵌入的 source commit 与版本。
稳定槽必须 Relay 关闭，候选必须显式开启；两边非空 group 名单必须完全一致。不同镜像、
版本、源码、分组或无实际 off→on 变化均拒绝。候选仍须健康、0%且无活动流，稳定槽保持100%；
NAT、依赖、资源余量与 PostgreSQL 备份检查完成后才重建候选。真实生产原Key/协议门禁继续执行。

结果提供两槽基础 environment SHA256 快照，只剔除 slot、release ID 和本次 Relay 开关差异，
不输出环境变量值或凭据。此快照不是完整配置等价证明；发布验收还要核对现有配置文件、
协议和分组运行面。快照不一致时候选保持0%，不能开流，先定位并通过正式配置流程处理差异。

同镜像不等于相同行为，不能据此使用 identical-image attestation 缩短真实流量验收。
off/on 对照仍按前述60分钟、12个完整桶、每模型每组至少1,000请求和覆盖率要求，报告
被恢复的提交前失败及最终 SLA；TTFT P90、请求级 cache 均须持平或改善。
