# 统一恢复策略等待补点

PR34 的小流量真实样本发现另一条优先于 legacy pool retry 的路径：ApplyErrorRecoveryAfterAttempt 命中恢复规则后，在 error_recovery_runtime.go 内直接等待500ms，再返回 ErrorRecoveryRetry。PR34 对 legacy pool timer 的补点不会执行，因此这段等待仍归入未归因。

实际样本 client e4c909de-f271-4974-adcb-5eedfe8601cb / request1673e212-3dc0-45b7-b8e6-03a6fbc0799f：首个上游错误5.828783s，第一次响应正文关闭5.828995s，下一次选择账号6.334625s，retry事件6.411252s；这约500ms间隔没有retry_backoff span。完整路径23→23→27、27.576s，completion_written。

本补丁仅在恢复策略已有 timer 等待周围记录 retry_backoff，正常重试、取消、预算退出都通过defer闭合。没有修改等待时长、预算、规则匹配、重试次数或渠道决策。增加正常恢复和取消两项定向测试，确认可见时间线中的retry_backoff等于原始span时长。

验证：`go test ./internal/service -run 'TestErrorRecovery|TestRecoveryErrorCode' -count=1` 通过。

生产状态：PR34 r2已因独立的account23管理API配置漂移而回滚到PR33，不把该失败门禁当通过。本补点需与PR34一起用于下一轮候选；同时先解决发布期间外部账号写入冲突，再按原0/10/100门禁发布，保持3,38采集计划及6h/32MiB边界。
