# Sub2API 单机蓝绿 Canary 发布设计

状态：已于 2026-08-11 完成生产接管（旧容器和旧镜像仍保留）
适用范围：当前腾讯云单机 Docker Compose 生产环境
目标：未来每次发布先让新镜像承接 10% 真实 API 流量，通过门禁后切到 100%，且不打断已有请求和 SSE/WebSocket 长连接。

## 1. 结论

改造前的生产环境只有一个 `sub2api` 容器直接监听宿主机 `8080`。执行
`docker compose up -d --no-deps sub2api` 时，旧监听器会关闭，新容器重新绑定端口；
应用自身的 graceful shutdown 只有 5 秒。因此旧流程不能承诺无损发布。

目标方案是在应用前增加一个长期不变的 HAProxy 入口，并把应用拆成两个可同时运行的
API slot 与一个 singleton worker：

```text
                         ┌───────────────────────────┐
client :8080 ───────────▶│ HAProxy（稳定监听，不随发布重启） │
                         └─────────────┬─────────────┘
                                       │
                           90% / 10% 或 0% / 100%
                             ┌─────────┴─────────┐
                             ▼                   ▼
                    ┌────────────────┐  ┌────────────────┐
                    │ sub2api-blue   │  │ sub2api-green  │
                    │ API role only  │  │ API role only  │
                    └───────┬────────┘  └───────┬────────┘
                            └─────────┬──────────┘
                                      ▼
                           PostgreSQL + Redis
                                      ▲
                                      │
                            ┌─────────┴─────────┐
                            │ sub2api-worker    │
                            │ singleton jobs   │
                            └───────────────────┘
```

发布期间旧 slot 不会被重建。切权重只影响新请求；已有连接继续由原 slot 服务。
旧 slot 只有在 HAProxy 显示活动连接为 0 后才停止。若连接长期不结束，发布标记为
“业务已切换、旧实例待排空”，自动化不得强杀。

本方案解决应用发布导致的中断，但不解决单机宕机、Docker daemon 重启、宿主机网络故障等
机器级单点问题。要覆盖这些故障，需要跨机器高可用，不属于本方案范围。

## 2. 已确认的现状与约束

- 生产主机为 4 vCPU、8 GiB 内存、约 100 GiB 系统盘。
- 检查时宿主机可用内存约 6.2 GiB，磁盘可用约 70 GiB。
- `sub2api`、PostgreSQL、Redis 实际内存合计约 435 MiB；具备同时运行两个 API
  slot 的基础资源余量，但发布前仍必须检查实时峰值与磁盘空间。
- 账号、用户、API Key 的并发槽位存储在 Redis，API 多实例共享，不会因两个 API
  slot 而天然翻倍。
- 数据库 migration 已使用 PostgreSQL advisory lock，能避免两个实例同时执行 migration；
  但这不代表任意 migration 都与旧版本兼容。
- 部分周期任务有 leader lock，部分没有。不能直接复制完整应用进程，否则可能重复执行
  Token 刷新、清理、监控或其他后台任务。
- 当前 `usage_logs` 和 `ops_error_logs` 没有部署 slot/version 字段，无法可靠地按蓝/绿
  计算 P50、SLA 和 Cache 率。
- 当前应用 `http.Server.Shutdown` 超时为 5 秒；Docker Compose 未配置更长的
  `stop_grace_period`。

## 3. 发布门禁

### 3.1 主门禁

蓝、绿必须使用同一个真实流量时间窗比较。不能拿“发布前 30 分钟”与“发布后 10 分钟”
直接比较，否则上游波动、请求模型和输入长度变化会污染结论。

| 指标 | 定义 | 通过条件 |
|---|---|---|
| P50 | 完成的流式请求 `first_token_ms` 的 P50，即 TTFT P50 | `green_p50 <= blue_p50 × 1.30` |
| SLA | 真实完成 SLA，排除用户自身业务限制，但计入 HTTP 错误及 HTTP 200 流内失败 | `green_sla >= blue_sla - 0.005` |
| Cache 率 | `Σcache_read_tokens / Σ(input_tokens + cache_read_tokens + cache_creation_tokens)` | `green_cache >= blue_cache - 0.10` |

其中 `0.005` 表示绝对值 0.5 个百分点，`0.10` 表示绝对值 10 个百分点，不是相对下降
0.5% 或 10%。任意一项越界都禁止全量。

### 3.2 样本与可比性

