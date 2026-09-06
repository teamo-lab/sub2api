# Sub2API 仓库操作约定

## 云端访问入口

- 101 控制台地址：`http://101.32.58.225/`
- 101 OpenAI-compatible API Base：`http://101.32.58.225/v1`
- 101 Chat Completions 接口：`http://101.32.58.225/v1/chat/completions`
- API 请求使用控制台生成的 API Key 进行 Bearer 认证；不得将 API Key 写入本文件或提交到仓库。

## 盯盘目标统一（2026-09-06）

- GPT 守护目标统一为香港 101 / `ins-1c788zls`，唯一目标配置为
  `tools/gpt_pro_guard/production_target.json`。49 机器的账号 ID、凭据和实验基线不得直接复用。
- 101 `sub2api-account-monitor.timer` 每 3 分钟只读巡检账号错误；现有 429 timer 保留原策略。
  本机 `local.gpt-pro-account-error-guard` 仅同步云端状态并通知，实际巡检不依赖本机在线。
- 当前本机状态在 `tools/gpt_pro_guard/.runtime/hk101/`；旧根目录指标是历史数据，不得冒充当前快照。
  `pro_model_split/state.json` 等原始实验映射账本仍按其原路径保留。
- 旧账号 56、OAuth 321–350 恢复和旧 sticky burst 实验入口已禁用，等待重新核对 101 账号与基线。
  已停用 P90、已结束冷却试验及暂停的综合守护不能因本次迁移自动重启。
- 本次仅统一守护目标和只读巡检位置，不改变 43→101 交付链路及任何账号配置。

## GPT Pro 协议不变量

- 101 个人 Pro 自 2026-09-06 起按 sol/astra 分组实验；每账号只允许所属
  `gpt-5.6-sol` 或 `gpt-6-astra`，两组均允许 `codex-auto-review`。
  分组与原始映射记录在 `tools/gpt_pro_guard/.runtime/pro_model_split/state.json`，
  云端副本为 `/opt/sub2api/deploy/pro-model-split-20260906.json`。
  编辑、续期或批量更新不得擅自恢复混跑；实验期间保持既有优先级、代理和并发。

- 101 机器上的个人 Pro 账号（`plan_type=pro`）禁止调度 `gpt-5.6-terra`。
  新增、续期、批量更新或编辑模型白名单时，必须保持 terra 不在
  `credentials.model_mapping` 的键或映射目标中；不得通过空映射、通配符或透传绕过。
  该限制仅针对个人 Pro，不自动扩展到 Team 或 Flux Enterprise。

- `GPT-Pro-余额` 分组（生产 `group_id=80`）同时允许 `/v1/responses` 和
  `/v1/chat/completions`，必须保持 `disable_chat_completions=false`。
- 该分组内 OpenAI API-key 账号统一使用上游自动适配：
  `extra.openai_responses_mode=auto`（字段缺失也视为 auto）。不得强制
  `force_responses`，也不得强制 `force_chat_completions`。
- 调整 Base URL、并发、优先级、重试或账号凭据后，必须重新核对分组入口与账号自动模式，
  并继续统计真实 `inbound_endpoint` / `upstream_endpoint` 组合；转换只告警，不自动改协议。
- GPT 生产盯盘必须把 WebSocket（`request_type=3`）与 SSE 分开统计内部/外显 SLA、
  样本和 400/429/5xx；任一协议的异常不得被另一协议的大样本或全局 SLA 掩盖。
- 内部 SLA 的健康目标仍为 `98%`，但不得因此直接挡流；冷会话 Gate 的内部 SLA
  触发线固定为 `<95%`。内部 SLA 在 `[95%,98%)` 时只告警和定位上游/路由问题。
- 内部 SLA 必须按 client 请求最终结果计算：以 `client_request_id/request_id` 去重，
  retry 或 fallback 最终成功的请求计为成功，不得把已被接住的中间上游错误重复计为失败。

## GPT 三个独立守护 loop（2026-09-06 最新用户授权）

