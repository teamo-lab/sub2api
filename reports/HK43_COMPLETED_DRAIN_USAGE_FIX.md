# completed 后 drain 终止与 usage 保留

候选基于 PR36 的生产候选 ec384ee089d625cf1c6e200152155cb566154b23；只修改原生 Responses 的结果保留与空闲收尾，不调整超时长度、账号、规则或重试次数。

## 已复现

- 服务真实 Forward 收到 output_text.delta + response.completed（含17 input/3 output tokens），上游保持连接不结束。旧版在 idle timer 到期后返回 stream data interval timeout；Forward 丢弃 parser 已解析结果，不能交给 handler 记 usage。
- 部分输出后 response.failed 带 usage，旧版同样返回 nil/error，丢弃计量。

原始失败日志保留在 /tmp/completed-drain-red.log。它证明代码机制，不证明生产样本必然走同一分支。

## 修改

已有终态的流在 idle 收尾时走既有 finalizeStream，保留 completed/failed 的终态语义，不追加新的 stream_timeout。普通不再恢复的纯文本流错误只在已有非零 token usage 且没有专属计量所有者时返回 OpenAIForwardResult 和原 error；可 fallback 的 UpstreamFailoverError、无计量失败、cyber policy 和媒体/搜索失败仍返回 nil，避免重复计费并保持既有专属处理不变。失败结果不绑定新的成功 response affinity。handler 已有 res!=nil 的部分计量路径负责入账。

这不会提前重放已输出请求，也不把部分失败变为成功。仍会等待既有 idle 周期，因此它是计量/终态修复，不是已完成356秒排空时长优化。JSON、Chat转换、WS不在此次变更范围，不能将本补丁的测试解释为全协议覆盖。

## 验证

原生 Forward completed-idle 与 partial-failed 两项从红转绿。真实 Responses handler、选择器、并发槽、同步 usage repository 链路验证：completed 后 idle 返回后恰好创建一条正确账号、17/3 token 的 usage；一次上游调用，槽位释放，无追加 stream_timeout。

真实 handler 唯一 usage 和既有 late recovery 回归已通过；无计量失败保持 nil、不生成零账单。独立 Go cache 下的 service 与 handler race 均通过；新增 cyber、媒体和无计量所有权负例同样通过。组合 PR36/PR37/PR38/PR39 的构建通过，组合定向 race 另行保留运行记录。生产未写入，历史缺失账单未补写，不能虚构或重发原请求。
