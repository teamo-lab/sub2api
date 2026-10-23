#!/usr/bin/env python3
# /// script
# requires-python = ">=3.9"
# dependencies = ["cos-python-sdk-v5>=1.9", "PySocks>=1.7"]
# ///
"""按 case 查 sv-1 Sub2API 的全链路：走了哪些号、每一跳的上游 ID、首字和耗时、COS 原文。

输入是 Sub2API 的 client_request_id（等于响应头 X-Oneapi-Request-Id / X-Client-Request-Id），
也接受 NewAPI 的 request_id（经 NewAPI logs.upstream_request_id 换算，需要 NEWAPI_ACCESS_TOKEN）。

数据来源：
  - usage_logs.request_id = 'client:<id>'：最终服务的账号、upstream_request_id、首字、耗时
  - ops_error_logs.client_request_id = <id>：upstream_errors 里每次换号失败的账号、状态码、时间、上游 ID
  - COS raw/<id>/：请求和响应原文（gzip JSON），可选下载解压

只读：Postgres 走 ssh + docker exec psql，COS 走 ssh -D 隧道（本机直连硅谷 COS 的 TLS 会被断）。
用法见 ../SKILL.md。
"""

from __future__ import annotations

import argparse
import datetime as dt
import gzip
import json
import os
import re
import shlex
import socket
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from typing import Any

DEFAULT_HOST = "ubuntu@43.173.108.67"
DEFAULT_SSH_KEY = "~/.ssh/id_rsa"
DEFAULT_PG_CONTAINER = "sub2api-postgres"
DEFAULT_BUCKET = "sub2api-sv-raw-1255660719"
DEFAULT_REGION = "na-siliconvalley"
DEFAULT_PREFIX = "raw"
DEFAULT_CREDENTIAL = "~/.tccli/default.credential"
DEFAULT_NEWAPI_BASE_URL = "https://newapi.floatai.cn"
DISPLAY_TZ = dt.timezone(dt.timedelta(hours=8), "UTC+8")

# client_request_id 由网关生成（UUID）或客户端透传；只接受安全字符，避免拼进 SQL/对象名时出问题。
CASE_ID_RE = re.compile(r"^[A-Za-z0-9._:\-]{1,128}$")
UUID_RE = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

USAGE_SQL = """
select coalesce(json_agg(t order by t.created_at), '[]'::json) from (
  select u.id, u.created_at, u.account_id, a.name as account_name,
         a.extra->>'upstream_request_id_header' as upstream_request_id_header,
         u.upstream_request_id, u.first_token_ms, u.duration_ms,
         u.model, u.upstream_model, u.stream, u.inbound_endpoint, u.upstream_endpoint,
         u.input_tokens, u.output_tokens, u.cache_read_tokens, u.actual_cost,
         u.group_id, u.api_key_id, u.ip_address, u.deployment_slot
  from usage_logs u left join accounts a on a.id = u.account_id
  where u.request_id = 'client:' || :'case_id'
) t;
"""

OPS_SQL = """
select coalesce(json_agg(t order by t.created_at), '[]'::json) from (
  select o.id, o.created_at, o.request_id, o.account_id, a.name as account_name,
         o.status_code, o.upstream_status_code, o.error_phase, o.error_type, o.error_message,
         o.upstream_error_detail, o.model, o.request_path, o.stream, o.duration_ms,
         o.time_to_first_token_ms, o.routing_latency_ms, o.upstream_latency_ms,
         o.deployment_slot, o.upstream_errors::json as upstream_errors
  from ops_error_logs o left join accounts a on a.id = o.account_id
  where o.client_request_id = :'case_id'
) t;
"""


class TraceError(RuntimeError):
    pass


def fmt_time(value: Any) -> str:
    """把 at_unix_ms 或 Postgres 时间串格式化成 UTC+8。"""
    if value is None or value == "":
        return "-"
    if isinstance(value, (int, float)):
        return dt.datetime.fromtimestamp(value / 1000, DISPLAY_TZ).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]
    try:
        parsed = dt.datetime.fromisoformat(str(value))
    except ValueError:
        return str(value)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    return parsed.astimezone(DISPLAY_TZ).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]


