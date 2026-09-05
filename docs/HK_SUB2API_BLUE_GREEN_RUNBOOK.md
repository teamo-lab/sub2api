# 香港 Sub2API 单机蓝绿发布 Runbook

适用实例：`ins-1c788zls`（公网 `101.32.58.225`，内网 `172.19.16.3`）。稳定入口为宿主机端口 `80`，首次接管期间临时 HAProxy 使用 `18080`。

## 不变量

- PostgreSQL、Redis 只运行一份；API 使用 blue/green 两槽，后台任务只由 singleton worker 执行。
- 切权重只影响新连接，旧 SSE/WebSocket 必须自然排空；禁止强杀 session。
- 发布与回滚只通过 rollout controller，禁止手工改 HAProxy、交换 slot 或重启活动槽。
- burst 是账号 `extra.openai_sticky_burst` 中的临时额外槽位，默认 `1`、范围 `0..10`；不改变基础 `concurrency` 与持久容量。
- `GPT-Pro-余额` 保持 Chat Completions 与 Responses 双入口，OpenAI API-key 账号保持 `openai_responses_mode=auto`。

## 本机环境

```bash
export SUB2API_CLOUD_HOST=101.32.58.225
export SUB2API_CLOUD_SSH_KEY="$HOME/.ssh/yumi_dev_sub2api"
export SUB2API_CLOUD_REMOTE_DIR=/opt/sub2api/deploy
```

## 日常发布

发布代码必须已提交并有 tag 指向 HEAD。

涉及101和43时，先取得一个覆盖整轮双机发布的 fleet release lease。两台严格串行：101完成并观察一个完整5分钟窗口后，才能开始43。任何其他任务发现lease已占用时只能只读。

```bash
~/.codex/skills/sub2api-rollout/scripts/sub2api-release stage \
  /absolute/repo/path <release-id> <version>
~/.codex/skills/sub2api-rollout/scripts/sub2api-release canary

# 完成本次功能回测后快速晋级
~/.codex/skills/sub2api-rollout/scripts/sub2api-release promote-fast <auditable-reason>
~/.codex/skills/sub2api-rollout/scripts/sub2api-release worker
~/.codex/skills/sub2api-rollout/scripts/sub2api-release complete 0
```

## 状态与回滚

```bash
~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --status
~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --dry-run
~/.codex/skills/sub2api-rollout/scripts/sub2api-rollout --rollback
```

调用以上命令时必须保留本机环境变量。回滚只切走新请求；被回滚槽的活动流继续排空。

## 首次接管

1. 将 `docker-compose.bluegreen.yml`、`deploy/rollout/*` 与配对探针安装到受管目录。
2. 执行 `prepare-bluegreen --check`，确认 legacy、PostgreSQL、Redis、磁盘和内存健康。
3. 执行 `bootstrap-bluegreen <release-id> <version>`，建立 blue=100、green=0 的相同镜像基线。
4. 先用 `entry-switch test <operator-ip>` 验证临时入口，再执行 `entry-switch activate`；旧连接仍留在 legacy。
5. 完成一次真实版本 canary、promotion、worker 与 complete。
6. legacy 活动连接连续三次为0后执行 `finalize-entry`，让 final HAProxy 直接监听端口80。

任何步骤失败均保留 legacy、镜像、数据库备份和 rollout 状态，不自动恢复数据库。

## 第二台香港实例：43 的外部依赖接管

`43.159.0.162`（`ins-4r9sd5og`）保留其自身数据库、Redis、账号配置和数据目录。
受管发布目录统一为 `/opt/sub2api/deploy`，原单容器数据仍为 `/opt/sub2api/data`。
通过 `rollout/site.env` 固化以下非敏感差异，禁止拷贝101的runtime.env或账号数据：

```dotenv
SUB2API_DEPENDENCY_MODE=external
SUB2API_DOCKER_NETWORK=sub2api_sub2api-network
SUB2API_LEGACY_DATA_DIR=/opt/sub2api/data
SUB2API_PUBLIC_PORT=80
SUB2API_INTERNAL_PORT=8080
ROLLOUT_FINAL_ENTRY_PORT=80
```

`dependency-client` 从本机健康应用容器获取现有依赖配置：通过容器内已有 `psql/pg_dump`
检查和备份外部 PostgreSQL，直接认证 PING 现有 Redis。凭据不写入命令参数或日志；不运行数据库恢复。
所有备份为权限600的custom dump，并校验PGDMP文件头及SHA256。

显式获准首次接管后，使用本机Skill的 `sub2api-bootstrap` 入口执行prepare、镜像import、bootstrap、
entry-test、activate、finalize。bootstrap镜像必须支持api/worker进程角色，不能使用不支持角色隔离的旧镜像。
切换阶段由原单容器继续执行singleton任务，蓝绿槽仅运行API；原容器连接排空后再停止原容器并启动唯一worker。
`finalize-entry` 按容器内部8080检查活动连接，不能使用公网80替代。

日常stage/canary/promote-fast/worker/complete和一键回滚仍与101共用同一套控制器。
双机同步版本应传输同一个不可变镜像，并核对两台API、worker的image ID和编译commit；不要各自构建后仅比较版本名。

## 跨机鉴权门禁

43依赖101的生产API Key。发布前记录43生产账号、101 API-key ID、Key指纹、Base URL、group、model mapping和账号状态，禁止记录Key本体。

在101候选0%、10%、100%，以及43候选0%、10%、100%和公网入口接管后，都必须从43实际服务容器的网络命名空间使用原生产Key验证：

- 101私网与公网 `/v1/models` 返回200；
- `/v1/responses` 的 `gpt-reserve` 和实际生产模型返回200；
- SSE必须出现 `response.completed`；
- 43对应账号保持 `active + schedulable`，并发、priority、Base URL不漂移；
- 对应阶段开始后的新增上游401为0。

mock上游、管理后台测试连接、单机curl、health和功能单测均不能替代这组门禁。后台测试连接可能改变账号状态，只用于诊断。

任意一次401或 `INVALID_API_KEY` 立即停止双机发布：先执行43的 `entry-switch rollback` 恢复legacy新连接，再按101 controller的 `rollback_target_slot` 回滚。已有长连接自然排空。禁止用 `restore-proxy` 冒充恢复legacy，禁止在事故中换Key、刷新凭据或重建账号。

43 的入口 NAT 规则必须包含 `-m addrtype --dst-type LOCAL`。缺少该条件时，容器访问其他主机的
目的端口80流量也会命中 PREROUTING `REDIRECT`，例如 `172.19.16.3:80` 会被错误送回43本机，
导致101的上游Key在43鉴权并返回 `INVALID_API_KEY`。安装或升级入口脚本后，必须同时验证公网请求
进入蓝绿槽、容器访问101仍到达101的deployment slot。

## 发布并发与singleton

- 一轮发布只能有一个task owner；从首个stage到两台最终验证期间不得释放lease。
- 首次接管时，legacy存活期间唯一singleton owner是legacy，worker必须停止。
- 只有入口已接管、legacy连接排空、真实鉴权门禁通过后，才停止legacy并启动唯一worker。
- 每次worker动作后必须列出所有all/worker角色，确认只有一个singleton owner。
- 任何阶段发现另一个任务正在写、controller状态与公网slot不一致或双worker，立即停止并恢复上一已验证状态。
