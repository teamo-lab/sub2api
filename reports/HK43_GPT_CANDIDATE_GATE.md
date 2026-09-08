# HK43 GPT 候选修复功能验收

本说明对应 2026-09-08 的两项恢复修复，只规定候选功能门禁；它不证明生产 SLA、P90 或 cache 已改善，也不授权绕过发布 lease 或真实 43→101 链路门禁。

## 候选来源

- 集成分支：`codex/recovery-integration-check`。
- PR #23 HEAD：`073c9b3d8095954d95bfcb543f0606a2e0c57260`，是集成分支的直接祖先，包含 type-only 规则匹配和裸 SSE 瞬态错误入口修复。
- PR #24 HEAD：`36825e065e2738ed666027a4609d47dc30ae305c`，原始两提交 `c05eb8232`、`36825e065` 在集成分支中为 `f98fbb013`、`af80f7467`。
- `git range-diff 658248bc4..36825e065 073c9b3..af80f7467` 对两提交均输出 `=`，确认 cherry-pick 未引入补丁差异。
- 组合回归：`backend/internal/handler/error_recovery_encrypted_integration_test.go`。只提交到集成分支，不混入两个独立 PR。

## 本地代码门禁

在 `backend` 目录执行，`GOTOOLCHAIN=auto` 已解析到 Go 1.27.0：

```sh
go test -tags=unit ./internal/service ./internal/handler -run 'Test(ErrorRecovery|CombinedErrorRecovery|OpenAIEncrypted|OpenAIStreamBareStructuredTransientError)' -count=1 -timeout=180s
go test -race -tags=unit ./internal/service ./internal/handler -run 'Test(ErrorRecovery|CombinedErrorRecovery|OpenAIEncrypted|OpenAIStreamBareStructuredTransientError)' -count=1 -timeout=180s
go test -race -tags=unit ./internal/service -run 'Test(RecoveryErrorCode|StreamFailoverEnvelope)' -count=1 -timeout=180s
go build ./internal/service ./internal/handler
```

组合测试从真实 `OpenAI Responses` handler 入口运行，依赖与上游均为进程内 fixture。它覆盖账号 A type-only 503 → 按配置切到 B → B 的流内密文拒绝 → 同 B 去掉被拒密文、保留摘要和用户消息 → 成功的完整组合。断言调用序列严格为 `[A, B, B]`，成功输出与终态各一次；挂起场景必须继承原始 1 秒预算，中断上游等待且只产生一个 `recovery_exhausted` 终态，不能重置到新增的 30 秒预算。

这些测试证明机制与边界，不等同于候选镜像/网络路径验收，更不能推导生产收益。

本轮执行结果：service/handler 相关回归通过（5.905s / 5.743s）；同组 race 通过（7.288s / 7.110s）；错误身份解析和流式错误信封的补充 race 通过（3.080s）；两个包 `go build` 通过。

## 现有 probe 的适用范围

1. `/Users/zhangyiming/.codex/skills/sub2api-rollout/scripts/sub2api-live-content-audit-probe.py`：已有的候选槽直连骨架。它读取槽的容器 IP 和 image，创建独占测试 group、只向私有 mock 发请求的 API-key account、测试 user/key，并在 `finally` 清理。当前实际场景只有 403 内容审计，断言不 retry、不改测试账号状态。**原样执行不能验收 PR #23/#24。**它仍会在生产依赖中创建和删除测试记录，因此不是零写入工具；当前其他 task 持有 lease 时不得执行。
2. `tools/gpt_pro_guard/hk43_native_error_probe.py`：从已有错误日志提取脱敏错误身份和数量，是只读取证工具，没有向候选发请求、没有验证恢复或预算，不能作为功能 gate。
3. `sub2api-release paired`：验证 HTTP 200、`response.completed` 和配对延迟，无法强制上游产生指定错误，不能替代下表的故障场景。

## 本轮新增的专用 probe

`tools/gpt_pro_guard/hk43_gpt_candidate_probe.py` 已准备好四类候选场景：type-only 503 有限尝试耗尽、首输出前裸 SSE server_error 切换成功、密文同号修复（普通及 passthrough 两条路径）、首输出后不可再切换。它读取现有全局规则推导次数和预算；规则缺失、同优先级歧义或预算无法完成精确验收时直接失败，不改全局规则。生产账号配置在数据库内转为哈希后比较，只输出变化账号 ID；credential、提示词、原始响应和全局规则正文不落账本。

账号创建会自动触发 `probe_ping` 工具能力探测。mock 单独成功应答并统计这些探测，不把它们混入恢复尝试次数。测试账号只加入独占测试组。删除前按创建账本 ID 和随机名称再次核对，只允许删除本次 fixture；不提供修改生产账号或全局规则的路径。创建结果不确定时保留 pending_creation 账本，gate 判为失败。

本地 `python3 -m unittest test_hk43_gpt_candidate_probe -v` 的 10 个安全/预期测试通过。**尚未执行任何云端 fixture 或候选功能请求**。以下命令仅供持有 lease 的发布 owner 在已经核对候选 image 和 binary 后调用；脚本的 `--release-owner` 是审计记录，不能替代外层 fleet lease 校验：

