# 43 client 请求内的 retry / fallback 耗时补点

2026-09-09 用户明确关注：43 向 101 收到 429 后重试同一渠道，再切到其他渠道；所有尝试与等待须属于同一 client 请求。不展开下游服务内部尝试。

已确认两个独立缺口：

- 生产采集仅 group 38，其两个渠道同号重试配置为 0。用户举例的 101 渠道和账号 33 属 group 3；101 渠道配置 429/502 同号重试上限 2。group 3 没被采集，不能拿 group 38 的轨迹代表其行为。
- OpenAI Responses/Chat Completions/WS 入口及 Images、Alpha Search、Grok Media 部分直接 timer 等待没有 retry_backoff span；WS 重连等待也未细分。

本补丁只给上述现有等待加 retry_backoff span，保留 timer、取消、返回路径和重试策略。原有每次 HTTP attempt、同号 retry、账号 fallback 记录与 client 请求关联保持不变。没有改模型、账号、并发、路由或重试次数。

验证：

- 定向 handler/service/requestprofile 测试通过；取消期间等待及时结束且 span 闭合，不新增尝试。
- HTTP 模拟链：22×3 返回429 → 33×2 返回503 → 34成功，记录6次attempt、3次retry、2次fallback、3段等待；阶段总和等于请求总耗时。
- 完整本地网关实测：创建独立本地 QA 分组与三个 localhost 上游，真实 /v1/responses 返回200+response.completed。request_id f415215c-27d2-4933-95d0-6ccb6f8aef4c，6次调用/3 retry/2 fallback，等待501023、500691、500772 µs；total与segment sum均1682289 µs，client_outcome=completion_written，dropped=0。不是生产合成故障测试。

43 后续采集部署范围为3,38，保留6h/32MiB队列/128KiB单记录上限；先10%候选检查group3实际payload、队列丢弃和写入失败，再决定100%。采集扩展属于部署配置，本补丁不修改默认关闭和白名单语义。初始测量：group3约1308usage/5m，group38约342/5m，38平均profile5646B；这只是容量估算参考，不替代group3自身小流量数据。历史未采集请求无法补成实测轨迹。