- 10% 阶段至少观察 10 分钟，并且 green 至少产生 200 个 terminal outcomes。
- 未达到 200 个 green 样本时继续观察，不自动扩大流量。
- blue 与 green 都必须有有效 TTFT 和 token usage；分母为 0、数据延迟或指标缺失一律
  fail closed，即禁止晋级。
- 按 `platform/model/request_type/reasoning_effort/input-token-bucket` 检查流量结构。
  任一主要分层占比相差超过 10 个百分点时延长窗口，避免把请求难度差异误判为版本差异。
- Cache 率使用 token 加权，而不是“命中过缓存的请求数 / 总请求数”，因为门禁关注实际成本。
- 统计窗口按请求开始时间归属，避免长请求在完成时集中落入后一个窗口，扭曲 TTFT 和 SLA。

### 3.3 真实 SLA

当前控制台 SLA 主要使用：

```text
usage success / (usage success + HTTP >= 400 且非业务限制的错误)
```

发布门禁必须使用更严格的 terminal outcome：

```text
success = 客户端收到完整 terminal success，且 usage 成功落库
failure = 非业务限制错误，或 response.failed，或缺少 terminal，或流中断且未恢复
SLA = success / (success + failure)
```

同一个 `request_id + api_key_id` 只计算一次最终结果，fallback 中间尝试不能重复计为请求失败。
这样 `stream_read_error`、缺少流式结束信号以及 HTTP 200 后的流内错误不会从 SLA 中消失。

### 3.4 辅助护栏

下面指标不替代三项主门禁，但触发时立即停止发布：

- green 容器 crash/restart、OOM、health/readiness 连续失败；
- migration 失败或 schema 兼容检查失败；
- HAProxy 到 green 的连接错误、无 terminal 响应突增；
- PostgreSQL/Redis 健康异常；
- 宿主机可用内存低于 2 GiB、磁盘可用低于 15 GiB，或出现持续 CPU 饱和；
- 观测数据延迟超过 2 分钟；
- 构建 commit、image digest、应用 `/version` 三者不一致。

辅助报告还必须包含总耗时 P50/P95、TTFT P95、上游 429/529、fallback 次数、账号分布、
CPU/内存和实际成本，防止为修复一个指标而拉坏另一个指标。

## 4. 运行架构

### 4.1 容器职责

```text
haproxy           固定绑定 0.0.0.0:8080
sub2api-blue      仅 expose 8080 到 Compose 内网，不映射宿主端口
sub2api-green     仅 expose 8080 到 Compose 内网，不映射宿主端口
sub2api-worker    不接收公网流量，只运行 singleton jobs
postgres          共享数据库
redis             共享缓存、并发槽位与分布式锁
```

需要增加 `SUB2API_PROCESS_ROLE`（或等价配置）：

- `api`：HTTP API、每实例缓存、请求级 usage worker、必要的 pub/sub subscriber；禁止周期性
  singleton jobs。
- `worker`：Token refresh、expiry、cleanup、scheduled report/test、backup、channel monitor 等
  周期任务；不进入 HAProxy backend。
- `all`：仅用于向后兼容本地开发，生产禁用。

每个现有后台服务都必须登记为 `per_replica` 或 `singleton`。未登记的服务不得默认在
两个角色都启动。对 singleton worker 的版本升级独立于 API 切流：API green 通过并全量后，
再以“先停旧 worker、再启新 worker”的短暂停顿完成升级。后台任务短暂停顿不会中断用户请求。

### 4.2 HAProxy 行为

- API 路由（`/v1/*` 及实际兼容入口）按权重分发；10% 阶段为 blue 90、green 10。
- 控制台、登录、静态资源和管理 API 在 canary 阶段固定到 blue，避免一次会话混用两套前端；
  API 门禁通过后再随全量切到 green。
- HAProxy Runtime API 用于即时调整权重和把旧 slot 置为 `drain`。
- Runtime 权重同时写入 `/opt/sub2api/deploy/rollout/state.json`；HAProxy 重启时必须从该文件
  恢复，不能因重启退回配置文件中的默认权重。
- 每次成功切权、回滚或 reconcile 后还要把 `show servers state` 原子持久化为
  `rollout/runtime/server-state`；final HAProxy 启动时加载该文件，避免进程重启到静态默认权重。
- 普通切权和回滚都先保留实际权重快照。HAProxy runtime 命令部分成功或验证失败时，按原
  API/console 权重以“先 ready、后 drain”的顺序恢复；恢复结果无法验证时进入 fail-closed，
  禁止自动重复写操作。
