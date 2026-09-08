# GPT Harness 长请求失败后的受控换号候选

本候选默认关闭，尚未部署或启用。应用层不硬编码机器的数据库账号/分组 ID；初次生产实验仅限真正 43 (`ins-4r9sd5og` / `43.159.0.162`) 的 **#22、认证 API Key group 3**，唯一实验范围见 `hk43_late_failure_switch.experiment.json`。该文件只是准备清单，不执行模型请求或配置写入。

## 问题与行为

43 当前 rule 4 在长时间失败后仍优先同号重试。在真实 handler 的可控 fixture 中，A 首次等待300秒后失败，再次 A 挂住，原策略到恢复30秒耗尽才返回502；同样预算下跳过 A 的重试，现有合格 B 在2秒后完成。生产案例报告见 root 的 `reports/HK43_HARNESS_RECOVERY_0435_20260909.md`。此项证实机制，不代表已取得真实生产收益。

启用后仅在以下条件全部成立时，跳过该次同号 Retry，直接进入原恢复引擎的 Switch 分支：

- 本次实际选中的 OpenAI API-key Account.extra 中，`openai_late_failure_switch_enabled` 必须是布尔 `true`，`openai_late_failure_switch_group_ids` 必须是非空的正整数数组，且包含认证 middleware 提供的 API Key GroupID。缺失、字符串、分数、非法数组不启用；不读取请求自报的 account/group/开关。
- 当前是请求的首次真实 Forward，也是首次匹配恢复规则的失败。之前的本机容量探测不计为 Forward；此前若已有上游 Forward 或恢复 state，就不激活本策略。
- 实际 Forward 用单调时钟计得的耗时至少 `max(60秒, 匹配规则的恢复预算)`。60秒是初始保守实验下限，集中定义于 `OpenAILateFailureSwitchMinWait`，extra 无法降低它。未来放宽需单独验证与审查。
- 已存在且匹配的规则必须是 limited、有同号重试额度且有剩余 switch；handler 自身也还有 switch 额度。仅处理原规则已经允许恢复的502/503/504，不增加错误类型白名单；429行为保持不变。
- 原始请求 context 与当前 context 有效，且原有 handler/service 安全边界确认没有已提交答案。服务已认证可恢复的仅推理输出保持原有边界；已提交答案和永久错误不得重放。

实现没有第二套 loop 或恢复 state。仍由原 rule 固化一个恢复 deadline、监听原请求取消、控制 account-switch 计数；跳过 A 的重试不伪记成一次实际 retry，不改缓存中的全局 rule，不延长预算，也不修改 B 后续失败的既有策略。成功与失败后的原用户配额、定价、利润终检、模型/协议选择、slot、usage 均沿用现有路径。

事件 `openai.late_failure_retry_skipped` 记录 account/group/rule、首个 Forward 耗时、有效门槛、预算与 switch 次数。它只表示跳过同号重试并选择换号，不代表备用已经接住；实际收益须关联后续最终 usage/error。

## 验证与生产实验约束

本地真实 Responses 与 Chat handler 用同一份 rule4（包括原 `same_account_retries=1`），仅改变账号 extra：关闭时 `[22,22] / 330秒 / 502`，开启时 `[22,23] / 302秒 / 200`。测试使用 `testing/synctest` 保留300秒首次等待、30秒恢复预算、500ms退避，并验证60秒下限及120秒预算取较大值；HTTPUpstream、Redis和持久化使用本地 doubles，没有真实模型流量。

范围/边界覆盖包括缺失及错误类型开关、其他账号未启用、认证group不匹配、请求body/header伪造group、短失败、429字节与调用序列不变、没有switch、已输出答案、取消、用户并发准入拒绝、备用利润不合格、原恢复 state 不重复跳过、不更换预算/timer、不修改全局 policy，且容量重选共享原 switch 计数。

2026-09-09 本地验证已通过：新增42个真实 handler 子用例、原68个容量重选 handler 子用例和原错误恢复 handler 回归一起通过；service 的范围/门槛/唯一预算测试及原错误恢复回归通过。相同相关集合的 `-race` 复核通过（service 8.725秒、handler 7.840秒）。这些结果不替代候选二进制或生产链路门禁。

应用支持按未来明确授权配置其他实际账号或group，但这不扩大本次实验范围。后续配置执行必须读取清单并只写真正43的 #22 extra 两个字段、白名单固定 `[3]`，保存精确写前字段和读回结果；任何其他账号启用都属于另一次独立授权实验。不得修改全局 rule4、账号并发/优先级/状态/模型/代理，也不得改变正在运行的 API #23 容量实验。

必须先完成独立候选二进制和原链路门禁，再在小流量启用。以首次失败的 eligible cohort 统计最终 SLA、全链路延迟、已有会话 cache、备用失败率与成本。没有 eligible 样本只是没有试验机会，不得把自然低峰当作收益。关闭布尔开关立即恢复旧重试策略；已在途的请求使用已经创建的原预算继续完成，不终止流。
