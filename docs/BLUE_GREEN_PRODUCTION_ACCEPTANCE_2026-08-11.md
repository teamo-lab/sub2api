# Sub2API 单机蓝绿发布生产验收报告

验收日期：2026-08-11（Asia/Shanghai）
生产入口：`http://49.51.251.58:8080`
结论：**蓝绿基础设施验收通过并已正式接管生产流量；旧容器和旧镜像均保留。**
后续发现的 compact 流内 SLA marker 修复已停放在 green（权重 0），因共享上游 overload
未通过发布门禁，未向客户放量。

## 1. 验收范围

本次验收覆盖：

- HAProxy 固定入口与 blue/green API slot；
- singleton worker 与 API 进程职责隔离；
- 10% canary、100% promotion、drain、complete 状态机；
- P50、SLA、Cache 率及辅助错误门禁；
- 流中 `response.failed` 的 SLA 可观测性；
- HAProxy 部分写入失败后的权重恢复；
- Runtime 权重持久化与 HAProxy 重启恢复；
- 一键回滚 Skill、回滚幂等性和长流排空；
- 同镜像无意义发布及生产 `8080` 入口接管。

本方案只解决单机内应用发布造成的中断，不消除宿主机、Docker daemon 和机房级单点。

## 2. 最终生产状态

入口接管于 `2026-08-11 04:29` 完成：

| 组件 | 状态 |
|---|---|
| `sub2api-haproxy-final` | 健康，直接监听宿主机 `8080` |
| `sub2api-blue` | 健康，API 权重 100，console 权重 100，restart 0 |
| `sub2api-green` | 健康，drain，权重 0，restart 0 |
| `sub2api-worker` | 健康，singleton worker，restart 0；公网 API 路由被拒绝 |
| PostgreSQL / Redis | 健康 |
| 旧 `sub2api` 容器 | 已停止但未删除，restart 0 |
| 旧稳定镜像 | 保留：`sha256:8099b8d3a6900cb652d9d93a7ed0ac6a9651d2a826f0a298d173efa9faf71153` |
| 临时 HAProxy | 保留在 `18080`，不再承接公网入口 |
| iptables 接入规则 | 通用重定向 0 条，测试来源规则 0 条 |

当前稳定发布为 `release-20260811-noop1`，稳定 slot 为 `blue`，镜像为：

```text
sha256:f12a513e4d04db07094650166d1c44cbeb8dc3aad1f7d11655abc5a3c05eb780
```

`/health` 从本机与公网访问均返回 HTTP 200，并带有：

```text
X-Sub2API-Deployment-Slot: blue
X-Sub2API-Process-Role: api
X-Sub2API-Release-Id: release-20260811-noop1
```

## 3. 发布门禁结果

门禁定义：

- candidate TTFT P50 相对 baseline 上涨不超过 30%；
- candidate SLA 相对 baseline 下降不超过绝对值 0.5 个百分点，且绝对 SLA 不低于 99.5%；
- candidate Cache 率相对 baseline 下降不超过绝对值 10 个百分点；
- scoped 窗口内 upstream 429、529、5xx 必须为 0；
- 指标缺失、样本不足、容器不健康或 restart 均 fail closed。

### 3.1 真实版本三轮配对门禁

每轮 blue/green 各 200 请求，使用相同请求序列和 8 个固定 cache shard：

| 轮次 | blue TTFT P50 | green TTFT P50 | green 变化 | blue Cache | green Cache | terminal |
|---|---:|---:|---:|---:|---:|---|
| 1 | 1.532 s | 1.561 s | +1.9% | 86.59% | 87.03% | blue 199 complete + 1 failed；green 200 complete |
| 2 | 1.437 s | 1.405 s | -2.2% | 87.47% | 87.47% | 两槽均 200/200 complete |
| 3 | 1.414 s | 1.506 s | +6.5% | 87.47% | 87.47% | 两槽均 200/200 complete |

三轮 P50 和 Cache 均在门槛内。第一轮 blue 的流内 `response.failed` 暴露了旧观测口径漏记问题；本次实现已把该 terminal 纳入 `ops_error_logs` 和真实 SLA，而不是把 HTTP 200 当作成功。

### 3.2 全量 soak

真实版本全量窗口记录：

