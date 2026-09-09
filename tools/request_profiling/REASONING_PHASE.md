# Reasoning 可观测事件阶段

在 PR36 的首有效输出前/后边界内，进一步单列 reasoning 事件阶段。聚合与详情均使用同一份非重叠 segments；不是把一个 reasoning 总数再次叠加到原响应体总耗时上。

范围：OpenAI Responses 原生 SSE、Responses 透传 SSE、Chat Completions 转 Responses SSE。仅消费既有流处理代码已经解析的事件类型与 item.type，不重新读响应体、不记录推理文本/item ID/工具参数，不改变输出内容、flush、重试、超时或计费。

口径：

- 收到 reasoning item.added、reasoning 文本/summary 等事件，开始观察 reasoning 阶段。
- reasoning done、非 reasoning 的输出 item/文本/工具/音频事件或终态结束当前阶段；中途再次收到 reasoning 事件可重新开始，因此允许与正文交错。
- 心跳、response.created / in_progress 等前导控制事件不切换阶段。
- 只有开始和结束边界都被观测到，才把区间与对应 attempt/account 的 response_body 取交集；缺少结束边界仅保留未闭合原始 span，不推定 reasoning 时长。
- 区间仍按成功下行首有效输出 flush 切成 reasoning_observed_before_output / after_output，未观测输出及边界未知分别保留。所有分段之和保持请求总时长。
- 这是从事件边界观察到的墙钟阶段，包含此阶段内等待与转发，**不等于上游模型实际计算时间**。只有 reasoning 配置或 usage.reasoning_tokens 而没有事件，不能还原推理时长；首个 reasoning 事件之前的隐藏推理仍可能落在等待区间。
- 原始响应体及 reasoning span 可能重叠，仅供排查；统计使用去重后的 segments。旧记录不回填。

验证：

- requestprofile race 测试通过，覆盖首输出边界、reasoning 与 response_body 交集、不同尝试不串线、未闭合区间不归因及总时长守恒。
- service TestProfileReasoningBoundary 与真实 handleStreamingResponse 流处理回归通过：通过 io.Pipe 逐段送入 reasoning→正文→reasoning→正文→completed，首输出前/后 reasoning 均记录，元数据不含推理正文。
- 前端 build、定向 ESLint、10 个定向测试通过。
- Chrome 本地真实页面组件 http://127.0.0.1:5201/output-qa.html，合成 API 样本验证点击首输出后 reasoning、放大交付前和非流式未知边界，截图已目视检查。两次初始 Chrome 连接超时；编译结束后恢复原生 Chrome 控制并完成验收。合成样本不作为生产性能证据。

未部署生产；与 completed 后 drain 导致 usage 缺失的修复分开，由「43账号守护策略」统一安排发布。