- 目标为 101 / group 80；账号切换、排队、P90 三个 loop 独立。当前仅定义账号切换；后两个保持 UNDEFINED，不部署自动动作。
- 账号切换源为运维面板同口径完整 300 秒桶：failover 类事件数 / (usage 成功记录 + 最终 error 记录数)，严格 >0.01 才进入错误降档判断。未完整桶只观察。
- 同账号 5 分钟内目标上游 429/502/503 合计至少 5 个独立请求、10 个错误事件，且导致切换：并发 >3 优先减并发；否则先减 sticky burst 到1，再把并发降到2。
- 每账号每窗口最多一个减1动作；允许同窗多个 OAuth 账号各自处理。并发和 sticky 均到下限后仍满足上述条件，priority 数值 +1。不自动恢复 priority。
- 连续两个完整5分钟窗口无目标错误，且各有效模型/协议的 upstream TTFT P90 <=10秒，并各有有效样本，则 concurrency 和 sticky 各+1，各最高5。零样本、缺失窗口、当前未完整桶已出现目标错误都不能恢复。
- 这次授权覆盖旧固定并发3、只允许单账号动作及旧429降档/恢复规则；新 loop 替代 `sub2api-account-429-guard.timer`，旧 timer 和旧配置 enabled 必须关闭，账本保留。
- 账号69/API-key/error/不可调度账号排除；不改账号状态、模型映射、proxy、协议模式。保留队列非零不减容、主机资源与发布锁保护。
- 新服务 `sub2api-account-switch-loop.timer`，规则及代码在 `tools/gpt_pro_guard/switch_loop*`，云端目录 `/opt/sub2api/guard-loops/account-switch`。每个动作保存写前意图、policy_hash、前后值、读回结果；写结果不确定或外部配置漂移则停止自动写入。
- 本地前端 `http://127.0.0.1:8788/` 展示三个区域，`/legacy` 保留旧看板。状态按 `target_id=hk101` 和时间校验，不以缺失数据冒充健康。

## GPT 连续 429 降并发守护（2026-09-06 用户授权）

> 历史规则：已由上述账号切换 loop 替代，禁止与新 loop 同时启用。

- 连续上游 429 是“保持原并发”规则的明确例外：允许守卫临时降低账号 `concurrency`，
  保持 `active + schedulable`，不改模型映射、代理、优先级和 sticky burst 配置。
- 101 的 `sub2api-account-429-guard.timer` 每 30 秒检查 GPT 分组 80，和 P90 守卫共用
  `/opt/sub2api/p90-account-guard/lock`。规则源文件：`tools/gpt_pro_guard/account_429_guard.py`。
- SSE、WS、非流式分别判定：60 秒内至少 3 个独立请求、6 次真实上游 429，或 5 分钟内
  至少 5 个独立请求、10 次 429，且最近错误不超过 90 秒，触发一次降 1 并发（最低 1）。
  同账号两次降档至少间隔 5 分钟，旧错误不得重复触发；单请求多次重试不触发账号降档。
- 降档要求队列为 0、同模型健康 OAuth P1 同伴至少 3 个空闲持久槽位、利用率低于 80%，
  且主机资源有余量。账号 69 始终排除；不得通过此规则隔离、停用或清理活动请求。
- 持久保存原并发和写前意图。无新 429 至少 30 分钟、有至少 20 个成功样本后，每次恢复 1，
  不超过守卫记录的原值；配置被人工或其他任务改动、写结果不确定时停止自动动作，保留现场。
- 恢复成功的重试仍计入 429 压力，但最终 SLA 按 client 请求结果去重，不能计为失败。

## GPT P90 账号与代理处置硬约束

- 普通慢尾、疑似风控、账号级 P90 异常或非 Payment Required 错误，只允许设置
  **30 分钟临时不可调度**。账号必须保持 `status=active` 和原并发配置，写入明确的
  `temp_unschedulable_until` 与原因；到期自动恢复并复测。
- 账号处置顺序固定为“最近 session → proxy/IP → 账号冷却”。最近三条 session 中若只有
  一条高尾、其余正常，则判定为单 session 问题，**不执行任何动作**：不解绑 sticky、
  不换 IP、不冷却账号。只有多个 session 均高尾时才考虑同国第二 IP 的单变量迁移；
  换 IP 后必须重新检查最近至少三条有有效 TTFT 的独立 session；只有这三条及以上全部
  明显高尾且健康对照账号正常，才允许上述 30 分钟临时不可调度。任一条正常或证据不足
  都保持只读观察。
- 严禁把普通异常账号永久关闭、暂停，或写成 `inactive`/`disabled`/`paused`；也严禁用
  持久 `schedulable=false` 冒充临时冷却。唯一例外是已经确认的 OpenAI OAuth
  `Payment Required`，按用户既定规则永久 `inactive + schedulable=false`。
- Flux Enterprise 账号 69 永远不得隔离、停用或设置 `schedulable=false`；守卫不得
  自动修改其优先级。P1/P2 只允许由明确的运营动作设置，且不得伴随状态、可调度性
  或并发配置削减。