```sh
python3 tools/gpt_pro_guard/hk43_gpt_candidate_probe.py \
  --slot blue \
  --expect-image sha256:REPLACE_WITH_VERIFIED_IMAGE_ID \
  --expect-binary-sha256 REPLACE_WITH_VERIFIED_BINARY_SHA256 \
  --release-owner REPLACE_WITH_CURRENT_LEASE_OWNER \
  --model gpt-6-astra \
  --execute-isolated-fixtures
```

账本输出到 `/opt/sub2api/deploy/rollout/runtime/gpt-candidate-<随机值>.json`。`ok=true` 同时要求全部场景通过、现有全局规则未变、原生产账号配置未变、所有已确认创建的 fixture 清理完成且没有创建结果不确定的记录。它只覆盖上述四类场景；下表的取消、opaque 状态及首输出后长流边界仍由本地回归保证，在对实际二进制完成相应场景前不能声称候选全部矩阵通过。

## 候选二进制验收方案

优先使用候选不可变 image 的隔离副本：同一 image/digest 和 `/app/sub2api` 二进制 SHA256，独立临时 PostgreSQL、Redis、应用配置及 mock 网络，且不加载生产环境变量、账号、Key 或网络访问权限。测试数据库仅含合成 fixture。这样恢复错误、取消、超时或临时不可调度只作用于测试账号，生产账号字段与请求完全不受影响。它验证候选二进制逻辑；发布 owner 仍须在实际候选槽完成健康、digest 与真实链路 gate。

若发布 owner 选择直接在 0% 候选槽复用已有 probe 骨架，必须同时满足以下隔离条件：

- 只创建带本轮随机标记的独占测试 group/account/user/key；测试账号不加入任何生产分组，测试 key 只能访问该组，API-key 仅指向该机私有 mock。禁止修改任何生产账号或调用后台“测试账号连接”。
- 账号 A、B 分别绑定 mock 的独立 URL 路径，记录仅含合成请求的安全摘要、尝试次序、内容保留断言和时间。测试各组只存在专属账号，避免误落真实上游。
- 最好只读复用已存在且已审核的恢复规则。当前 `ErrorPassthroughRule` **没有 group/account scope**；不能为了测试修改全局现有规则。必须注入测试规则时，规则需 `match_mode=all`，关键词匹配本轮不可预测随机标记，并将同一标记加在 mock 错误 message 中；恢复模型和账号类型也限于测试范围，`skip_monitoring=false`。新规则只按本轮创建 ID 清理；不得覆盖或删除原规则。
- 每场景从独立 fixture 开始，避免上场测试的临时模型错误/路由冷却干扰后续尝试；不通过清理生产 Redis 或修改真实账号消除干扰。
- 执行前后只读核对生产账号的 status、schedulable、priority、concurrency、模型映射、proxy、Base URL 及 Key 指纹；任何漂移保留现场并停止。不要打印凭据、请求内容或原始账号记录。
- 清理仅依据本次创建成功后记录的精确 ID。清理失败须报告残留 ID 和非敏感错误，并判定 gate 失败，不能输出“全部通过”。

## 必须执行的故障矩阵

| 场景 | mock 行为 | 候选必须满足 |
|---|---|---|
| type-only 503 恢复成功 | A 503，仅 `error.type=service_unavailable_error`；B 成功 | A、B 各一次，客户端成功且没有透出已恢复错误 |
| type-only 耗尽 | A、B 均上述错误 | 严格按规则预算结束，不能走旧默认八次尝试；一个终态 |
| 首内容前裸 SSE 错误 | HTTP 200 + `event:error`，code 或 type 为受限瞬态错误；B 成功 | 命中有限恢复并成功，不将 HTTP 200 误计成功 |
| 已输出、参数、内容审计或未知错误 | 分别先输出 delta 再错误，或永久/未知身份 | 不 replay、不切号，不重复可见内容或错误终态 |
| 密文同号修复 | B 流内 `invalid_encrypted_content` 后返回成功；输入带有效明文 reasoning summary | B 总共两次；只去被拒密文，保留 summary、消息和工具配对 |
| 不可修复密文状态 | opaque compaction、空 summary、`previous_response_id` 或 item reference | 不丢弃不透明状态，不新增重试 |
| 两修复组合 | A type-only 503 → B 密文错误 → B 成功 | 次序 A、B、B，唯一答案和 completed，保留消息摘要 |
| 组合预算与取消 | 上一组合第三尝试挂起/客户端取消 | 原外层截止时间生效、等待被取消、无第二个终态 |
| 首内容后正常长流 | 修复后首内容及时出现，后续流长于首输出预算 | 正常排空，不因首输出预算误杀流 |

所有矩阵结果需绑定 source commit、image/digest、binary hash、场景 ID、尝试序列、终态计数、耗时与清理结果。先运行失败路径再进行任何生产策略启用，不能用 health 或后台连接测试替代。

## 生产效果验收

候选功能 gate 通过后，再由唯一发布 owner 按既有流程上线，保留真实 Key 和公网/私网链路门禁。上线后的分钟诊断分别对 GPT API、GPT Harness 的 SSE/WS 按 client 请求最终结果统计。用完整且不含写入时刻的窗口比较：原 type-only、裸 SSE、密文拒绝各减少多少最终失败，恢复增加多少尝试/累计等待，已有对话同号和换号 cache 各自变化。

内容审计、客户端参数错误仅在具有明确身份且不是网关引入时按审核规则排除，必须同时保留原始错误数量。Client SLA 99.5% 的达成需足量真实生产窗口证明；本地或 mock 成功不算该目标完成。
