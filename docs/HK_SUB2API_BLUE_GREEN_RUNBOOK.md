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
