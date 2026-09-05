# 可配置错误恢复规则

在原错误透传面板增加 recovery_policy（默认空，旧规则行为不变）。当前执行范围为 OpenAI HTTP Responses、Messages 适配入口及 Chat Completions；包含 HTTP 错误与 SSE 终态错误，不覆盖 WebSocket。

先匹配既有平台/状态码/关键词条件，再按实际出错账号的 oauth/apikey 类型、精确请求模型及原始结构化 error.code / response.error.code 匹配。不扫描请求正文，不将所有502/503视为过载。

模式包括 default、return、limited。limited 配置额外同账号重试次数、最多换号次数和1–120秒的累计恢复预算。第一次命中后冻结规则；预算从第一次错误开始，跨账号共享，覆盖选号、退避和后续上游请求。有效输出开始后停止计时，不取消正常长流。预算到期通过显式上下文标记穿过原上游取消隔离，真实取消挂起请求。次数或预算耗尽后，返回原始错误 code 和消息及 recovery_exhausted；已提交 SSE 则发送 error 事件。账号状态、并发、优先级和代理均不修改；错误监控保留。

本次生产配置：101 OAuth，502/503 + server_is_overloaded/slow_down，同账号重试1、换号1、预算10秒。43 API-key，503 + server_is_overloaded/slow_down，同账号重试0、换号1、预算10秒。43的10秒从它第一次收到过载开始，不能理解为跨两台服务器的全链路10秒。

数据库迁移235只增加可空JSONB列，旧程序可以继续运行。所有规则在候选验证完成后才启用。禁用新规则即可恢复原重试逻辑；代码回滚保留新增列，不恢复数据库。

验证：真实handler的OAuth/API-key、HTTP/SSE调用次数与挂起取消；独立规则匹配、错误code隔离、客户端断开、SSE终态、有效输出取消预算；旧透传与首输出超时回归；前端build；原生Chrome真实组件新增/编辑/缺少code阻止保存及截图检查。预览使用本地内存API，不作为生产链路证据。发布各阶段另执行43容器网络命名空间内的原Key私网/公网真实模型门禁。