- 入口删除客户端伪造的 `X-Sub2API-Deployment-*`，再写入可信的 slot/version 标记。
- 正确传递 client IP，但应用只信任 Compose 内网中的 HAProxy，防止伪造
  `X-Forwarded-For`。
- 不启用响应缓冲、代理缓存或压缩；不得缩短现有大请求和 SSE 的生命周期。
- `timeout client/server/tunnel` 设为至少 1 小时；SSE keepalive 仍由应用负责。
- Runtime socket 只挂载到本机受限目录，不暴露 TCP 公网端口。

### 4.3 可观测性归属

为 `usage_logs`、`ops_error_logs` 以及 terminal outcome 增加：

```text
deployment_slot       blue | green
deployment_version    版本字符串
deployment_digest     镜像 digest 的短标识或完整 digest
request_started_at    请求进入 API 的时间
terminal_outcome      success | failed | cancelled | incomplete
```

新增字段必须是 nullable/default-safe 的 expand migration，旧版本可以忽略。HAProxy 覆盖
客户端同名 header，并且两个 API slot 不开放宿主端口，因此应用可以信任该内部标记。

不能只依赖 HAProxy access log 与数据库按 `request_id` 临时 join：这种方式无法可靠识别
HTTP 200 流内失败，也容易因日志轮转或 request ID 缺失造成门禁漏算。

## 5. 发布状态机

```text
PRECHECK
   │
   ▼
START_GREEN(weight=0) ──失败──▶ ABORT
   │
   ▼
WARMUP_AND_SMOKE ───────失败──▶ STOP_GREEN
   │
   ▼
CANARY_10 (blue=90, green=10)
   │                         │
   │三项门禁通过             │任一越界/数据缺失
   ▼                         ▼
PROMOTE_100             ROLLBACK_GREEN_TO_0
   │                         │
   ▼                         ▼
SOAK_100                 DRAIN_GREEN
   │
   ▼
DRAIN_BLUE ──活动连接>0──▶ 保留并继续观察
   │活动连接=0
   ▼
UPDATE_WORKER_AND_COMPLETE
   │仅首次接入，且无意义发布也验证完成
   ▼
FINALIZE_ENTRY(8080 → HAProxy)
```

### 5.1 PRECHECK

1. 对 PostgreSQL 创建带时间戳的备份并验证文件非空。
2. 记录 blue commit、version、immutable image digest 和当前 HAProxy 状态。
3. 检查 PostgreSQL、Redis、HAProxy、blue 健康；读取主机 CPU、内存、磁盘。
4. 校验 candidate 镜像签名/摘要、构建 commit、版本信息，不使用可漂移的 `latest` 作为
   发布或回滚定位。
5. 检查 migration 清单。只允许向后兼容的 expand migration；drop、rename、收紧约束、
   改变旧字段语义等变更必须拆成至少两个 release。
6. 保存同窗比较所需的过滤条件与基线快照。

### 5.2 START_GREEN 与预热

1. 将 candidate 启动到空闲 slot，权重保持 0。
2. 等 PostgreSQL、Redis `service_healthy` 后再启动 green。
3. readiness 连续 3 次成功；核对 `/version`、digest、schema 状态。
4. 直接访问 Compose 内网 green，执行脱敏回放与 smoke：
   - 普通 Responses SSE；
   - Chat Completions；
   - 长输入/compact；
   - tool call；
   - `response.failed`、缺 terminal、`stream_read_error` 等事故回放；
   - 现有 9-case 数据集。
5. 对固定 prompt 做 blue/green 配对测试，验证 TTFT 与 Cache usage 没有明显退化。
   配对负载使用固定数量的 session/cache shard（当前 8 个），每个 shard 内保持相同公共前缀，
   两槽按相同序列发压。禁止所有请求共用一个 session seed，否则会把请求钉到少数账号，制造
   假性 overload；也禁止每请求完全随机 prompt，否则 Cache 率失去可比性。

### 5.3 CANARY_10

1. 原子写入期望状态，再通过 Runtime API 设置 blue=90、green=10。
2. 每 30 秒计算一次同窗指标；门禁采用滚动窗口，不能只看某一个瞬时点。
3. 满足“至少 10 分钟 + green 至少 200 个 terminal outcomes”后才允许判断通过。
4. 连续 3 个计算周期均通过才晋级；任一周期越界立即把 green 置为 `drain`/权重 0。
5. 即使 candidate 相对 baseline 通过，若两边同时因测试负载出现低绝对 SLA 或上游 overload，
   该窗口仍作废且不累计 consecutive pass；先停止测试、冷却并降低测试 QPM。
   控制器默认要求 candidate 绝对 SLA 不低于 99.5%，且 scoped 窗口内 upstream 429/529/5xx
   均为 0；这些是辅助护栏，不改变三项主门禁的相对阈值。