- 同一账号累计最多使用两个不同 IP/proxy；更换 proxy 时不得跨国家，但允许同国内跨地域。
  所有新绑定必须严格一 IP 一账号：只允许选择实时账号数为 `0` 的 IP，创建后必须恰好为 `1`。
  历史上已经绑定多个账号的 IP 不回退、不迁移，但一律冻结新增。
- 没有满足上述约束的空闲 IP 时，采购新的 IPRoyal Datacenter Dedicated（DC）IP。
  实锤 P90 有问题的 IP 必须停止新分配、排空并从 active 池删除/归档，同时加入持久 denylist。

## 云端版本检查与升级

- 涉及 Sub2API 云端版本、容器健康、备份或升级时，使用本机 `sub2api-cloud-upgrade` Skill：`/Users/zhangyiming/.codex/skills/sub2api-cloud-upgrade/SKILL.md`。
- 只检查版本与运行状态：

  ```bash
  ~/.codex/skills/sub2api-cloud-upgrade/scripts/sub2api-cloud-upgrade --check
  ```

- 备份并执行升级：

  ```bash
  ~/.codex/skills/sub2api-cloud-upgrade/scripts/sub2api-cloud-upgrade --apply
  ```

- 执行 `--apply` 前必须先确认当前三容器健康；升级流程必须先创建 PostgreSQL 备份，只重建 `sub2api` 服务，完成后复查 `/health` 以及 accounts、api_keys 等持久化记录数量。
- 云端受管部署目录为 `/opt/sub2api/deploy`，备份目录为 `/opt/sub2api/backups`。不得删除 Docker volumes、部署目录、备份或镜像，不得自动恢复数据库；异常时保留现场并报告。
- 不得在输出、日志或文档中暴露 `.env`、数据库凭证、API Key、OAuth 凭证或账号内容。
- 官方版本以 GitHub `Wei-Shaw/sub2api` 的 latest release 为准，部署镜像为 `weishaw/sub2api:latest`。

## 每日版本检查 loop

- LaunchAgent 配置：`/Users/zhangyiming/Library/LaunchAgents/local.sub2api-release-check.plist`。
- 每天 10:00 检查 GitHub 官方 release；发现新版本时发送 macOS 通知。
- loop 只检查和通知，禁止自动升级；实际升级必须显式执行 `--apply`。
- 文档更新时的基线（2026-08-08）：云端 `0.1.172`，官方 `v0.1.172`，无需升级。后续判断必须以最新一次 `--check` 输出为准。

## 蓝绿发布与一键回滚

- 涉及生产 blue/green 状态、canary 权重、回滚准备或紧急回滚时，使用本机
  `sub2api-rollout` Skill：`/Users/zhangyiming/.codex/skills/sub2api-rollout/SKILL.md`。
- 只读检查：

  ```bash
  ~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --status
  ```

- 只读回滚预演：

  ```bash
  ~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --dry-run
  ```

- 只有用户明确要求回滚时，才执行以下任一等价命令：

  ```bash
  ~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --rollback
  ~/.codex/skills/sub2api-rollout/scripts/sub2api-rollback-now
  ```

- 回滚必须以控制器固化的 `rollback_target_slot` 为准；禁止手工交换 slot、直接改 HAProxy、
  重启/停止容器、终止活动 session 或自动恢复数据库。回滚只切走新请求，已有 SSE/WebSocket
  继续排空；Skill 的 post-check 未通过时保留 action ID 和现场，不得自动发起第二次写操作。

### 101 / 43 双机发布硬门禁

- 双机发布必须先取得 `sub2api-fleet-release-lease`，从首个写操作到两台最终验证只能由一个
  Codex task owner 持有。其他任务发现 lease 或活跃发布任务后只能只读，禁止拆分执行
  stage、canary、promote、worker、complete 或入口切换。
- 发布严格串行：先完成 101，并观察一个完整 5 分钟窗口确认 43→101 新增 401 为 0；之后才能
  开始 43。禁止两台交叉推进或同时切流。
- 101 候选 0%/10%/100% 和 43 候选 0%/10%/100%/公网入口接管后，必须从 43 实际服务容器的
  网络命名空间，使用原生产 API Key 验证 101 私网/公网 `/v1/models`、`gpt-reserve`、实际生产
  模型和 Responses SSE `response.completed`。只记录 Key ID 与指纹哈希，不输出或复制 Key。
- mock 上游、单机 curl、health、功能单测和后台“测试账号连接”均不能替代上述真实链路门禁；
  后台测试可能改变账号状态，只用于诊断。
