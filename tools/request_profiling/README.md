# 耗时分析

管理入口 `/admin/request-profiling`，API `/api/v1/admin/ops/request-profiles`。只记录元数据，不存请求正文、凭据或上游 URL。默认启用；设置 `GATEWAY_REQUEST_PROFILING_ENABLED=false` 并重启或重新部署应用可停止新采集，历史记录仍可查询。Compose 示例已传递此开关。

## 时间口径

- 从最早的请求日志中间件入口开始，包含后续鉴权、正文读取/解压/校验、bootstrap、审计、排队、选号、JSON 与协议处理、上游调用及响应处理。时间为同一进程的单调微秒。
- `total_us` 是**服务端处理总时间**，不是客户端网络 RTT；客户端断开后继续排空的时间也在其中。
- `first_semantic` 是原有上游 TTFT 判定点的入口相对时间，**不等于下行交付**。不改变或重新命名已有 `usage_logs.first_token_ms`。
- `downstream_first_output_us` 与 `downstream_complete_us` 只在已有实际下行 flush 之后记时。有效输出排除单纯心跳和推理事件；完成事件单独记录。当前明确接入原生 Responses、Responses 透传及 Responses→Chat 回退输出。其他路径的语义交付缺口保持显式，不用上游计时填补。
- 首次原始写入 / flush 可能只是心跳，不当成有效输出。写出只能证明本服务端的动作，不能证明远端浏览器或代理已经收到。
- 入口 context 取消时立刻记录取消观测点；之后后台继续处理、出现上游 completed 或成功 usage 都不覆盖连接取消状态。
- WebSocket 按**连接**查询，标明轮次、新建连接与请求尝试。连接内正常的新一轮不推断为重试；连接总时长不当成单次 response 的 P90。界面要求明确选择传输方式，默认 SSE。

## 长条与聚合

原始 span 保留重叠关系。绘制单根长条时，每个时间片分配给覆盖它的最窄 span，同宽时取后记录者；其余时间标为未归因。并行调用保留原始 span，长条不是因果推断。每条长条的片段总和严格等于总时间。

聚合基于筛选窗口内全部已存 profile，不只计算当前页。阶段均值为该阶段总量除以全部匹配请求数，总和等于总体平均时间；P90 从完整请求总时间独立计算，禁止相加各阶段 P90。

账号筛选包含失败或切换前参与过的账号。上游重试/换号需有前次失败证据；并行调用不自动推断重试。本机准入重选以 `local_account_admission` 单独记录、筛选和计数，不作为上游 429 或 fallback。

时间筛选使用记录完成时间。还未结束的请求不在此完成记录列表里。历史日志重建用 `evidence=historical` 与实测记录分开，原始重试次数未知时不显示为零次。

## 完整性与开销

每请求最多 512 个 span、128 个事件，超限显式记录 `dropped`；下行与取消的关键时间字段独立保留。复用已有有界异步运维日志 sink 与留存策略。原有非法鉴权/拒绝日志保护仍保留，不能把查询到的记录数当成所有入站流量。

页面显示 sink 健康数据；它们是整个日志 sink 的累计统计，不是本页面的请求丢失率。日志级别提高到 warn 不会关闭 profile 持久化。关闭专门开关才停止新采集。没有样本或交付打点缺失时显示未知，不宣称健康或客户成功。

`response_body` 覆盖收到响应头后到响应体读完/关闭的处理阶段，包含上游等待、接收、解析和下游转发，不能全部归为模型生成耗时。细分时优先使用已记录子阶段与实际交付节点。

## 本地验收工具

在独立本地 Sub2API 实例中创建 `.runtime/local-env.json` 和管理员 token 文件；这些文件被忽略，禁止提交。工具固定使用 loopback，不会修改生产环境：

- `mock_upstream.py`：9187 端口模拟 503、可修复 400 与有效 Responses SSE。
- `seed_local_scenario.py`：通过本地 8187 管理 API 建立独立测试分组/账号/Key，凭据仅写入 `.runtime`。
- `run_local_request.py --repair-retry`：真实穿过网关的 503 → 换号 → 400 → 同账号修复重试 → 有效输出/完成。
- `run_local_disconnect.py`：在有效输出前主动断开，检查稍后落盘的 profile，不用后台结果冒充客户完成。
- `import_history.py --source-dir <本地元数据证据目录>`：仅把已有日志区间重建导入隔离 QA 数据库，保留真实时间与缺口；不伪造函数级实测。

集成测试 `TestRequestProfilePostgresAggregationAndAttemptFiltering` 使用 `REQUEST_PROFILE_TEST_DSN` 指定 PostgreSQL，并只创建连接级临时表，不修改应用表。

尚未生产上线。只有本地完整回归和浏览器交互验收通过后才提交 PR，之后再向指定协调任务一次性交接。
