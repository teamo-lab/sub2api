#!/usr/bin/env python3
"""Explicit candidate-slot GPT recovery gate; creates only isolated test fixtures.

Run by the release owner on the target host after the fleet lease check. This is
not a read-only command. No production rule/account is changed, and no credentials
or request/response payloads are written to its durable evidence ledger.
"""

import argparse
import hashlib
import http.server
import json
import os
import pathlib
import secrets
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid


ROOT = pathlib.Path("/opt/sub2api/deploy")
TYPE_ONLY = {"error": {"type": "service_unavailable_error", "message": "Our servers are currently overloaded. Please try again later."}}
BARE_ERROR = {"type": "error", "error": {"code": "server_error", "message": "Internal server error"}}
ENCRYPTED_ERROR = {"type": "error", "error": {"code": "invalid_encrypted_content", "type": "invalid_request_error", "message": "Encrypted content could not be verified"}}


class GateError(Exception):
    """Only fixed non-sensitive error codes are exposed to the operator."""


def check(condition, code):
    if not condition:
        raise GateError(code)


def canonical(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def fingerprint(value):
    return hashlib.sha256(canonical(value).encode()).hexdigest()


def scoped_contains(values, value):
    return not values or value.lower() in [str(v).strip().lower() for v in values]


def select_rule(rules, model, status, code, envelope):
    """Mirror matchRecoveryRule; equal-priority ambiguity fails closed."""
    matched = []
    body = canonical(envelope).lower()[:8192]
    for rule in rules:
        policy = rule.get("recovery_policy") or {}
        if not rule.get("enabled") or policy.get("mode") not in ("limited", "return"):
            continue
        if rule.get("platforms") != ["openai"] or rule.get("skip_monitoring"):
            continue
        if not all(scoped_contains(policy.get(field), value) for field, value in
                   (("account_types", "apikey"), ("models", model), ("upstream_codes", code))):
            continue
        if not policy.get("upstream_codes"):
            continue
        errors, keywords = rule.get("error_codes") or [], rule.get("keywords") or []
        conditions = ([status in errors] if errors else []) + ([any(str(k).lower() in body for k in keywords)] if keywords else [])
        if not conditions or not (all(conditions) if rule.get("match_mode") == "all" else any(conditions)):
            continue
        same, switches, budget = (policy.get(k, 0) for k in ("same_account_retries", "account_switches", "budget_seconds"))
        if not (0 <= same <= 10 and 0 <= switches <= 10 and 1 <= budget <= 120):
            continue
        matched.append(rule)
    matched.sort(key=lambda r: r.get("priority", 0))
    check(matched, "required_limited_recovery_rule_missing")
    check(len(matched) < 2 or matched[0].get("priority", 0) != matched[1].get("priority", 0), "ambiguous_recovery_rule_priority")
    rule = matched[0]
    policy = rule["recovery_policy"]
    check(policy["mode"] == "limited" and policy.get("account_switches", 0) >= 1, "rule_does_not_allow_recovery")
    maximum = (1 + policy.get("same_account_retries", 0)) * (1 + policy.get("account_switches", 0))
    check(maximum <= 8, "rule_fixture_attempt_limit_exceeded")
    minimum_delay = (1 + policy.get("account_switches", 0)) * sum(min(0.5 * 2 ** i, 8) for i in range(policy.get("same_account_retries", 0)))
    check(minimum_delay + 3 < policy["budget_seconds"], "rule_budget_too_short_for_exact_attempt_gate")
    return {"id": rule["id"], "same_account_retries": policy.get("same_account_retries", 0),
            "account_switches": policy.get("account_switches", 0), "budget_seconds": policy["budget_seconds"], "max_attempts": maximum}


def sse_event(value):
    return ("data: " + canonical(value) + "\n\n").encode()


def parse_events(body):
    events = []
    for line in body.decode("utf-8", errors="replace").splitlines():
        if line.startswith("data:"):
            try:
                events.append(json.loads(line[5:].strip()))
            except (ValueError, TypeError):
                pass
    return [event for event in events if isinstance(event, dict)]


class Scenario:
    def __init__(self, kind):
        self.kind = kind
        self.calls = []
        self.capability_probes = 0
        self.lock = threading.Lock()
        self.first_has_cipher = False
        self.repair_preserves_summary = False
        self.repair_preserves_message = False
        self.repair_removes_cipher = False

    def response(self, ordinal, payload):
        # Account creation asynchronously probes Responses tool support. Reply
        # conclusively to that synthetic probe without consuming a fault attempt.
        if payload.get("stream") is False and payload.get("tool_choice") == "required" and any(isinstance(tool, dict) and tool.get("name") == "probe_ping" for tool in payload.get("tools", [])):
            with self.lock:
                self.capability_probes += 1
            body = {"id": "resp_capability_fixture", "object": "response", "status": "completed", "output": [{"type": "function_call", "id": "fc_probe", "call_id": "call_probe", "name": "probe_ping", "arguments": '{"ok":true}'}], "usage": {"input_tokens": 1, "output_tokens": 1}}
            return 200, "application/json", canonical(body).encode()
        with self.lock:
            self.calls.append(ordinal)
            attempt = len(self.calls)
            if self.kind.startswith("encrypted"):
                items = payload.get("input") or []
                first = items[0] if isinstance(items, list) and items and isinstance(items[0], dict) else {}
                if attempt == 1:
                    self.first_has_cipher = first.get("encrypted_content") == "synthetic-cipher"
                else:
                    self.repair_removes_cipher = "encrypted_content" not in first
                    self.repair_preserves_summary = first.get("summary") == [{"type": "summary_text", "text": "retain synthetic summary"}]
                    self.repair_preserves_message = len(items) == 2 and items[1] == {"type": "message", "role": "user", "content": "synthetic followup"}
        if self.kind == "type_only_exhausted":
            return 503, "application/json", canonical(TYPE_ONLY).encode()
        if self.kind == "bare_sse_fallback" and ordinal == 0:
            return 200, "text/event-stream", sse_event(BARE_ERROR)
        if self.kind == "already_output":
            return 200, "text/event-stream", sse_event({"type": "response.output_text.delta", "delta": "synthetic partial answer"}) + sse_event(BARE_ERROR)
        if self.kind.startswith("encrypted") and attempt == 1:
            return 200, "text/event-stream", sse_event(ENCRYPTED_ERROR)
        return 200, "text/event-stream", sse_event({"type": "response.output_text.delta", "delta": "synthetic recovered answer"}) + sse_event({"type": "response.completed", "response": {"id": "resp_synthetic_candidate_gate", "status": "completed", "usage": {"input_tokens": 1, "output_tokens": 1}}})


class CandidateProbe:
    def __init__(self, args):
        self.args = args
        self.tag = "gpt-candidate-" + uuid.uuid4().hex
        self.path = ROOT / "rollout/runtime" / (self.tag + ".json")
        self.ledger = {"schema_version": 1, "tag": self.tag, "slot": args.slot, "release_owner": args.release_owner,
                       "started_at": int(time.time()), "status": "preflight", "fixtures": [], "results": [], "cleanup_errors": []}
        self.admin_headers = {}
        self.token = None
        self.mock = None
        self.scenarios = {}
        self.production_before = None
        self.rules_before = None
        self.persist()

    def persist(self):
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        temporary = self.path.with_suffix(".tmp")
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as file:
            json.dump(self.ledger, file, ensure_ascii=False, indent=2)
        os.replace(temporary, self.path)

    def command(self, *argv):
        try:
            return subprocess.check_output(argv, stderr=subprocess.DEVNULL, timeout=25).decode().strip()
        except (subprocess.SubprocessError, OSError):
            raise GateError("local_dependency_command_failed") from None

    def api(self, method, path, body=None, user=False):
        check(method in ("GET", "POST", "DELETE"), "mutation_method_forbidden")
        if method == "POST":
            check(path in ("/api/v1/auth/login", "/api/v1/admin/groups", "/api/v1/admin/accounts", "/api/v1/admin/users", "/api/v1/keys"), "mutation_route_forbidden")
        if method == "DELETE":
            owned = [("/api/v1/keys/" if item["kind"] == "keys" else "/api/v1/admin/" + item["kind"] + "/") + str(item["id"]) for item in self.ledger["fixtures"]]
            check(path in owned, "delete_unowned_fixture_forbidden")
        headers = {"Content-Type": "application/json", **self.admin_headers}
        if user:
            headers = {"Content-Type": "application/json", "Authorization": "Bearer " + self.token}
        request = urllib.request.Request(self.base + path, data=canonical(body).encode() if body is not None else None, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=25) as response:
                result = json.load(response)
        except urllib.error.HTTPError as error:
            raise GateError("fixture_api_http_" + str(error.code)) from None
        except Exception:
            raise GateError("fixture_api_response_uncertain") from None
        check(isinstance(result, dict) and result.get("code", 0) == 0, "fixture_api_failed")
        return result.get("data", result)

    def create(self, kind, body, user=False):
        route = "/api/v1/keys" if kind == "keys" else "/api/v1/admin/" + kind
        self.ledger["pending_creation"] = {"kind": kind, "name": body.get("name") or body.get("email")}
        self.persist()
        result = self.api("POST", route, body, user)
        check(isinstance(result, dict) and isinstance(result.get("id"), int), "fixture_creation_missing_id")
        self.ledger["fixtures"].append({"kind": kind, "id": result["id"], "identity": body.get("name") or body.get("email"), "deleted": False})
        self.ledger.pop("pending_creation", None)
        self.persist()
        return result

    def production_snapshot(self, ids=None):
        # Hash credentials inside PostgreSQL; only IDs and hashes leave it.
        where = "deleted_at IS NULL"
        if ids is not None:
            check(all(isinstance(value, int) for value in ids), "invalid_snapshot_ids")
            where = "id IN (" + ",".join(str(value) for value in ids) + ")" if ids else "false"
        sql = "SELECT coalesce(json_object_agg(id, fingerprint),'{}') FROM (SELECT id, md5(jsonb_build_object('status',status,'schedulable',schedulable,'concurrency',concurrency,'priority',priority,'load_factor',load_factor,'credentials',credentials,'extra',extra,'proxy_id',proxy_id,'deleted_at',deleted_at)::text) AS fingerprint FROM accounts WHERE " + where + ") t"
        return json.loads(self.command(str(ROOT / "rollout/dependency-client"), "psql", "-X", "-Atq", "-c", sql))

    def preflight(self):
        info = json.loads(self.command("docker", "inspect", "sub2api-" + self.args.slot))[0]
        check(info.get("State", {}).get("Health", {}).get("Status") == "healthy", "candidate_not_healthy")
        check(info.get("Image") == self.args.expect_image, "candidate_image_mismatch")
        binary = self.command("docker", "exec", "sub2api-" + self.args.slot, "sha256sum", "/app/sub2api").split()[0]
        check(binary == self.args.expect_binary_sha256, "candidate_binary_mismatch")
        networks = list(info["NetworkSettings"]["Networks"].values())
        check(len(networks) == 1 and networks[0].get("Gateway") and networks[0].get("IPAddress"), "ambiguous_candidate_network")
        self.base = "http://" + networks[0]["IPAddress"] + ":8080"
        self.gateway = networks[0]["Gateway"]
        self.ledger.update({"image": info["Image"], "binary_sha256": binary})
        key = self.command(str(ROOT / "rollout/dependency-client"), "psql", "-X", "-Atq", "-c", "SELECT value FROM settings WHERE key='admin_api_key';")
        if key.startswith('"'):
            key = json.loads(key)
        if key:
            self.admin_headers = {"x-api-key": key}
        else:
            env = dict(value.split("=", 1) for value in info["Config"]["Env"] if "=" in value)
            check(env.get("ADMIN_EMAIL") and env.get("ADMIN_PASSWORD"), "admin_auth_unavailable")
            auth = self.api("POST", "/api/v1/auth/login", {"email": env["ADMIN_EMAIL"], "password": env["ADMIN_PASSWORD"]})
            check(auth.get("access_token"), "admin_auth_requires_verification")
            self.admin_headers = {"Authorization": "Bearer " + auth["access_token"]}
        self.rules_before = self.api("GET", "/api/v1/admin/error-passthrough-rules")
        check(isinstance(self.rules_before, list), "unrecognized_rule_list")
        self.type_rule = select_rule(self.rules_before, self.args.model, 503, "service_unavailable_error", TYPE_ONLY)
        self.bare_rule = select_rule(self.rules_before, self.args.model, 502, "server_error", BARE_ERROR)
        self.production_before = self.production_snapshot()
        self.ledger.update({"rules_sha256": fingerprint(self.rules_before), "recovery_rules": {"type_only": self.type_rule, "bare_sse": self.bare_rule}, "production_account_count": len(self.production_before), "production_config_sha256": fingerprint(self.production_before), "status": "fixture_setup"})
        self.persist()

    def start_mock(self):
        probe = self

        class Mock(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                try:
                    parts = self.path.split("/")
                    scenario = probe.scenarios[parts[1]]
                    ordinal = int(parts[2])
                    length = int(self.headers.get("Content-Length", "0"))
                    check(0 < length < 65536, "unexpected_mock_payload_size")
                    payload = json.loads(self.rfile.read(length))
                    status, content_type, body = scenario.response(ordinal, payload)
                    self.send_response(status)
                    self.send_header("Content-Type", content_type)
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    pass
                except Exception:
                    self.send_error(400, "invalid synthetic request")

        self.mock = http.server.ThreadingHTTPServer((self.gateway, 0), Mock)
        threading.Thread(target=self.mock.serve_forever, daemon=True).start()

    def create_user(self, groups):
        password = secrets.token_urlsafe(32)
        email = self.tag + "@example.invalid"
        user = self.create("users", {"email": email, "password": password, "role": "user", "balance": 1, "concurrency": 1, "allowed_groups": groups, "restrict_public_groups": True, "notes": self.tag})
        auth = self.api("POST", "/api/v1/auth/login", {"email": email, "password": password})
        check(auth.get("access_token"), "fixture_user_login_failed")
        self.token = auth["access_token"]
        return user

    def run_cases(self):
        cases = [("type_only_exhausted", self.type_rule["account_switches"] + 1), ("bare_sse_fallback", 2), ("encrypted_normal", 1), ("encrypted_passthrough", 1), ("already_output", 2)]
        groups = {}
        for kind, count in cases:
            group = self.create("groups", {"name": self.tag + "-" + kind, "description": "Isolated candidate recovery verification", "platform": "openai", "rate_multiplier": 1, "is_exclusive": True, "subscription_type": "standard", "disable_chat_completions": False})
            groups[kind] = group["id"]
            self.scenarios[kind] = Scenario(kind)
            for ordinal in range(count):
                self.create("accounts", {"name": self.tag + "-" + kind + "-" + str(ordinal), "notes": self.tag, "platform": "openai", "type": "apikey", "credentials": {"api_key": "mock-only-" + secrets.token_hex(16), "base_url": "http://" + self.gateway + ":" + str(self.mock.server_address[1]) + "/" + kind + "/" + str(ordinal), "pool_mode": True, "pool_mode_retry_count": 0, "model_mapping": {self.args.model: self.args.model}}, "extra": {"openai_responses_mode": "auto", "use_responses_api": True, "openai_passthrough": kind == "encrypted_passthrough"}, "concurrency": 1, "priority": ordinal + 1, "group_ids": [group["id"]], "expires_at": int(time.time()) + 900})
        self.create_user(list(groups.values()))
        for kind, _ in cases:
            key = self.create("keys", {"name": self.tag + "-" + kind, "group_id": groups[kind], "expires_in_days": 1, "quota": 1}, user=True)
            payload = {"model": self.args.model, "stream": True, "store": False, "input": "synthetic probe"}
            if kind.startswith("encrypted"):
                payload["input"] = [{"type": "reasoning", "encrypted_content": "synthetic-cipher", "summary": [{"type": "summary_text", "text": "retain synthetic summary"}]}, {"type": "message", "role": "user", "content": "synthetic followup"}]
            request = urllib.request.Request(self.base + "/v1/responses", data=canonical(payload).encode(), headers={"Content-Type": "application/json", "Authorization": "Bearer " + key["key"]})
            self.ledger["active_case"] = kind
            self.persist()
            started = time.monotonic()
            try:
                with urllib.request.urlopen(request, timeout=135) as response:
                    status, body = response.status, response.read(131072)
            except urllib.error.HTTPError as error:
                status, body = error.code, error.read(131072)
            except Exception:
                raise GateError("candidate_request_incomplete") from None
            elapsed = time.monotonic() - started
            scenario, events = self.scenarios[kind], parse_events(body)
            completed = sum(event.get("type") == "response.completed" for event in events)
            deltas = sum(event.get("type") == "response.output_text.delta" for event in events)
            result = {"case": kind, "http": status, "attempts": list(scenario.calls), "capability_probes": scenario.capability_probes, "elapsed_seconds": round(elapsed, 3), "completed_events": completed, "delta_events": deltas, "ok": False}
            self.ledger["results"].append(result)
            self.persist()
            if kind == "type_only_exhausted":
                expected = [ordinal for ordinal in range(self.type_rule["account_switches"] + 1) for _ in range(self.type_rule["same_account_retries"] + 1)]
                terminal = json.loads(body).get("error", {}) if not events else (events[-1].get("error") or {})
                check(scenario.calls == expected, "type_only_attempt_budget_mismatch")
                check(terminal.get("recovery_exhausted") is True and terminal.get("recovery_rule_id") == self.type_rule["id"], "type_only_missing_rule_exhaustion")
                check(elapsed < self.type_rule["budget_seconds"] + 3 and not completed and not deltas, "type_only_deadline_or_terminal_mismatch")
            elif kind == "bare_sse_fallback":
                check(scenario.calls == [0] * (self.bare_rule["same_account_retries"] + 1) + [1], "bare_sse_did_not_follow_rule")
                check(status == 200 and completed == 1 and deltas == 1 and b"Internal server error" not in body, "bare_sse_recovery_failed")
            elif kind.startswith("encrypted"):
                check(scenario.calls == [0, 0] and scenario.first_has_cipher, "encrypted_attempt_mismatch")
                check(scenario.repair_removes_cipher and scenario.repair_preserves_summary and scenario.repair_preserves_message, "encrypted_repair_lost_context")
                check(status == 200 and completed == 1 and deltas == 1 and b"invalid_encrypted_content" not in body, "encrypted_recovery_failed")
                result["summary_and_message_preserved"] = True
            else:
                check(scenario.calls == [0] and deltas == 1 and not completed, "visible_output_replayed_or_succeeded")
                check(b"synthetic partial answer" in body and (b"server_error" in body or b"response.failed" in body), "visible_output_error_missing")
            result["ok"] = True
            self.persist()

    def cleanup(self):
        for fixture in sorted(self.ledger["fixtures"], key=lambda row: {"keys": 0, "accounts": 1, "users": 2, "groups": 3}[row["kind"]]):
            if fixture["deleted"]:
                continue
            kind, identifier = fixture["kind"], fixture["id"]
            route = "/api/v1/keys/" if kind == "keys" else "/api/v1/admin/" + kind + "/"
            try:
                current = self.api("GET", route + str(identifier), user=kind == "keys")
                identity = current.get("email") if kind == "users" else current.get("name")
                check(identity == fixture["identity"] and identity.startswith(self.tag), "fixture_identity_changed")
                self.api("DELETE", route + str(identifier), user=kind == "keys")
                fixture["deleted"] = True
            except Exception:
                self.ledger["cleanup_errors"].append({"kind": kind, "id": identifier, "code": "fixture_cleanup_failed"})
            self.persist()
        if self.mock:
            self.mock.shutdown()
            self.mock.server_close()

    def run(self):
        passed = False
        try:
            self.preflight()
            self.start_mock()
            self.run_cases()
            passed = True
        except Exception as error:
            self.ledger["error"] = str(error) if isinstance(error, GateError) else "unexpected_probe_failure"
        finally:
            self.cleanup()
            if self.rules_before is not None:
                try:
                    current = self.api("GET", "/api/v1/admin/error-passthrough-rules")
                    self.ledger["global_rules_unchanged"] = fingerprint(current) == fingerprint(self.rules_before)
                except Exception:
                    self.ledger["global_rules_unchanged"] = False
            if self.production_before is not None:
                try:
                    after = self.production_snapshot([int(value) for value in self.production_before])
                    self.ledger["production_accounts_unchanged"] = after == self.production_before
                    self.ledger["changed_production_ids"] = [int(key) for key in self.production_before if self.production_before[key] != after.get(key)]
                except Exception:
                    self.ledger["production_accounts_unchanged"] = False
            passed = passed and not self.ledger["cleanup_errors"] and not self.ledger.get("pending_creation") and self.ledger.get("global_rules_unchanged") and self.ledger.get("production_accounts_unchanged")
            self.ledger.update({"ok": bool(passed), "status": "passed" if passed else "failed", "completed_at": int(time.time())})
            self.persist()
            print(canonical(self.ledger))
        return 0 if passed else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--slot", choices=("blue", "green"), required=True)
    parser.add_argument("--expect-image", required=True, help="Exact immutable docker image ID (sha256:...)")
    parser.add_argument("--expect-binary-sha256", required=True)
    parser.add_argument("--model", choices=("gpt-5.6-sol", "gpt-6-astra"), default="gpt-6-astra")
    parser.add_argument("--release-owner", required=True, help="Owner already holding the fleet lease; checked by the audited caller")
    parser.add_argument("--execute-isolated-fixtures", action="store_true", help="Required acknowledgement that temporary fixture records will be created")
    args = parser.parse_args()
    if not args.execute_isolated_fixtures:
        parser.error("requires --execute-isolated-fixtures after the audited fleet lease check")
    return CandidateProbe(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
