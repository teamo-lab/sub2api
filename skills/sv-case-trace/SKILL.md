---
name: sv-case-trace
description: Trace one request on the Silicon Valley Sub2API (sub2api-sv-1, KA's NewAPI gpt group) end to end — which accounts it tried, each hop's status and upstream request ID, the final account's upstream ID, first-token time and duration, and the raw request/response stored in COS. Use when someone hands over a case ID (Sub2API client_request_id / X-Oneapi-Request-Id, or a NewAPI request_id) for sv-1 and asks what happened, which upstream served it, or for the original input/output. Read-only.
---

# SV Case Trace

`scripts/sv_case_trace.py` 只读地把一条请求在 sv-1 上的全链路拼出来：

| 数据 | 来源 |
| --- | --- |
| 最终服务的账号、它的上游请求 ID、首字、耗时、tokens | `usage_logs.request_id = 'client:<id>'` |
| 每次换号失败的账号、状态码、时间、上游 ID（拿不到业务 ID 时是 cf-ray） | `ops_error_logs.client_request_id = <id>` 的 `upstream_errors` |
| 请求和响应原文（gzip JSON，30 天过期） | COS `sub2api-sv-raw-1255660719` 的 `raw/<id>/<开始纳秒>.json.gz` |

## 用法

```bash
cd skills/sv-case-trace/scripts
uv run sv_case_trace.py 0f646b8e-05cc-4c4e-adee-b14d9d190f21
uv run sv_case_trace.py <id> --download ./cases      # 原文解压到 ./cases/<id>/
uv run sv_case_trace.py <id> --json                  # 给程序用
uv run sv_case_trace.py <newapi request_id> --newapi # 输入是 NewAPI 的 request_id
```

`uv run` 会按脚本头装好 `cos-python-sdk-v5` 和 `PySocks`。前提：

- `ssh -i ~/.ssh/id_rsa ubuntu@43.173.108.67` 能登录（可用 `--host`、`--ssh-key` 或 `SV1_SSH_HOST`、`SV1_SSH_KEY` 改）；Postgres 走 `sudo docker exec sub2api-postgres psql`。
- COS 凭据在 `~/.tccli/default.credential`（`--credential` 可改）。本机直连硅谷 COS 的 TLS 会被断，脚本会临时起 `ssh -D` 隧道走 socks5h，用完关掉；已经开着 `ssh -f -N -D 18080 …` 的话传 `--socks-port 18080` 复用。
- 用 NewAPI 的 request_id 查时，需要 `NEWAPI_ACCESS_TOKEN` 和 `NEWAPI_USER_ID`（NewAPI 个人设置里生成的系统访问令牌和管理员用户 ID；`NEWAPI_BASE_URL` 默认 `https://newapi.floatai.cn`）。脚本按 `logs.request_id` 查到 `upstream_request_id`，它就是 Sub2API 的 client_request_id。输入不是 UUID、Sub2API 里也查不到时会自动尝试这一步。

## 怎么读结果

- 「各跳」按时间排：`0.` 是请求进入 Sub2API 的时间（取自 COS 对象名），每跳带相对耗时。失败跳来自 `upstream_errors`，最后一跳 `200 成功` 来自 `usage_logs`。
- 上游 ID 读的是账号 `extra.upstream_request_id_header` 指定的头（Zero 是 `X-Oneapi-Request-Id`，new-api v1.6.x 是 `X-Request-Id`，Sub2API 站是 `X-Client-Request-Id`）。失败跳在该头缺失时退回 `x-request-id`，同时记 `cf-ray`。Cloudflare 边缘直接返回的错误（403 `error code: 1010`、524）只有 cf-ray。新版本上线前的失败跳一律只读 `x-request-id`，所以老 case 里常常是空的。
- 客户端状态 200、ops 里又有记录，说明中途换过号、最后成功了（`Recovered upstream error`）。只有 ops、没有 usage，说明请求最终失败。
- 原文是网关写给客户端的最终请求和响应，不含中间失败跳的上游响应。失败跳的上游报错摘要看「各跳」里 `|` 后面的内容。
- 两张表都没查到：ID 不对、不是 sv-1 的请求，或者还没写库（异步写入，通常延迟几秒）。COS 没有对象：超过 30 天、请求进来时还没开落盘，或者 ID 不对。