### 5.4 PROMOTE_100 与 SOAK

1. 设置 green=100、blue=`drain`。该动作只改变新请求归属。
2. 继续观察至少 10 分钟，并对 green 单独复算三项门禁与辅助护栏。
3. 全量窗口失败时，只要 blue 仍健康且保持 warm，立即恢复 blue=100、green=`drain`。
4. 全量窗口通过后升级 singleton worker。

### 5.5 DRAIN 与停止

- 通过 HAProxy stats/runtime 数据确认旧 slot 当前活动连接数。
- 活动连接大于 0 时绝不执行 `docker stop`。
- 活动连接为 0 后，再向旧应用发送 SIGTERM。
- 应用 graceful shutdown 改为可配置，生产建议 15 分钟；Compose
  `stop_grace_period` 必须略长于应用超时，例如 16 分钟。
- Go `http.Server.Shutdown` 不会等待 hijacked WebSocket，因此停止条件必须同时依赖
  HAProxy 的实际 server stream 数，而不能只依赖应用 shutdown。
- 自动化不调用 HAProxy 的 `shutdown sessions server`；该命令会主动中断客户流，只能作为
  人工事故处置命令。

### 5.6 首次接入后的最终入口转移

首次蓝绿接入期间，旧容器仍占用宿主机 `8080`，新连接由带精确 comment 的 PREROUTING
规则重定向到临时 HAProxy `18080`。只有真实版本和一次相同代码的无意义发布都完成
`CANARY_10 → SOAK_100 → COMPLETE` 后，才允许执行 `finalize-entry`：

1. 要求 `phase=complete`、stable 为 blue、blue=100/green=0，且旧容器、两 slot 和临时
   HAProxy 都健康；同一个 controller lock 防止与发布/回滚并发。
2. 连续三次确认旧容器宿主端口没有 established connection；有活动连接时 fail closed。
3. 停止但保留旧容器和 immutable image，启动 stable digest 对应的唯一 worker，确认健康且
   restart count 为 0。
4. 启动直接绑定 `8080` 的 final HAProxy，先通过本机 GET `/health` 核对可信 slot header。
5. 原子切换控制器 socket 和 state 中的 `entry_port=8080`，再移除临时重定向规则；临时
   HAProxy `18080` 暂时保留，作为入口转移故障恢复路径。
6. 任一步失败都幂等恢复 `18080` 重定向、原 socket/state，停止新 worker/final HAProxy，并
   重启旧容器。不得删除旧容器、旧镜像、volume 或备份。

入口转移完成后，一键回滚仍只修改 final HAProxy 的 runtime 权重；控制器依据 state 的
`entry_port` 选择 `8080` 健康探针，不需要再改 iptables，也不会重启入口。

## 6. 回滚规则

| 阶段 | 回滚动作 | 用户影响 |
|---|---|---|
| green 权重 0 | 停 green | 无 |
| 10% canary | green 置 `drain`，blue 恢复 100；green 已有流继续排空 | 新请求立即回 blue，已有请求不中断 |
| 100% soak | blue 恢复 `ready`/100，green 置 `drain` | 新请求立即回 blue，已有请求不中断 |
| blue 已停止 | 重新启动已固定 digest 的 blue，健康后切回 | 回滚较慢，因此 blue 必须保留到 soak 完成 |
| destructive migration 已执行 | 禁止自动回滚 | 该版本本就不应进入 canary |

数据库备份是灾难恢复手段，不是普通应用回滚步骤。自动流程不得自动恢复数据库。

### 6.1 控制器命令契约

云端只允许一个控制器修改 rollout state、HAProxy 权重和 slot 生命周期：

```text
/opt/sub2api/deploy/bin/sub2api-rollout status --json
/opt/sub2api/deploy/bin/sub2api-rollout prepare-stage --release-id <id> --candidate-slot <slot> --action-id <id> --json
/opt/sub2api/deploy/bin/sub2api-rollout stage --release-id <id> --candidate-slot <slot> --candidate-version <version> --candidate-digest sha256:<digest> --action-id <id> --json
/opt/sub2api/deploy/bin/sub2api-rollout weights --blue 90 --green 10 --phase canary_10 --json
/opt/sub2api/deploy/bin/sub2api-rollout gate --window-start <UTC> --window-end <UTC> --json
/opt/sub2api/deploy/bin/sub2api-rollout complete --actor <actor> --action-id <id> --json
/opt/sub2api/deploy/bin/sub2api-rollout reconcile --json
/opt/sub2api/deploy/bin/sub2api-rollout rollback --to rollback-target --dry-run --json
/opt/sub2api/deploy/bin/sub2api-rollout rollback --to rollback-target --actor <actor> --reason <reason> --action-id <id> --json
```