- terminal outcomes：240；
- 成功：237，失败：3；
- TTFT P50：1382 ms；
- Cache 率：`3,874,688 / (1,445,404 + 3,874,688) = 72.83%`；
- upstream 429 / 529：0 / 0；
- 数据库记录实际成本：`$2.0869872`。

另一次独立生产观察得到 205/205 完成、TTFT P50 约 1539 ms、Cache 率约 85.6%，无 429/529。

## 4. 无意义发布验证

无意义发布将同一个 immutable image digest 从 green 重新发布到 blue。控制器要求同镜像证据至少持续 5 分钟、两槽至少各 10 个完整 terminal，并对证据文件做 SHA-256 审计。

| 指标 | stable green | candidate blue | 结果 |
|---|---:|---:|---|
| terminal | 10/10 complete | 10/10 complete | 通过 |
| TTFT P50 | 1.421 s | 1.296 s | candidate -8.8% |
| Cache 率 | 69.88% | 61.15% | candidate -8.74 个点，通过 |
| HTTP / 流 terminal | 10 个 HTTP 200 / 10 complete | 10 个 HTTP 200 / 10 complete | 通过 |

证据摘要：

```text
ad7dce11f89a5ac2770fdc13bf83c25f5a52af406a5327f7141fdbd08a88c417
```

同镜像全量自然流量窗口出现 6 条 provider 5xx，错误正文均为上游 `servers are currently overloaded`。两槽镜像完全相同，且相同错误在切换前后的稳定槽均出现，因此该窗口被标记为共享上游容量事件，不作为版本回归通过样本，也没有被隐藏为成功。

## 5. 长连接与无感切换

两次 SSE 跨权重切换均完整结束：

| 场景 | 持续时间 | 事件数 | terminal | 结果 |
|---|---:|---:|---|---|
| 真实版本 promotion | 55.624 s | 2165 | `response.completed` | 切权后原 slot 继续服务，排空至 0 |
| 无意义发布切换 | 50.739 s | 1984 | `response.completed` | marker 完整，排空至 0 |

控制器只改变新请求权重；已有 SSE/WebSocket 连接不会被 shutdown。slot 仍有活动流时只进入 drain，不允许强停。

## 6. 回滚验证

无意义发布 canary 期间执行过一次真实一键回滚：

- 回滚前：blue 10 / green 90；
- 回滚目标：控制器固化的 immutable `rollback_target_slot`；
- 结果：`traffic_restored`，自动复验通过；
- 耗时：9 秒；
- 已有流：保留到自然结束，没有调用 `shutdown sessions`；
- 恢复后继续完成无意义发布，证明回滚不会破坏后续状态机。

正式接管 `8080` 后再次执行 dry-run，结果为 `ready`。本地快速入口：

```bash
~/.codex/skills/sub2api-rollout/scripts/sub2api-rollback-now
```

Skill 会调用云端控制器并自动做回滚后验证；禁止手工交换 slot、直接写 HAProxy 权重或自动恢复数据库。

## 7. 入口接管验证

最终入口接管采用 fail-safe 顺序：

1. 确认 release complete、双槽健康、权重 100/0、旧服务健康；
2. 连续三次确认旧服务没有直连 `8080` established connection；
3. 保留临时 HAProxy 和转发规则，停止但不删除旧容器；
4. 启动并验证 singleton worker；
5. 启动最终 HAProxy，验证本机 `8080/health` 命中 stable slot；
6. 原子切换 Runtime socket，写入 `entry_mode=direct`；
7. 最后才删除 iptables 通用/测试重定向规则。

任一步失败都会尝试恢复临时转发、旧容器、Runtime socket 与入口前状态。最终切换耗时约 22 秒；这段时间新连接仍由临时 HAProxy 承接，已有连接也继续留在临时 HAProxy，因此没有入口空窗。

接管后首个 2 分钟短窗 smoke（样本只用于入口正确性，不替代性能门禁）：

- 3/3 terminal 成功；
- upstream 429 / 529 / 5xx：0 / 0 / 0；
- 最终 HAProxy、blue、green、worker restart count 均为 0。

窗口成熟到 8.5 分钟后，真实 terminal 口径为 10 个 outcome、9 成功、1 失败。失败是
HTTP 200 SSE 内的 provider HTTP/2 `INTERNAL_ERROR`，没有 usage 行。旧门禁 SQL 只筛
`status_code >= 400`，会漏掉这类记录；验收因此没有沿用早期的 9/9 结果，而是修正为
9/10，并将该事件计入 `upstream_5xx=1`。

