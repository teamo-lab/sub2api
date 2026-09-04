#!/usr/bin/env python3
"""Fixed-rate Responses SSE load gate with TTFT, latency, cache, and terminal metrics."""

import argparse
import concurrent.futures
import json
import math
import os
import threading
import time
import urllib.error
import urllib.request


def percentile(values, p):
    if not values:
        return None
    ordered = sorted(values)
    index = min(len(ordered) - 1, math.ceil(p * len(ordered)) - 1)
    return round(ordered[index], 3)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--requests", type=int, default=1000)
    parser.add_argument("--duration", type=float, default=60.0)
    parser.add_argument("--concurrency", type=int, default=40)
    parser.add_argument("--timeout", type=float, default=120.0)
    parser.add_argument("--recovery", default="text-continuation")
    parser.add_argument("--user-agent", default="codex_cli_rs/0.141.0 (codex_cli_rs; 0.141.0)")
    parser.add_argument("--prompt-cache-key", default="sub2api-rollout-paired-v1")
    parser.add_argument("--cache-key-shards", type=int, default=8)
    parser.add_argument("--max-output-tokens", type=int, default=128)
    args = parser.parse_args()
    api_key = os.environ.get("SUB2API_CANARY_KEY")
    if not api_key:
        raise SystemExit("SUB2API_CANARY_KEY is required")
    if args.cache_key_shards < 1 or args.cache_key_shards > 64:
        raise SystemExit("cache-key-shards must be between 1 and 64")

    base_body = {
        "model": "gpt-5.6-sol",
        "stream": True,
        "store": False,
        "max_output_tokens": args.max_output_tokens,
        "reasoning": {"effort": "low"},
    }
    lock = threading.Lock()
    results = []

    def one(index):
        scheduled = started + index * args.duration / args.requests
        delay = scheduled - time.monotonic()
        if delay > 0:
            time.sleep(delay)
        request_started = time.monotonic()
        shard = index % args.cache_key_shards
        payload = dict(base_body)
        payload["input"] = f"Reply with exactly OK\nLoad shard: {shard}"
        payload["prompt_cache_key"] = f"{args.prompt_cache_key}-{shard}"
        body = json.dumps(payload).encode()
        req = urllib.request.Request(args.url, data=body, method="POST", headers={
            "Authorization": "Bearer " + api_key,
            "Content-Type": "application/json",
            "User-Agent": args.user_agent,
            "Originator": "codex_cli_rs",
            "X-Codex-Window-Id": f"sub2api-rollout-{shard}",
            "X-Sub2API-Stream-Recovery": args.recovery,
        })
        status = 0
        slot = ""
        terminal = ""
        ttft = None
        usage = {}
        error = ""
        try:
            with urllib.request.urlopen(req, timeout=args.timeout) as response:
                status = response.status
                slot = response.headers.get("X-Sub2API-Deployment-Slot", "")
                for raw_line in response:
                    if not raw_line.startswith(b"data:"):
                        continue
                    try:
                        event = json.loads(raw_line[5:].strip())
                    except (ValueError, UnicodeDecodeError):
                        continue
                    event_type = event.get("type", "")
                    if ttft is None and event_type in {
                        "response.output_text.delta",
                        "response.function_call_arguments.delta",
                        "response.custom_tool_call_input.delta",
                    }:
                        ttft = time.monotonic() - request_started
                    if event_type in {"response.completed", "response.failed"}:
                        terminal = event_type
                        usage = event.get("response", {}).get("usage") or {}
        except urllib.error.HTTPError as exc:
            status = exc.code
            error = "http_error"
            exc.read()
        except Exception as exc:  # noqa: BLE001 - load report needs classification
            error = type(exc).__name__
        result = {
            "status": status,
            "slot": slot,
            "terminal": terminal,
            "ttft": ttft,
            "latency": time.monotonic() - request_started,
            "usage": usage,
            "error": error,
        }
        with lock:
            results.append(result)

    wall_started = time.time()
    started = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [pool.submit(one, i) for i in range(args.requests)]
        for future in futures:
            future.result()
    wall_finished = time.time()

    statuses = {}
    terminals = {}
    errors = {}
    slots = {}
    for result in results:
        statuses[str(result["status"])] = statuses.get(str(result["status"]), 0) + 1
        key = result["terminal"] or "none"
        terminals[key] = terminals.get(key, 0) + 1
        if result["error"]:
            errors[result["error"]] = errors.get(result["error"], 0) + 1
        slot = result["slot"] or "unknown"
        slots[slot] = slots.get(slot, 0) + 1
    completed = [r for r in results if r["terminal"] == "response.completed"]
    ttfts = [r["ttft"] for r in completed if r["ttft"] is not None]
    latencies = [r["latency"] for r in completed]
    input_tokens = sum((r["usage"].get("input_tokens") or 0) for r in completed)
    cached_tokens = sum(((r["usage"].get("input_tokens_details") or {}).get("cached_tokens") or 0) for r in completed)
    output_tokens = sum((r["usage"].get("output_tokens") or 0) for r in completed)
    print(json.dumps({
        "started_unix": wall_started,
        "finished_unix": wall_finished,
        "elapsed_seconds": round(wall_finished - wall_started, 3),
        "requests": len(results),
        "target_qpm": round(args.requests * 60 / args.duration, 3),
        "concurrency": args.concurrency,
        "cache_key_shards": args.cache_key_shards,
        "statuses": statuses,
        "slots": slots,
        "terminals": terminals,
        "errors": errors,
        "ttft_seconds": {"p50": percentile(ttfts, .50), "p95": percentile(ttfts, .95), "p99": percentile(ttfts, .99), "max": round(max(ttfts), 3) if ttfts else None},
        "latency_seconds": {"p50": percentile(latencies, .50), "p95": percentile(latencies, .95), "p99": percentile(latencies, .99), "max": round(max(latencies), 3) if latencies else None},
        "tokens": {"input": input_tokens, "cached": cached_tokens, "output": output_tokens,
                   "cache_rate": round(cached_tokens / input_tokens, 6) if input_tokens else None},
    }, sort_keys=True))


if __name__ == "__main__":
    main()