- 任意一次新 401 / `INVALID_API_KEY`、43 对应账号状态漂移、双 singleton owner、controller 与
  公网 slot 不一致或另一任务写入，都立即停止整轮发布。先 `entry-switch rollback` 恢复 43 legacy，
  再按 101 controller 的固化 rollback target 回滚；禁止把 `restore-proxy` 当作恢复 legacy。
- 鉴权事故中禁止换 Key、刷新凭据、改 Base URL、删除/重建账号或重启数据库/Redis。必须使用原 Key
  证明恢复，并确认 43 公网请求 200 + completed、连续两分钟新增 401 为 0、账号配置未漂移、
  唯一 worker 归属明确。
- 43 的 `entry-switch` PREROUTING 规则必须限制 `-m addrtype --dst-type LOCAL`；禁止只按目的端口80
  做全局 REDIRECT，否则容器访问101的80端口会被重定向回43本机并产生 `INVALID_API_KEY`。

### Router outcome 双机发布固定流程（2026-09-06）

这条规则专门约束 Teamo Router 直连 Sub2API 的 outcome 协议发布，防止后续任务重复走错账号、错机器或错顺序。

- **机器身份固定**：101 是香港 CVM `ins-1c788zls`，公网 `101.32.58.225`，私网 `172.19.16.3`；43 是香港 CVM `ins-4r9sd5og`（实例名 `sub2api-hk`），公网 `43.159.0.162`，私网 `172.19.0.100`。`49.51.251.58` 是另一台 `yumi-dev`，不属于 43，严禁再作为 43 发布目标；`43.159.230.106` 是 Dashboard 实例公网地址，也不是 43。若需要通过业务入口验收，仍须从真正的 43（`ins-4r9sd5og`）实际服务容器的网络命名空间发起。
- **当前候选版本**：Sub2API outcome 协议候选来自 `teamo-lab/sub2api` 的已提交 commit 和 release tag；Router Gateway 候选来自 Router 的已提交 commit。发布必须使用 commit archive 或 `sub2api-release` 审计脚本，禁止在主机上直接 `docker compose up`、手改 HAProxy 或用临时 SSH 命令替代 controller。
- **发布 owner/lease**：先取得唯一的 `sub2api-fleet-release-lease`，owner、release ID 从 101 stage 开始一直保持到 43 最终验证；不得由多个任务分别操作两台机器。
- **固定顺序**：先 101 `stage → 0% 健康 → 10% canary → 真实链路门禁 → 100% → worker/complete`，观察完整 5 分钟且真正的 43（`ins-4r9sd5og`）→101 新增 401 为 0；之后才能对 43 执行同样流程。101 未完成时，43 候选不得保持 10% 或更高流量。
- **基线账号选择**：不能固定依赖历史 account22。每轮发布前必须从真正的 43 运行面或其受信任管理源实时确认候选 `status=active AND schedulable=true`、实际 Base URL、模型映射和 Key ID；不能假设 43 一定有本地 PostgreSQL，也不能从 49 或其他机器数据库借用账号/Key。只记录 Key ID 与指纹哈希，不输出 Key。若候选是已撤销 OAuth、`error`、`schedulable=false` 或 Base URL 不符合基线，立即换一个合规候选，禁止修改/刷新凭据来“修”门禁。
- **真实 Key 门禁**：候选必须从 43 active 容器网络命名空间验证 101 公网和私网 Base URL 的 `/v1/models`、`gpt-reserve`、实际生产 GPT 模型 Responses SSE，均须 HTTP 200 且有 `response.completed`。只通过公网不算通过；私网不可达时停止发布并修复网络/路由后再试。
- **替代账号的等价性**：换账号只能换“真实可用的同一交付链路基线”，不能用任意 active API Key 代替。至少核对 43/101 Key ID 或指纹、group/model mapping、Base URL、协议和账号状态；后续请求必须证明仍然走同一 43→101 链路。
- **错误处理**：任何一次新的 401、`INVALID_API_KEY`、账号状态漂移、私网/公网链路不一致，立即停止整轮；先用 controller 回滚新请求到稳定槽，保留 action ID 和现场，再排查。不得继续 canary、换 Key、删账号、重建数据库或跳过门禁。
- **回滚与现场**：回滚只使用 controller 固化的 `rollback_target_slot`，不手工交换 slot；已有 SSE/WebSocket 继续排空。候选镜像、数据库备份、release ID、Key 指纹和门禁输出必须保留，不能为了“重新开始”删除现场。
- **完成条件**：只有 101 和 43 都通过真实 Key 的公网/私网 `/v1/models`、`gpt-reserve`、生产 GPT Responses SSE，且两台 active slot、版本、digest、worker owner、入口一致，才能宣称双机上线完成。单机健康、mock、单元测试或后台测试账号连接都不能替代这条证据。

