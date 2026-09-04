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