def parse_unix_ms(value: Any) -> int | None:
    try:
        parsed = dt.datetime.fromisoformat(str(value))
    except (TypeError, ValueError):
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    return int(parsed.timestamp() * 1000)


def fmt_ms(value: Any) -> str:
    if value is None:
        return "-"
    return f"{int(value) / 1000:.2f}s"


class SV1:
    def __init__(self, host: str, ssh_key: str, pg_container: str):
        self.host = host
        self.ssh_key = os.path.expanduser(ssh_key)
        self.pg_container = pg_container

    def ssh_base(self) -> list[str]:
        return ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-i", self.ssh_key, self.host]

    def query_json(self, sql: str, case_id: str) -> Any:
        if not CASE_ID_RE.match(case_id):
            raise TraceError(f"case id 含非法字符：{case_id!r}")
        # CASE_ID 经 docker exec -e 传进容器，再由 psql 的 :'case_id' 按字面量转义。
        psql = 'psql -X -q -At -v ON_ERROR_STOP=1 -v case_id="$CASE_ID" -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
        inner = "sudo docker exec -i -e CASE_ID={} {} sh -c {}".format(
            shlex.quote(case_id), shlex.quote(self.pg_container), shlex.quote(psql)
        )
        proc = subprocess.run(
            self.ssh_base() + [inner],
            input="set timezone = 'UTC';\n" + sql,
            capture_output=True,
            text=True,
            timeout=60,
        )
        if proc.returncode != 0:
            raise TraceError(f"psql 失败（exit {proc.returncode}）：{proc.stderr.strip()}")
        out = proc.stdout.strip().splitlines()
        if not out:
            raise TraceError("psql 没有输出")
        return json.loads(out[-1])