切换后资源快照：

| 组件 | CPU | 内存 |
|---|---:|---:|
| blue | 1.04% | 76.29 MiB |
| green | 0.43% | 50.79 MiB |
| worker | 0.30% | 24.45 MiB |
| final HAProxy | 0.08% | 75.45 MiB |
| PostgreSQL | 0.38% | 196.7 MiB |
| Redis | 0.24% | 6.52 MiB |

主验收结束后，canary 专用 API Key 与测试用户均已设为 `disabled` 并软删除；2308 条历史
usage 记录保持不变。后续 SLA marker candidate 使用的第二个临时身份也已同样清理，182 条
usage 记录保持不变。两次 auth cache invalidation outbox 均已处理完毕，包含明文测试密钥的
临时文件均已从云端删除。

## 8. 已验证的失败保护

- HAProxy API/console 权重分步写入时，任一写入或复验失败会按快照以“先 ready、后 drain”恢复；
- 每次切权、回滚和 reconcile 后持久化 `show servers state`，HAProxy 重启不会回到静态默认权重；
- rollback target 在 release 生命周期中不可由简单 slot 互换推断，避免二次回滚反向切流；
- schema 不兼容、指标缺失、低绝对 SLA、provider 429/529/5xx、容器 restart 均阻止晋级；
- 入口接管失败恢复旧服务，但不删除容器、镜像、volume、备份或数据库；
- worker role 的 `/health` 返回 200，但 `/v1/responses` 返回 503，证明 worker 不会误接公网 API。

## 9. 测试结论与遗留风险

蓝绿发布链路、无意义发布、真实一键回滚、长连接排空和最终入口接管均已完成，满足当前单机发布无感目标。旧容器、旧镜像和临时 HAProxy 暂时保留，作为最坏情况下的恢复资产。

仍需明确的风险：

- OpenAI OAuth 上游存在独立的间歇性 overload；蓝绿发布无法消除 provider 容量问题；
- 当前仍为单机部署，宿主机故障会同时影响 HAProxy、两个 API slot、worker、PostgreSQL 和 Redis；
- 保留 green 与临时 HAProxy 会增加少量常驻内存，但换来快速回滚与排障能力；
- 后续 migration 必须继续遵守 expand/contract 兼容规则，否则控制器应禁止回滚。

## 10. 验收后发现的 SLA marker 问题

最终入口接管后的成熟窗口暴露了一个观测问题：compact keepalive 已提交 HTTP 200 后，
`response.failed` 使用了 `MarkOpsStreamError`，其 `CountTowardsSLA=false`。中间件随后优先
记录 provider attempt，导致数据库行保留 wire status 200，正式门禁漏算。

已完成两层修复：

1. `writeOpenAICompactSSEFailureMessage` 改用 `MarkOpsStreamFailure`，把 intended 502 和
   `CountTowardsSLA=true` 写入最终错误标记；
2. 门禁 SQL 增加旧 producer 防御：provider 状态 200、且同一请求没有最终 usage 行时，
   仍作为 terminal failure，并纳入 `upstream_5xx` 辅助护栏。

修正后的 SQL 已在云端生效，并对事故窗口复算为：

```text
terminal_outcomes=10, successes=9, failures=1, upstream_5xx=1
```

应用修复镜像已构建并以 `0.1.172+bluegreen.4` 部署到 green，digest：

```text
sha256:91e8e49c367fa3c49a5418f811693f89a025da43dc4a072a6dd24909e2090b74
```

green 权重保持 0，health 正常、restart 0，rollback target 为仍在服务的 blue。green 单请求
smoke 为 1/1 complete；但两轮低速配对测试分别在总 40 QPM 和总 20 QPM 下遇到两槽共享
provider overload。按 fail-closed 规则，窗口作废，candidate 不晋级。当前状态明确为
`staged`，不是 `complete`；生产仍为已验收的 blue 100%。

配对任务中还发现 SSH 断开只杀包装 shell、没有回收 Python 子进程。脚本已增加
`EXIT/HUP/INT/TERM` 清理 trap，并用两个 dummy 子进程验证 `SIGHUP` 后均被回收。该问题只
影响测试编排，未改变生产权重。
