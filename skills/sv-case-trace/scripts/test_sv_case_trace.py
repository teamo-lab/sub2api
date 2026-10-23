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


if __name__ == "__main__":
    unittest.main()
