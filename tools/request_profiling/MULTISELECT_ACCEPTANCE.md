# 耗时分析多选筛选验收

2026-09-09，基于 teamo/main `0f9694fca`。

模型、分组、账号/渠道三个输入框改成可搜索的多选下拉框。分组与账号显示名称，不要求输入或记忆 ID。每个下拉框可清空该维度全部选择；空选择表示不限，同维度 OR、跨维度 AND。最多 100 项，Esc 或点外侧关闭。

选项取自整个时间/协议/证据窗口，包含所有分页及重试过程账号，不随当前维度选择缩小。首次加载及时间/协议/证据改变时请求选项；普通勾选、分页不重复查询选项。现有查询的 24 小时上限、10 秒上下文超时保持不变。保留原单值接口参数兼容性；新增数组参数使用参数化 SQL。名称从现有 groups/accounts 表查询，无数据库迁移、无生产配置变更。

验证：

- `npm run build` 通过（已有 chunk size 警告）。
- 定向 ESLint 通过；RequestProfilingView 4 个测试通过，覆盖多模型、跨维度组合、单独清空和刷新错误恢复。
- `go test ./internal/repository ./internal/handler/admin -run RequestProfile -count=1` 通过；设置本地 REQUEST_PROFILE_TEST_DSN 后 PostgreSQL 集成测试通过，使用临时表验证多选 OR/AND、统计结果、重试前渠道和跨分页选项。
- 本地实际服务接口：多选查询 HTTP 200；负数 ID、非法 ID、超过 100 项 HTTP 400。
- Chrome 真实本地页面 `http://127.0.0.1:5197/admin/request-profiling`：从真实本地测试库显示 Local Profiling QA 分组，Controlled 503 / Healthy local stream 渠道；连续勾选两个渠道仍命中同一重试请求；搜索不存在名称出现空态；清空后还原；按分组名称选择并用 Escape 收起。截图已逐张检查，名称、勾选状态、清空入口和整体布局正常。

本次没有部署生产。前后端需要一起发布；仅替换前端而使用旧接口会缺失候选项及多选过滤能力。
