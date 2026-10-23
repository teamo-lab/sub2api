import contextlib
import io
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import sv_case_trace as t  # noqa: E402


class BuildHopsTest(unittest.TestCase):
    def test_failover_then_served(self):
        ops = [
            {
                "upstream_errors": [
                    {
                        "kind": "failover",
                        "detail": "error code: 524",
                        "account_id": 2,
                        "account_name": "avemujica 0.5x",
                        "at_unix_ms": 1792729671371,
                        "upstream_status_code": 524,
                        "upstream_cf_ray": "8f1e2d3c4b5a6978-SJC",
                    }
                ]
            }
        ]
        usage = [
            {
                "created_at": "2026-10-23T04:28:21.127743+00:00",
                "account_id": 3,
                "account_name": "codebot 0.15x",
                "upstream_request_id": "codebot-1",
                "upstream_request_id_header": "X-Client-Request-Id",
                "first_token_ms": None,
                "duration_ms": 29748,
            }
        ]
        hops = t.build_hops(usage, ops)
        self.assertEqual([h["result"] for h in hops], ["failed", "served"])
        self.assertEqual(hops[0]["status"], 524)
        self.assertEqual(hops[0]["upstream_cf_ray"], "8f1e2d3c4b5a6978-SJC")
        self.assertEqual(hops[0]["message"], "error code: 524")
        self.assertEqual(hops[0]["at"], "2026-10-23 12:27:51.371")
        self.assertEqual(hops[1]["account_id"], 3)
        self.assertEqual(hops[1]["upstream_request_id"], "codebot-1")
        self.assertEqual(hops[1]["at_unix_ms"], 1792729701127)

    def test_duplicate_events_across_rows_are_merged(self):
        ev = {"account_id": 1, "at_unix_ms": 1, "upstream_status_code": 502, "kind": "failover"}
        hops = t.build_hops([], [{"upstream_errors": [ev]}, {"upstream_errors": [dict(ev)]}])
        self.assertEqual(len(hops), 1)


class CaseIDTest(unittest.TestCase):
    def test_rejects_shell_and_sql_metacharacters(self):
        self.assertTrue(t.CASE_ID_RE.match("0f646b8e-05cc-4c4e-adee-b14d9d190f21"))
        self.assertTrue(t.CASE_ID_RE.match("20261023122821123456789abcdef"))
        for bad in ["x'; drop table usage_logs;--", "a b", "$(id)", "", "a" * 129]:
            self.assertFalse(t.CASE_ID_RE.match(bad), bad)

    def test_query_json_refuses_bad_id_before_ssh(self):
        with self.assertRaises(t.TraceError):
            t.SV1("host", "~/.ssh/id_rsa", "pg").query_json("select 1", "x'y")


class NewAPIResolveTest(unittest.TestCase):
    def test_resolves_upstream_request_id_with_admin_headers(self):
        seen = {}

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):  # noqa: N802
                seen["path"] = self.path
                seen["auth"] = self.headers.get("Authorization")
                seen["user"] = self.headers.get("New-Api-User")
                body = json.dumps(
                    {
                        "success": True,
                        "data": {
                            "items": [
                                {"request_id": "20261023122821abc", "upstream_request_id": "0f646b8e-05cc-4c4e-adee-b14d9d190f21"},
                                {"request_id": "other", "upstream_request_id": "x"},
                            ]
                        },
                    }
                ).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            items = t.resolve_newapi_request_id(
                "20261023122821abc", f"http://127.0.0.1:{server.server_port}/", "tok", "1"
            )
        finally:
            server.shutdown()
        self.assertEqual([i["upstream_request_id"] for i in items], ["0f646b8e-05cc-4c4e-adee-b14d9d190f21"])
        self.assertIn("request_id=20261023122821abc", seen["path"])
        self.assertTrue(seen["path"].startswith("/api/log/?"))
        self.assertEqual((seen["auth"], seen["user"]), ("tok", "1"))


