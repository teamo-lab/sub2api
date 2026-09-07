#!/usr/bin/env python3
"""Open one long Responses SSE stream and report terminal continuity."""

import argparse
import json
import os
import pathlib
import time
import urllib.request


def atomic_signal(path, payload):
    target = pathlib.Path(path)
    temporary = target.with_name(target.name + ".tmp")
    temporary.write_text(json.dumps(payload, separators=(",", ":")) + "\n", encoding="utf-8")
    os.chmod(temporary, 0o600)
    temporary.replace(target)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--ready-file", required=True)
    parser.add_argument("--timeout", type=float, default=300.0)
    args = parser.parse_args()
    api_key = os.environ.get("SUB2API_CANARY_KEY")
    if not api_key:
        raise SystemExit("SUB2API_CANARY_KEY is required")

    marker = "STREAM_SWITCH_TEST_DONE"
    body = json.dumps({
        "model": "gpt-5.6-sol",
        "input": (
            "Write a detailed 1200-word technical explanation of why changing HAProxy backend "
            "weights must not terminate an already established SSE response. Use multiple "
            f"sections and finish with the exact marker {marker}."
        ),
        "stream": True,
        "store": False,
        "max_output_tokens": 2048,
        "reasoning": {"effort": "medium"},
    }).encode()
    request = urllib.request.Request(args.url, data=body, method="POST", headers={
        "Authorization": "Bearer " + api_key,
        "Content-Type": "application/json",
        "User-Agent": "codex_cli_rs/0.101.0",
        "X-Sub2API-Stream-Recovery": "text-continuation",
    })

    started = time.monotonic()
    terminal = ""
    marker_seen = False
    event_count = 0
    with urllib.request.urlopen(request, timeout=args.timeout) as response:
        slot = response.headers.get("X-Sub2API-Deployment-Slot", "")
        atomic_signal(args.ready_file, {"connected": True, "slot": slot, "status": response.status})
        for raw_line in response:
            if not raw_line.startswith(b"data:"):
                continue
            try:
                event = json.loads(raw_line[5:].strip())
            except (UnicodeDecodeError, ValueError):
                continue
            event_count += 1
            event_type = event.get("type", "")
            if event_type in {"response.completed", "response.failed"}:
                terminal = event_type
            if marker in json.dumps(event, ensure_ascii=False):
                marker_seen = True

    print(json.dumps({
        "elapsed_seconds": round(time.monotonic() - started, 3),
        "event_count": event_count,
        "marker_seen": marker_seen,
        "terminal": terminal or "none",
    }, separators=(",", ":"), sort_keys=True))


if __name__ == "__main__":
    main()
