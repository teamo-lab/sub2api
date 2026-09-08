"""Offline safety/expectation tests. No live services or production fixtures."""

import copy
import unittest

from hk43_gpt_candidate_probe import BARE_ERROR, TYPE_ONLY, CandidateProbe, GateError, Scenario, parse_events, select_rule


def rule(**overrides):
    result = {"id": 7, "enabled": True, "priority": 10, "platforms": ["openai"], "error_codes": [503], "match_mode": "all", "skip_monitoring": False,
              "recovery_policy": {"mode": "limited", "account_types": ["apikey"], "models": ["gpt-6-astra"], "upstream_codes": ["service_unavailable_error"], "same_account_retries": 0, "account_switches": 1, "budget_seconds": 10}}
    result.update(overrides)
    return result


class RuleTests(unittest.TestCase):
    def select(self, rules, **overrides):
        args = {"model": "gpt-6-astra", "status": 503, "code": "service_unavailable_error", "envelope": TYPE_ONLY}
        args.update(overrides)
        return select_rule(rules, **args)

    def test_finds_type_only_rule_and_derives_attempts_without_mutation(self):
        rules = [rule()]
        before = copy.deepcopy(rules)
        result = self.select(rules)
        self.assertEqual(result["max_attempts"], 2)
        self.assertEqual(result["budget_seconds"], 10)
        self.assertEqual(rules, before)

    def test_missing_rule_wrong_model_or_return_mode_cannot_pass(self):
        for rules, options in [([], {}), ([rule()], {"model": "gpt-5.6-sol"}), ([rule(enabled=False)], {}), ([rule(skip_monitoring=True)], {})]:
            with self.assertRaises(GateError):
                self.select(rules, **options)
        current = rule()
        current["recovery_policy"]["mode"] = "return"
        with self.assertRaisesRegex(GateError, "rule_does_not_allow_recovery"):
            self.select([current])

    def test_matches_keyword_and_code_as_all(self):
        self.assertEqual(self.select([rule(keywords=["OVERLOADED"])])["id"], 7)
        with self.assertRaises(GateError):
            self.select([rule(keywords=["unrelated"] )])

    def test_equal_priority_ambiguity_stops(self):
        with self.assertRaisesRegex(GateError, "ambiguous"):
            self.select([rule(), rule(id=8)])
        self.assertEqual(self.select([rule(), rule(id=8, priority=9)])["id"], 8)

    def test_large_budget_or_retry_backoff_is_bounded(self):
        current = rule()
        current["recovery_policy"]["account_switches"] = 10
        with self.assertRaisesRegex(GateError, "attempt_limit"):
            self.select([current])
        current["recovery_policy"].update(account_switches=1, same_account_retries=1, budget_seconds=2)
        with self.assertRaisesRegex(GateError, "too_short"):
            self.select([current])

    def test_bare_server_error_matches_its_own_native_identity(self):
        current = rule(error_codes=[502])
        current["recovery_policy"]["upstream_codes"] = ["server_error"]
        self.assertEqual(self.select([current], status=502, code="server_error", envelope=BARE_ERROR)["max_attempts"], 2)


class MockTests(unittest.TestCase):
    def test_async_account_capability_probe_does_not_consume_fault_attempt(self):
        scenario = Scenario("encrypted_normal")
        status, content_type, body = scenario.response(0, {"stream": False, "tool_choice": "required", "tools": [{"type": "function", "name": "probe_ping"}]})
        self.assertEqual((status, content_type), (200, "application/json"))
        self.assertIn(b'"type":"function_call"', body)
        self.assertEqual(scenario.calls, [])
        self.assertEqual(scenario.capability_probes, 1)

    def test_encrypted_repair_preserves_summary_and_message(self):
        scenario = Scenario("encrypted_normal")
        body = {"input": [{"type": "reasoning", "encrypted_content": "synthetic-cipher", "summary": [{"type": "summary_text", "text": "retain synthetic summary"}]}, {"type": "message", "role": "user", "content": "synthetic followup"}]}
        _, _, first = scenario.response(0, body)
        self.assertEqual(parse_events(first)[0]["error"]["code"], "invalid_encrypted_content")
        del body["input"][0]["encrypted_content"]
        _, _, second = scenario.response(0, body)
        self.assertEqual(scenario.calls, [0, 0])
        self.assertTrue(scenario.first_has_cipher and scenario.repair_removes_cipher and scenario.repair_preserves_summary and scenario.repair_preserves_message)
        self.assertEqual([event["type"] for event in parse_events(second)], ["response.output_text.delta", "response.completed"])

    def test_fallback_requires_second_identity_but_after_output_stays_failed(self):
        scenario = Scenario("bare_sse_fallback")
        self.assertEqual(parse_events(scenario.response(0, {})[2])[0]["type"], "error")
        self.assertEqual(parse_events(scenario.response(1, {})[2])[-1]["type"], "response.completed")
        output = Scenario("already_output").response(0, {})[2]
        self.assertEqual([event["type"] for event in parse_events(output)], ["response.output_text.delta", "error"])


class WriteSafetyTests(unittest.TestCase):
    def test_api_rejects_global_rule_or_existing_account_writes_before_network(self):
        probe = object.__new__(CandidateProbe)
        probe.ledger = {"fixtures": [{"kind": "accounts", "id": 900, "identity": "owned"}]}
        for method, path in [("PUT", "/api/v1/admin/accounts/22"), ("POST", "/api/v1/admin/error-passthrough-rules"), ("DELETE", "/api/v1/admin/accounts/22"), ("DELETE", "/api/v1/admin/groups/900")]:
            with self.assertRaises(GateError):
                probe.api(method, path)


if __name__ == "__main__":
    unittest.main()