## 原生 Chrome 账号入池与续期

### 已验证工具链，禁止偏离

- ChatGPT、Sub2API 后台、OAuth 和邮箱等网页内操作，个人账号与 Business/Team 成员统一使用
  用户已经打开的原生 Chrome，并通过 `chrome:control-chrome` 接管。
- 明确要求原生 Chrome 时，禁止用 `cua.getState()` 做全量 surface 枚举；当前桌面端版本该路径
  可能等待无关 surface 并在 30 秒后超时，从而把可用的 Chrome 误报为不可用。必须直接通过
  `chrome:control-chrome` 的 `agent.browsers.get("chrome")` 选择 Chrome；若调用统一 CUA，直接
  使用 `cua.getBrowser({id:"chrome"})`，不得先调用 `cua.getState()`。
- Chrome 连通性以 `agent.browsers.get("chrome")` 成功并且 `chrome.user.openTabs()` 能返回当前
  用户标签页为准；`chrome.tabs.list()` 为空只表示没有 Agent 创建/接管的标签页，不代表断线。
- 不启动或依赖其他 Chrome 发行版，不读取、创建或要求用户创建独立 Chromium 个人资料；旧的
  浏览器个人资料映射不再属于本项目工作流。
- 不要求无痕模式。整批账号复用一个 Sub2API 后台标签页、一个 OAuth 标签页，以及按邮箱服务
  复用的收件箱标签页；缺少时只在当前原生 Chrome 中补建必要标签页。
- 禁止使用 `osascript`、AppleScript、System Events、截图识别、图片坐标点击、键鼠坐标脚本或
  其他临时 UI 自动化替代浏览器 Skill。语义 DOM 点击优先；只有网页自定义组件拦截语义点击时，
  才可依据同一 DOM 元素的实时 bounding box 使用浏览器内 CUA 点击。
- 浏览器控制暂时不可用时先重连一次；仍不可用则保留当前页面和状态，报告阻塞并让用户完成唯一
  必需的人工步骤，不自行更换浏览器或自动化路线。
- 每个账号都在 OAuth 账号选择页明确选择“登录至另一个帐户”，按完整邮箱重新登录并核对身份。
  Business/Team 还必须核对目标 Business workspace；Personal 必须明确选择 Personal。不能凭
  浏览器残留登录态、显示名、头像或工作区简称猜测。
- 用户回复“好了”后，重新检查当前 URL、完整邮箱和工作区，再继续。
- Sub2API OpenAI OAuth 必须保持一轮一对一：在后台原新增/重新授权弹窗中生成链接后，该弹窗不得
  关闭、刷新或重新生成；必须用这条链接在复用的 OAuth 标签页完成授权，再把它产生的 callback
  填回原弹窗。禁止先填 callback 再生成链接，禁止旧 callback 与新 state 混用。
- 用户提供三列 `mail.com` 账号时，第一列为邮箱、第二列为 `mail.com` 邮箱密码、第三列为 OpenAI
  密码。验证码直接登录 `mail.com` 收件箱读取，不使用 `smsreceive.cloud`；密码、验证码、OAuth URL
  和 callback 均不得写入仓库、TSV、Skill 或日志。
- 每新增一个成功入池的云端账号，账号自身持久 `concurrency` 固定为 `3`，号池总配置并发增加 `3`；
  GPT Pro 分组对命中同账号最近成功记录的 sticky 会话可额外临时借用 `3` 个 slot，即 `3 durable + 3 sticky`。
  sticky `+3` 不是持久配置容量，不写回账号 `concurrency`，也不得重复计入号池总配置并发或用户侧上限。
  同时必须确认客户实际交付 API Key 的所属用户，并将该用户的并发上限增加持久容量 `3`。当前交付 Key
  属于 `admin@sub2api.local`。账号侧容量和用户侧上限是两个独立配置，两者都复核后才能完成入池；
  重新授权旧账号不得重复增加任一侧并发。
- 新建后变为 `error` 或不可调度的账号可以保留持久 `concurrency=3` 以便后续修复，但其有效可调度
  容量和交付用户并发贡献均为 `0`；不得仅因状态异常把账号字段改成 `0`，也不得把原始账号配置总和
  误报成有效号池容量。只有稳定 `active + schedulable` 的新增账号才给交付用户增加 `3`。