def resolve_newapi_request_id(request_id: str, base_url: str, token: str, user_id: str) -> list[dict]:
    """用 NewAPI 管理接口按 request_id 查日志，返回带 upstream_request_id 的记录。"""
    query = urllib.parse.urlencode({"p": 1, "page_size": 20, "request_id": request_id})
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}/api/log/?{query}",
        headers={"Authorization": token, "New-Api-User": user_id, "Accept": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        payload = json.loads(resp.read().decode("utf-8"))
    if not payload.get("success"):
        raise TraceError(f"NewAPI 查询失败：{payload.get('message')}")
    data = payload.get("data") or {}
    items = data.get("items") if isinstance(data, dict) else data
    return [item for item in (items or []) if item.get("request_id") == request_id]


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def port_open(port: int) -> bool:
    with socket.socket() as s:
        s.settimeout(0.5)
        return s.connect_ex(("127.0.0.1", port)) == 0


class SocksTunnel:
    """复用已有的 ssh -D 端口；没有就自己起一个，退出时关掉。"""

    def __init__(self, sv1: SV1, port: int | None):
        self.sv1 = sv1
        self.port = port
        self.proc: subprocess.Popen | None = None

    def __enter__(self) -> int:
        if self.port and port_open(self.port):
            return self.port
        self.port = self.port or free_port()
        cmd = self.sv1.ssh_base()[:-1] + ["-N", "-o", "ExitOnForwardFailure=yes", "-D", f"127.0.0.1:{self.port}", self.sv1.host]
        self.proc = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        deadline = time.time() + 20
        while time.time() < deadline:
            if port_open(self.port):
                return self.port
            if self.proc.poll() is not None:
                raise TraceError(f"ssh -D 隧道启动失败：{self.proc.stderr.read().decode(errors='replace').strip()}")
            time.sleep(0.2)
        raise TraceError("ssh -D 隧道 20 秒内没有就绪")

    def __exit__(self, *exc) -> None:
        if self.proc and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()


def cos_client(socks_port: int, credential_path: str, region: str):
    try:
        from qcloud_cos import CosConfig, CosS3Client
    except ImportError as exc:  # pragma: no cover - 依赖缺失时给出提示
        raise TraceError("缺少 qcloud_cos，请用 `uv run sv_case_trace.py ...` 运行（会按脚本头自动装依赖）") from exc
    cred = json.load(open(os.path.expanduser(credential_path)))
    proxy = f"socks5h://127.0.0.1:{socks_port}"
    config = CosConfig(
        Region=region,
        SecretId=cred["secretId"],
        SecretKey=cred["secretKey"],
        Scheme="https",
        Proxies={"http": proxy, "https": proxy},
    )
    return CosS3Client(config)


def summarize_raw_record(record: dict) -> dict:
    """原文对象的摘要（不打印请求/响应正文）。"""
    summary = {
        "status": record.get("status"),
        "model": record.get("model"),
        "stream": record.get("stream"),
        "duration_ms": record.get("duration_ms"),
        "request_bytes": record.get("body_bytes"),
        "response_encoding": record.get("response_encoding"),
        "response_bytes": record.get("response_bytes"),
        "response_truncated": record.get("response_truncated", False),
    }
    text = record.get("response_text")
    if isinstance(text, str):
        summary["sse_events"] = text.count("event:")
        summary["has_response_completed"] = "response.completed" in text
    return {k: v for k, v in summary.items() if v is not None}


def fetch_cos(sv1: SV1, case_id: str, args: argparse.Namespace) -> list[dict]:
    prefix = f"{args.prefix.strip('/')}/{case_id}/"
    with SocksTunnel(sv1, args.socks_port) as port:
        client = cos_client(port, args.credential, args.region)
        resp = client.list_objects(Bucket=args.bucket, Prefix=prefix, MaxKeys=100)
        objects = []
        for obj in resp.get("Contents", []) or []:
            entry = {
                "key": obj["Key"],
                "size": int(obj.get("Size", 0)),
                "last_modified": obj.get("LastModified"),
                "url": f"cos://{args.bucket}/{obj['Key']}",
            }
            # 对象名末段是请求开始时间（纳秒），见 rawcapture.ObjectKey。
            stem = os.path.basename(obj["Key"]).split(".", 1)[0]
            if stem.isdigit():
                entry["started_at_unix_ms"] = int(stem) // 1_000_000
            if args.download:
                raw = client.get_object(Bucket=args.bucket, Key=obj["Key"])["Body"].get_raw_stream().read()
                try:
                    data = gzip.decompress(raw)
                except OSError:
                    data = raw
                out_dir = os.path.join(os.path.expanduser(args.download), case_id)
                os.makedirs(out_dir, exist_ok=True)
                name = os.path.basename(obj["Key"])
                if name.endswith(".gz"):
                    name = name[:-3]
                out_path = os.path.join(out_dir, name)
                with open(out_path, "wb") as fh:
                    fh.write(data)
                entry["downloaded_to"] = out_path
                try:
                    entry["summary"] = summarize_raw_record(json.loads(data))
                except ValueError:
                    entry["summary"] = {"error": "not json"}
            objects.append(entry)
        return objects


def build_hops(usage: list[dict], ops: list[dict]) -> list[dict]:
    """按时间排出每一跳：ops upstream_errors 里的失败，加上 usage_logs 的最终成功。"""
    hops: list[dict] = []
    seen = set()
    for row in ops:
        for ev in row.get("upstream_errors") or []:
            key = (ev.get("at_unix_ms"), ev.get("account_id"), ev.get("upstream_status_code"), ev.get("kind"))
            if key in seen:
                continue
            seen.add(key)
            hops.append(
                {
                    "result": "failed",
                    "at": fmt_time(ev.get("at_unix_ms")),
                    "at_unix_ms": ev.get("at_unix_ms"),
                    "account_id": ev.get("account_id"),
                    "account_name": ev.get("account_name"),
                    "status": ev.get("upstream_status_code"),
                    "kind": ev.get("kind"),
                    "upstream_request_id": ev.get("upstream_request_id") or "",
                    "upstream_cf_ray": ev.get("upstream_cf_ray") or "",
                    "message": (ev.get("message") or ev.get("detail") or "")[:300],
                    "upstream_url": ev.get("upstream_url") or "",
                }
            )
    hops.sort(key=lambda h: h.get("at_unix_ms") or 0)
    for row in usage:
        hops.append(
            {
                "result": "served",
                "at": fmt_time(row.get("created_at")),
                "at_unix_ms": parse_unix_ms(row.get("created_at")),
                "account_id": row.get("account_id"),
                "account_name": row.get("account_name"),
                "status": 200,
                "upstream_request_id": row.get("upstream_request_id") or "",
                "upstream_request_id_header": row.get("upstream_request_id_header") or "",
                "first_token_ms": row.get("first_token_ms"),
                "duration_ms": row.get("duration_ms"),
            }
        )
    return hops


def print_report(report: dict) -> None:
    print(f"case: {report['case_id']}")
    if report.get("newapi"):
        for item in report["newapi"]:
            print(
                f"  NewAPI request_id={item.get('request_id')} → Sub2API client_request_id={item.get('upstream_request_id')}"
                f"  channel={item.get('channel')} model={item.get('model_name')} use_time={item.get('use_time')}s"
            )
    usage, ops = report["usage_logs"], report["ops_error_logs"]
    if not usage and not ops:
        print("  Sub2API 里没有这条请求（usage_logs / ops_error_logs 都没查到）")
    for row in usage:
        print("\n最终服务")
        print(f"  账号      #{row['account_id']} {row.get('account_name') or ''}")
        hdr = row.get("upstream_request_id_header") or "（账号未配 upstream_request_id_header）"
        print(f"  上游 ID   {row.get('upstream_request_id') or '-'}   [头：{hdr}]")
        print(f"  时间      {fmt_time(row.get('created_at'))}（UTC+8，写入时间）")
        print(f"  首字      {fmt_ms(row.get('first_token_ms'))}    耗时 {fmt_ms(row.get('duration_ms'))}    stream={row.get('stream')}")
        print(
            f"  模型      {row.get('model')} → {row.get('upstream_model') or '-'}   {row.get('inbound_endpoint') or ''}"
            f"   tokens in/out/cache {row.get('input_tokens')}/{row.get('output_tokens')}/{row.get('cache_read_tokens')}"
        )
    for row in ops:
        print(f"\nops_error_logs #{row['id']}  {fmt_time(row.get('created_at'))}")
        print(
            f"  客户端状态 {row.get('status_code')}   最后上游状态 {row.get('upstream_status_code')}   "
            f"{row.get('error_type')}: {row.get('error_message') or ''}"
        )
    cos = report.get("cos")
    started_ms = None
    if isinstance(cos, list):
        starts = [obj["started_at_unix_ms"] for obj in cos if obj.get("started_at_unix_ms")]
        started_ms = min(starts) if starts else None
    print("\n各跳（按时间）")
    if started_ms:
        print(f"  0. {fmt_time(started_ms)}  请求进入 Sub2API（COS 原文的开始时间）")
    if not report["hops"]:
        print("  （无）")
    for i, hop in enumerate(report["hops"], 1):
        who = f"#{hop.get('account_id')} {hop.get('account_name') or ''}".strip()
        if started_ms and hop.get("at_unix_ms"):
            who = f"+{(hop['at_unix_ms'] - started_ms) / 1000:.1f}s  " + who
        if hop["result"] == "served":
            line = f"  {i}. {hop['at']}  {who}  200 成功  上游ID={hop['upstream_request_id'] or '-'}"
            line += f"  首字 {fmt_ms(hop.get('first_token_ms'))}  耗时 {fmt_ms(hop.get('duration_ms'))}"
        else:
            ids = f"上游ID={hop['upstream_request_id'] or '-'}"
            if hop.get("upstream_cf_ray"):
                ids += f"  cf-ray={hop['upstream_cf_ray']}"
            line = f"  {i}. {hop['at']}  {who}  {hop.get('status')} {hop.get('kind') or ''}  {ids}"
            if hop.get("message"):
                line += f"  | {hop['message']}"
        print(line)
    print("\nCOS 原文")
    if cos is None:
        print("  （跳过）")
    elif isinstance(cos, dict) and cos.get("error"):
        print(f"  查询失败：{cos['error']}")
    elif not cos:
        print(f"  {report['cos_prefix']} 下没有对象（未开落盘、超过 30 天已过期，或 ID 不对）")
    else:
        for obj in cos:
            print(f"  {obj['url']}  {obj['size']} 字节  {obj.get('last_modified') or ''}")
            if obj.get("downloaded_to"):
                print(f"    已解压到 {obj['downloaded_to']}")
                print(f"    {json.dumps(obj.get('summary') or {}, ensure_ascii=False)}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="按 case 查 sv-1 Sub2API 全链路（只读）")
    parser.add_argument("case_id", help="Sub2API client_request_id（= X-Oneapi-Request-Id），或 NewAPI 的 request_id")
    parser.add_argument("--host", default=os.environ.get("SV1_SSH_HOST", DEFAULT_HOST))
    parser.add_argument("--ssh-key", default=os.environ.get("SV1_SSH_KEY", DEFAULT_SSH_KEY))
    parser.add_argument("--pg-container", default=DEFAULT_PG_CONTAINER)
    parser.add_argument("--no-cos", action="store_true", help="不查 COS 原文")
    parser.add_argument("--download", metavar="DIR", help="把原文下载解压到 DIR/<id>/")
    parser.add_argument("--socks-port", type=int, default=None, help="复用已有的 ssh -D 端口（如 18080）；不给就临时起一个")
    parser.add_argument("--bucket", default=DEFAULT_BUCKET)
    parser.add_argument("--region", default=DEFAULT_REGION)
    parser.add_argument("--prefix", default=DEFAULT_PREFIX)
    parser.add_argument("--credential", default=DEFAULT_CREDENTIAL, help="tccli 凭据文件（secretId/secretKey）")
    parser.add_argument("--newapi", action="store_true", help="强制把输入当 NewAPI request_id 换算")
    parser.add_argument("--newapi-base-url", default=os.environ.get("NEWAPI_BASE_URL", DEFAULT_NEWAPI_BASE_URL))
    parser.add_argument("--json", action="store_true", help="输出 JSON")
    args = parser.parse_args(argv)

    case_id = args.case_id.strip()
    if not CASE_ID_RE.match(case_id):
        parser.error(f"case id 含非法字符：{case_id!r}")
    sv1 = SV1(args.host, args.ssh_key, args.pg_container)
    report: dict[str, Any] = {"case_id": case_id}

    def lookup(cid: str) -> tuple[list, list]:
        return sv1.query_json(USAGE_SQL, cid), sv1.query_json(OPS_SQL, cid)

    usage: list = []
    ops: list = []
    if not args.newapi:
        usage, ops = lookup(case_id)
    if args.newapi or (not usage and not ops and not UUID_RE.match(case_id)):
        token = os.environ.get("NEWAPI_ACCESS_TOKEN", "")
        user_id = os.environ.get("NEWAPI_USER_ID", "")
        if not token or not user_id:
            msg = "按 NewAPI request_id 换算需要环境变量 NEWAPI_ACCESS_TOKEN 和 NEWAPI_USER_ID（NewAPI 个人设置里生成的系统访问令牌和用户 ID）"
            if args.newapi:
                raise TraceError(msg)
            print(f"提示：{msg}", file=sys.stderr)
        else:
            items = resolve_newapi_request_id(case_id, args.newapi_base_url, token, user_id)
            report["newapi"] = [
                {k: item.get(k) for k in ("request_id", "upstream_request_id", "channel", "model_name", "use_time", "created_at", "type")}
                for item in items
            ]
            upstream_ids = [item.get("upstream_request_id") for item in items if item.get("upstream_request_id")]
            if not upstream_ids:
                raise TraceError("NewAPI 日志里这条请求没有 upstream_request_id（可能不是走 sv-1 Sub2API 的渠道）")
            case_id = upstream_ids[0]
            report["case_id"] = case_id
            usage, ops = lookup(case_id)

    report["usage_logs"] = usage
    report["ops_error_logs"] = ops
    report["hops"] = build_hops(usage, ops)
    report["cos_prefix"] = f"cos://{args.bucket}/{args.prefix.strip('/')}/{case_id}/"
    if args.no_cos:
        report["cos"] = None
    else:
        try:
            report["cos"] = fetch_cos(sv1, case_id, args)
        except Exception as exc:  # COS 查不到不影响数据库部分的结论
            report["cos"] = {"error": f"{type(exc).__name__}: {exc}"}

    if args.json:
        print(json.dumps(report, ensure_ascii=False, indent=2, default=str))
    else:
        print_report(report)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except TraceError as exc:
        print(f"错误：{exc}", file=sys.stderr)
        sys.exit(2)