`prepare-stage` 必须在替换 idle container 之前执行：它先把新 release 的 rollback target 固定为
当前 serving stable，从而消除“candidate 正在被替换、控制器却仍把它当回滚目标”的窗口。
`stage` 只接受已经 prepare、当前 0% 且活动流为 0 的 idle slot，并同时校验容器实际 image ID 与四项部署
metadata；`complete` 只有在三轮门禁通过、100% soak 达到最短时间且旧 stable 仍是健康 warm
rollback target 时才允许交换 stable/candidate 语义。CLI 输出必须是结构化 JSON，不能输出凭证、环境变量、
账号内容或未经白名单过滤的日志。

无意义发布使用 `/opt/sub2api/deploy/bin/sub2api-stage-identical-release`。该脚本只允许把 stable
的同一个 immutable image ID 放入 0%/0-stream 的 candidate slot；它先调用 `prepare-stage`，
再重建 idle container、验证 health/slot header/restart count，最后调用 `stage`。任何中途失败
都保持 serving stable=100，状态停在 `preparing`，由一键回滚幂等收敛，不会删除旧镜像。

当 stable/candidate 的 immutable image ID 完全相同时，可用 `attest-identical` 对 no-op 使用
严格快路径，避免为了重复证明同一二进制而争抢客户号池容量。控制器只接受
`reports/paired-*` 下 root-owned `0600` 的至少 5 分钟证据，两槽必须各至少 10/10 完整 terminal，
并重新计算 P50/SLA/Cache 与 99.5% 绝对 SLA、upstream 429/529/5xx 护栏；证据文件组合做
SHA-256 后写入 state/history。快路径只替代重复的镜像性能取样，不替代真实 10% canary、
一键回滚演练、长 SSE 切换、100% promotion 和 10 分钟 soak。

建议稳定退出码：

| 退出码 | 含义 |
|---:|---|
| 0 | 成功，或目标状态已经满足（幂等成功） |
| 2 | 参数错误 |
| 3 | 仍是旧单容器拓扑，控制器不可用 |
| 4 | 回滚前置条件不满足或 schema 不兼容 |
| 5 | 已有 rollout/rollback 持有控制器锁 |
| 6 | previous slot 无法启动或 readiness 不通过 |
| 7 | HAProxy 修改失败，控制器已进入 reconcile |
| 8 | 切流后的验证失败 |

`status --json` 至少返回：

```json
{
  "supported": true,
  "topology_mode": "blue_green",
  "release_id": "release-20260811-001",
  "generation": 42,
  "phase": "soak_100",
  "active_slot": "green",
  "stable_slot": "blue",
  "candidate_slot": "green",
  "rollback_target_slot": "blue",
  "blue": {
    "state": "ready",
    "health": "healthy",
    "weight": 0,
    "active_streams": 0,
    "version": "...",
    "digest": "sha256:..."
  },
  "green": {
    "state": "ready",
    "health": "healthy",
    "weight": 100,
    "active_streams": 12,
    "version": "...",
    "digest": "sha256:..."
  },
  "rollback": {
    "allowed": true,
    "target": "blue",
    "block_reason": null
  }
}
```

### 6.2 Rollout state 与审计

控制器将期望状态原子写入：

```text
/opt/sub2api/deploy/rollout/state.json
```

写入必须使用“临时文件 + `fsync` + rename”，并维护单调递增的 `generation`。状态至少包含：

```text
generation
topology_mode
phase
release_id / active_slot / stable_slot / candidate_slot / rollback_target_slot
blue/green desired_state、desired_weight、version、digest
schema_version / rollback_schema_compatible
worker_version / worker_digest
entry_mode / entry_port / entry_proxy_container
last_action_id / last_actor / last_reason
updated_at
```

每次动作同时向只追加的 `history.jsonl` 写审计事件。审计记录包含动作 ID、旧状态、新状态、
HAProxy 实际状态、执行结果和时间，不包含请求正文或凭证。状态文件和审计文件权限为
`root:root 0600`，目录为 `0700`。

