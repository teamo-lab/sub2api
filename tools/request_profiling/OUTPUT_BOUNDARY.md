# 响应流按首有效输出拆分

按已有的 `downstream_first_output_us`（成功下行 flush 后观测）拆分 `response_body`，聚合与单请求时间线直接使用同一份非重叠 segments。原始 spans 保留；所有阶段时长之和保持请求总耗时，retry/fallback 的 attempt/account 归属保留。没有新增正文解析、网络读写或计时器，不改变路由及重试策略。

- `response_body_before_output`：首次有效输出写出前的响应体处理。
- `response_body_after_output`：首次有效输出写出后的响应体处理。
- `response_body_no_output`：串行 SSE 且已有交付观测支持，但请求结束仍未观测到有效输出。
- `response_body_output_unknown`：非流式、WebSocket、多上游并行或交付观测未覆盖，不能推定尚未输出。

沿用实际服务中有效输出定义，包含文字和工具调用；心跳、HTTP 响应头、上游 first_semantic 不作为交付边界。服务端写出不等于客户已收到。输出前仅表示可进一步评估重试，不能代替协议提交状态、错误策略、客户端取消和重试预算判断；已产生上游副作用也需单独判断。输出后不应直接重放整个请求。

仅新结束请求落盘时生成拆分；旧数据保留原始 response_body，不凭缺失打点自动推断。WS 需要按 turn 定义独立交付边界，本变更不拿首轮边界覆盖整个会话。未知、未观测到输出与未归因互不混用。

验收：`go test -race ./internal/pkg/requestprofile -count=1`、middleware RequestProfile 定向测试通过；覆盖连续 retry 的归属、各分界点时长守恒、心跳/上游事件不当成输出，以及 unknown/no-output 区分。前端 9 个定向测试、build、定向 ESLint 通过。Chrome `http://127.0.0.1:5199/output-qa.html` 使用真实页面组件和合成 API 样本验收：点击输出前分段、放大交付前、切换非流式未知边界；两种状态均截图并目视检查，样式和交互正常。该本地样本仅验证展示，不作为生产性能证据。