class OpsRowHopTest(unittest.TestCase):
    # sv-1 case 137dbcae：流内 GOAWAY，ops 行带 account_id 但 upstream_errors 为空。
    OPS_ROW = {
        "id": 130,
        "created_at": "2026-10-23T06:14:49.471017+00:00",
        "account_id": 4,
        "account_name": "switchbase-0.2x",
        "status_code": 502,
        "upstream_status_code": None,
        "error_type": "upstream_error",
        "error_message": "Upstream request failed",
        "upstream_error_message": None,
        "error_body": 'data: {"error":{"code":"stream_read_error","message":"stream_read_error","type":"upstream_error"},"sequence_number":0,"type":"error"}',
        "upstream_errors": None,
    }

    def test_ops_row_without_upstream_errors_becomes_a_hop(self):
        hops = t.build_hops([], [dict(self.OPS_ROW)])
        self.assertEqual(len(hops), 1)
        hop = hops[0]
        self.assertEqual(hop["source"], "ops_row")
        self.assertEqual((hop["account_id"], hop["account_name"], hop["status"]), (4, "switchbase-0.2x", 502))
        self.assertEqual(hop["at"], "2026-10-23 14:14:49.471")
        self.assertEqual(hop["message"], "Upstream request failed [stream_read_error]")
        self.assertEqual(hop["upstream_request_id"], "")

    def test_ops_row_prefers_upstream_status_and_message(self):
        row = dict(self.OPS_ROW, upstream_status_code=503, upstream_error_message="overloaded", error_body="")
        hop = t.build_hops([], [row])[0]
        self.assertEqual((hop["status"], hop["message"]), (503, "overloaded"))

    def test_ops_row_without_account_is_skipped(self):
        self.assertEqual(t.build_hops([], [dict(self.OPS_ROW, account_id=None)]), [])

    def test_ops_row_with_upstream_errors_uses_events_only(self):
        row = dict(self.OPS_ROW, upstream_errors=[{"account_id": 4, "at_unix_ms": 1, "upstream_status_code": 502, "kind": "stream_error", "upstream_request_id": "sb-1"}])
        hops = t.build_hops([], [row])
        self.assertEqual(len(hops), 1)
        self.assertEqual((hops[0]["kind"], hops[0]["upstream_request_id"]), ("stream_error", "sb-1"))
        self.assertNotIn("source", hops[0])

    def test_report_prints_ops_row_hop(self):
        report = {
            "case_id": "137dbcae-f72c-404f-8927-bf916f54dbcf",
            "usage_logs": [],
            "ops_error_logs": [dict(self.OPS_ROW)],
            "hops": t.build_hops([], [dict(self.OPS_ROW)]),
            "cos": [{"url": "cos://b/raw/137dbcae/1792736042098423284.json.gz", "size": 26427147,
                     "last_modified": "2026-10-23T06:14:49.000Z", "started_at_unix_ms": 1792736042098}],
            "cos_prefix": "cos://b/raw/137dbcae/",
        }
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            t.print_report(report)
        text = out.getvalue()
        self.assertNotIn("（无）", text)
        self.assertIn("+47.4s  #4 switchbase-0.2x  502 upstream_error  上游ID=-  | Upstream request failed [stream_read_error]", text)
        self.assertIn("写入 2026-10-23 14:14:49.000", text)
        self.assertNotIn("+00:00", text)
        self.assertNotIn("06:14:49", text)


class TimeFormatTest(unittest.TestCase):
    def test_postgres_json_timestamps_with_any_fraction_are_utc8(self):
        # case 2be045e7：Postgres 去掉末尾 0 后只剩 5 位小数，Python 3.10 fromisoformat 解析不了。
        self.assertEqual(t.fmt_time("2026-10-23T06:20:37.34544+00:00"), "2026-10-23 14:20:37.345")
        self.assertEqual(t.fmt_time("2026-10-23T06:20:37+00:00"), "2026-10-23 14:20:37.000")
        self.assertEqual(t.fmt_time("2026-10-23T06:20:37.3+00:00"), "2026-10-23 14:20:37.300")
        self.assertEqual(t.fmt_time("2026-10-23T14:14:49.471017+08:00"), "2026-10-23 14:14:49.471")
        self.assertEqual(t.fmt_time("2026-10-23 06:20:37.1234567"), "2026-10-23 14:20:37.123")
        self.assertEqual(t.fmt_time("2026-10-23T06:14:49.000Z"), "2026-10-23 14:14:49.000")
        self.assertEqual(t.fmt_time("2026-10-23T06:14:49-0700"), "2026-10-23 21:14:49.000")
        self.assertEqual(t.fmt_time(1792729671371), "2026-10-23 12:27:51.371")
        self.assertEqual(t.fmt_time(None), "-")
        self.assertEqual(t.fmt_time("garbage"), "garbage")

    def test_parse_unix_ms_handles_five_digit_fraction(self):
        self.assertEqual(t.parse_unix_ms("2026-10-23T06:20:37.34544+00:00"), 1792736437345)
        self.assertIsNone(t.parse_unix_ms("garbage"))

    def test_served_hop_and_report_use_utc8(self):
        usage = [{
            "created_at": "2026-10-23T06:20:37.34544+00:00",
            "account_id": 4,
            "account_name": "switchbase-0.2x",
            "upstream_request_id": "72ed0c7d-bbdd-4933-a583-e16e20d52a03",
            "upstream_request_id_header": "X-Client-Request-Id",
            "first_token_ms": 20710,
            "duration_ms": 21410,
            "stream": True,
        }]
        ops = [{
            "id": 131,
            "created_at": "2026-10-23T06:20:37.344+00:00",
            "account_id": 4,
            "status_code": 200,
            "upstream_status_code": 503,
            "error_type": "upstream_error",
            "error_message": "Recovered upstream error 503",
            "upstream_errors": [{"account_id": 4, "account_name": "switchbase-0.2x", "at_unix_ms": 1792736415428,
                                 "upstream_status_code": 503, "kind": "failover",
                                 "upstream_request_id": "3847a731-e03c-405c-aeb6-160ce2fc1b66"}],
        }]
        hops = t.build_hops(usage, ops)
        self.assertEqual([h["result"] for h in hops], ["failed", "served"])
        self.assertEqual(hops[1]["at"], "2026-10-23 14:20:37.345")
        self.assertEqual(hops[1]["at_unix_ms"], 1792736437345)
        report = {"case_id": "2be045e7-d9b2-47f6-8daa-27706135c420", "usage_logs": usage, "ops_error_logs": ops,
                  "hops": hops, "cos": None, "cos_prefix": ""}
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            t.print_report(report)
        text = out.getvalue()
        self.assertIn("时间      2026-10-23 14:20:37.345（UTC+8，写入时间）", text)
        self.assertIn("2. 2026-10-23 14:20:37.345  #4 switchbase-0.2x  200 成功", text)
        self.assertNotIn("+00:00", text)


if __name__ == "__main__":
    unittest.main()