控制器使用 `/opt/sub2api/deploy/rollout/controller.lock` 的 `flock` 保证单写者。拿不到锁时
立即返回退出码 5，不能排队等待后执行，因为延迟执行的回滚可能覆盖更新后的人工决策。

`rollback_target_slot` 在一次 release 创建时固化，在该 release 完成或终止前不得因 promote、
rollback 或 active slot 变化而互换。`stable_slot` 只有在 100% soak、主门禁和辅助护栏全部通过，
并且发布被明确标记为 `COMPLETE` 后才更新。这样连续触发两次“一键回滚”只会得到第二次幂等
成功，不会把流量从已恢复的稳定 slot 再切回问题 candidate。

`COMPLETE` 后的角色交换为：promoted slot 成为新 `stable_slot`，旧 stable 成为下一个 idle
`candidate_slot`，但 `rollback_target_slot` 继续指向旧 stable。回滚控制器以
`rollback_target_slot` 的对侧作为 source，而不是盲目使用 `candidate_slot`；否则发布完成后的
第一次回滚会把 source/target 解析成同一个槽。

`action_id` 是一次控制器调用的幂等键，审计记录必须对其建立唯一约束或等价索引。相同
`action_id` 重试时返回第一次动作的结果；网络超时后使用同一 `action_id` 重试不得重复修改
权重。即使客户端因未知结果生成了新的 `action_id`，控制器也必须根据 release 的
`phase=rolled_back` 与相同 `rollback_target_slot` 返回 `already_rolled_back`，不得反向切流。

### 6.3 一键回滚算法

`rollback --to rollback-target` 必须按以下顺序执行：

1. 获取非阻塞控制器锁，加载 state，并读取 HAProxy 当前实际状态。
2. 固定本次 `release_id`、`generation`、`active_slot` 与 `rollback_target_slot`。若 phase 已是
   `rolled_back` 且 active slot 等于回滚目标，返回 `already_rolled_back`，不修改任何权重。
3. 检查 `rollback_schema_compatible=true`。不兼容时退出，不自动恢复数据库。
4. 验证 rollback target 的 immutable digest。容器不存在时只允许按 state 中记录的 digest
   重新启动，禁止使用 `latest`。
5. 等 rollback target readiness 连续 3 次通过；失败时保持现有流量不变。已 warm 的 slot
   目标 15 秒内完成，需冷启动时最长等待 90 秒，超时退出码 6。
6. 先将 rollback target 置为 `ready` 且权重 100，再把当前 active slot 置为 `drain`。该顺序允许
   极短时间的双后端可用，但绝不允许出现两个后端都不可用的窗口。
7. 通过稳定入口发送带 controller probe 标识的无副作用健康请求，核对响应中的可信 slot
   header，同时用 HAProxy stats 确认 rollback target 可接收新连接、candidate 权重为 0。
8. 原子提交 `active_slot=rollback_target_slot`、`phase=rolled_back`、权重、generation 和
   action ID；保持本 release 的 rollback target 不变，并追加审计记录。
9. 返回 `result=traffic_restored`。若被回滚 slot 仍有活动流，返回
   `drain_pending=true`，后台只观察排空，不阻塞回滚命令完成。
10. 活动流归零后才停止被回滚 slot；控制器永不调用 `shutdown sessions server`。

HAProxy Runtime API 的多条命令不是数据库事务。控制器在写操作前先记录
`phase=rollback_applying` 和期望状态；任一步失败都重新读取 HAProxy 实际状态，确保至少一个
健康 backend 为 `ready`，然后返回退出码 7。控制器或 HAProxy 重启后，reconciler 根据
最新 `generation` 恢复期望状态。

### 6.4 快速回滚时延与故障收敛

- rollback target 已 warm 且健康时，从获得锁到新请求 100% 回到稳定 slot 的控制面目标为
  10 秒内；本地 Skill 含自动复核的目标为 20 秒内。
- rollback target 需要冷启动时，控制器最多等待 90 秒。超时保持当前健康流量不变，不能先
  把 candidate 权重降为 0 再等待 target。
- 本地 SSH 设置 keepalive，防止控制器已执行而客户端因静默连接误判；控制器仍以
  `action_id` 和 release 状态负责最终幂等。
- 回滚命令只等待“新流量已恢复”，不等待旧流排空。长 SSE/WebSocket 不会把紧急回滚阻塞
  数分钟。

| 失败点 | 控制器行为 | 是否自动重试写操作 |
|---|---|---|
| state/HAProxy 实际状态不一致 | 先 reconcile；无法确认时 fail closed | 否 |
| rollback target 不健康 | 保持当前流量不变并退出 6 | 否 |
| target 已 100、source 尚未 drain | 完成 source drain 后复核 | 是，幂等 |
| source 已 drain、target 验证失败 | 保持至少一个健康 backend ready，进入 `rollback_failed_safe` | 仅 reconcile |
| state 提交失败但 HAProxy 已切换 | 从 HAProxy 反查并补写同一 generation/action | 是，幂等 |
| 本地 post-check 失败 | 报告动作 ID和未知/不一致状态，不重复触发回滚 | 否 |

### 6.5 本地快速触发 Skill

本地 Skill 安装在：

```text
/Users/zhangyiming/.codex/skills/sub2api-rollout
```

它只调用云端控制器，不直接操作 HAProxy 或 Docker：

```bash
# 只读状态
~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --status

# 只读回滚预演
~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --dry-run

# 用户明确要求时一键回滚
~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --rollback

# 紧急场景最短入口，与上面完全等价
~/.codex/skills/sub2api-rollout/scripts/sub2api-rollback-now
```

Skill 对控制器输出做字段白名单，只显示 slot、权重、健康、active streams、版本、digest、
回滚许可和动作结果。实际回滚成功后，包装脚本会自动再执行一次只读 `status`，只有 active
slot 等于 rollback target、target 健康且权重为 100、candidate 权重为 0 时才返回
`verified=true`。若生产仍是当前旧单容器拓扑，返回
`unsupported_legacy_topology` 并停止；不得降级成重启单容器。

## 7. Migration 规则

蓝绿期间两个版本会同时访问同一数据库，必须遵守 expand/contract：

1. Release N 只新增 nullable 字段、表或兼容索引，旧代码继续可用。
2. Release N+1 在所有实例已使用新结构后停止读写旧字段。
3. Release N+2 才允许删除旧结构。

大型索引使用 `CREATE INDEX CONCURRENTLY`；长时间锁表或数据回填必须独立执行并监控，
不能藏在 API 容器启动路径里。migration 最终应改为显式 one-shot job，API slot 只验证
schema version，不自行改变 schema。

## 8. 资源与容量

按当前抽样，额外 API slot 的常驻内存远低于主机余量，单机蓝绿可行。生产配置仍需：

- 为 PostgreSQL/Redis 保留至少 2 GiB 安全余量；可用内存低于 2 GiB 不发布。
- candidate 启动后先保持权重 0，观察 2 分钟内存与 goroutine 是否持续增长。
- compact/超长输入会产生瞬时内存，预热必须包含真实长输入样本。
- 不把 10% canary 当容量压测；容量承诺另用固定 QPM/并发测试验证。
- API 两实例共享 Redis 并发槽位，但必须继续验证 scheduler/session affinity，避免测试请求
  因相同 body 固定命中单一账号而产生虚假容量结果。

## 9. 实施拆分

### Phase A：发布可观测性

- 增加 slot/version/digest 与 terminal outcome 记录。
- 增加按 slot 的 P50、真实 SLA、Cache 率查询与 JSON 报告。
- 实现门禁计算器及边界测试。

### Phase B：进程角色

- 增加 `api/worker/all` role。
- 审计所有启动服务并登记 `per_replica/singleton`。
- 为遗漏的 singleton job 补 leader lock，role 作为第一层保护，分布式锁作为第二层保护。

### Phase C：稳定入口与蓝绿 Compose

- 增加 HAProxy、blue、green、worker 服务。
- 使用 immutable digest、healthcheck、readiness、长连接 timeout 和 stop grace。
- 实现 rollout state 持久化及 HAProxy 重启后的 reconcile。

### Phase D：发布控制器

- 实现 `precheck/start-green/canary/promote/rollback/drain/report` 子命令。
- 任一步失败都保留现场、镜像、备份和报告，不删除 volume。
- 同一时间只允许一个 rollout，通过文件锁或 PostgreSQL advisory lock 防止并发发布。

### Phase E：一次性生产接入

当前应用直接占用宿主机 `8080`，第一次安装 HAProxy 与以后的日常发布不同。本实现先用
精确 PREROUTING 规则把新连接交给临时 HAProxy `18080`，旧连接继续由旧容器完成；完成真实
版本和无意义版本两轮验证后，再按 5.6 的故障可恢复流程把 `8080` 所有权交给 final HAProxy。
以后常规发布只改 runtime 权重，不再触碰监听端口或 iptables。

## 10. 验证计划

### 10.1 单元测试

- 30% P50、0.5 个 SLA 点、10 个 Cache 点的等号边界均通过，超出最小单位即失败。
- 百分比与百分点不混淆。
- 无样本、延迟数据、分母为 0、重复 request ID 均 fail closed 或正确去重。
- 客户端伪造 deployment header 会被覆盖。
- fallback 中间失败不重复计入最终 SLA。
- 最终 `HTTP 200 + response.failed` 即使同时带有前序 retry/fallback context，也必须以逻辑
  4xx/5xx 单独落库并覆盖 recovered 结果；成功恢复的中间失败仍不计入 SLA。

### 10.2 集成测试

- 实际请求分配接近 90/10，并且数据库 slot 与 HAProxy backend 一致。
- 10% 切到 100% 时，持续 5 分钟以上的 SSE 不断流、不重复、不缺 terminal。
- WebSocket、普通非流式、大请求体、compact 和 tool call 均跨切流通过。
- 旧 slot drain 后不接收新请求，活动连接归零后才被停止。
- HAProxy reload/restart 后恢复最近一次期望权重。
- worker 任意时刻只有一个 singleton executor；API 两实例都能正常记录 usage。
- migration 失败时 green 不进入流量，blue 不受影响。

### 10.3 故障注入

- green crash、readiness 失败、Redis 短暂失败、PostgreSQL 查询超时。
- HAProxy 到 green connect error。
- HTTP 200 后 `response.failed`、无 terminal、`stream_read_error`、客户端取消。
- green 指标写入延迟或完全缺失。
- drain 中存在长时间不结束的 SSE/WebSocket。

预期结果都是：停止晋级或把 green 新流量降到 0；既有连接继续排空；blue 不被提前停止。

### 10.4 生产演练验收

先用“相同版本的 blue/green”演练一次，以排除代理和流程本身的性能影响。验收报告至少包含：

| 项目 | 必填内容 |
|---|---|
| 构建身份 | commit、version、blue/green image digest |
| 流量 | 观察时间、blue/green 请求数、实际分流比例、请求分层 |
| 主门禁 | 两边 P50、SLA、Cache 率、差值、PASS/FAIL |
| 完整性 | HTTP 状态、terminal success/failure/incomplete、流内失败 |
| 性能 | TTFT P95、总耗时 P50/P95、QPS/TPS |
| 成本 | input/output/cache tokens、cache rate、实际成本 |
| 上游 | 429/529、fallback、账号请求分布 |
| 资源 | CPU、RSS、可用内存、磁盘、restart/OOM |
| 排空 | 切流时间、最后活动连接结束时间、是否发生强制终止 |

报告同时保存 JSON 原始数据和 Markdown 摘要到：

```text
/opt/sub2api/deploy/reports/<UTC timestamp>-<candidate digest>/
```

## 11. 发布完成定义

只有同时满足以下条件，发布才能标记为 `COMPLETE`：

1. green 已承接 100% 新流量并通过 10 分钟 soak；
2. 三项主门禁与全部辅助护栏通过；
3. previous stable 活动连接为 0，保持健康 warm rollback target；首次接入的 legacy 容器只在
   final HAProxy 就绪时停止，且容器和 immutable image 继续保留；
4. singleton worker 已切到目标版本并健康；
5. PostgreSQL、Redis、HAProxy 健康，持久化记录数量核对无异常；
6. 完整发布报告已落盘；
7. 回滚 digest、数据库备份和 rollout state 均可定位。

## 12. 设计审查结论

| 风险 | 处理结果 |
|---|---|
| 直接重建导致端口空窗 | 用稳定 HAProxy 入口消除 |
| 5 秒 shutdown 截断长流 | 先代理 drain，连接归零后再停；同时延长应用与 Compose grace |
| 双实例重复后台任务 | API/worker 角色拆分 + singleton lock |
| 双实例把账号并发翻倍 | 已确认并发槽位在 Redis；保留集成验证 |
| 只看 HTTP 状态导致 SLA 高估 | terminal outcome + HTTP 200 流内失败纳入真实 SLA |
| canary 请求结构不同导致误判 | 同窗比较、分层检查、最低样本数 |
| Cache 指标口径错误 | 使用 prompt token 加权公式并明确“百分点” |
| HAProxy 重启丢失动态权重 | rollout state 持久化并在启动时 reconcile |
| migration 破坏旧版本 | expand/contract + 显式 migration gate |
| rollback 强杀正在跑的 green 请求 | green 置 drain，已有流继续完成 |
