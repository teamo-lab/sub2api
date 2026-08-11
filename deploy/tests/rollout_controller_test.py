import argparse
import importlib.machinery
import importlib.util
import json
import os
import pathlib
import tempfile
import unittest
from unittest import mock


CONTROLLER_PATH = pathlib.Path(__file__).parents[1] / "rollout" / "sub2api-rollout"


def load_controller(root):
    os.environ["SUB2API_ROLLOUT_ROOT"] = root
    os.environ["SUB2API_ROLLOUT_EVIDENCE_UID"] = str(os.getuid())
    loader = importlib.machinery.SourceFileLoader("sub2api_rollout_controller", str(CONTROLLER_PATH))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class RolloutControllerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.controller = load_controller(self.temp.name)
        self.persist_server_state_impl = self.controller.persist_server_state
        self.controller.persist_server_state = lambda: pathlib.Path(self.temp.name) / "server-state"

    def tearDown(self):
        self.temp.cleanup()

    @staticmethod
    def metrics(slot):
        return {
            "slot": slot,
            "terminal_outcomes": 200,
            "successes": 198,
            "failures": 2,
            "ttft_samples": 180,
            "ttft_p50_ms": 1000.0,
            "input_tokens": 10,
            "cache_read_tokens": 80,
            "cache_creation_tokens": 10,
        }

    def test_gate_exact_boundaries_pass(self):
        baseline = self.metrics("blue")
        candidate = self.metrics("green")
        candidate.update({
            "ttft_p50_ms": 1300.0,
            "successes": 197,
            "failures": 3,
            "cache_read_tokens": 70,
            "input_tokens": 20,
            "cache_creation_tokens": 10,
        })
        passed, reason, checks = self.controller.evaluate_gate(baseline, candidate, 200)
        self.assertTrue(passed)
        self.assertIsNone(reason)
        self.assertEqual(3, len(checks))

    def test_gate_fails_closed_when_samples_missing(self):
        candidate = self.metrics("green")
        candidate["terminal_outcomes"] = 199
        passed, reason, checks = self.controller.evaluate_gate(self.metrics("blue"), candidate, 200)
        self.assertFalse(passed)
        self.assertEqual("insufficient_candidate_samples", reason)
        self.assertEqual([], checks)

    def test_auxiliary_gate_rejects_relative_pass_during_shared_upstream_overload(self):
        candidate = self.metrics("green")
        candidate.update({"successes": 190, "failures": 10, "upstream_5xx": 10})

        passed, checks = self.controller.evaluate_auxiliary_guards(candidate, 0.995)

        self.assertFalse(passed)
        self.assertFalse(next(item for item in checks if item["name"] == "absolute_sla")["passed"])
        self.assertFalse(next(item for item in checks if item["name"] == "upstream_5xx")["passed"])

    def test_metrics_query_scopes_to_api_key_without_exposing_credentials(self):
        captured = {}

        class Result:
            returncode = 0
            stdout = '{"slot":"green","terminal_outcomes":200}'

        def run(command, **kwargs):
            captured["command"] = command
            captured["input"] = kwargs["input"]
            return Result()

        self.controller.METRICS_SQL_PATH = pathlib.Path(self.temp.name) / "gate.sql"
        self.controller.METRICS_SQL_PATH.write_text("SELECT 1", encoding="utf-8")
        self.controller.subprocess.run = run

        result = self.controller.query_metrics("green", "2026-08-11T00:00:00Z", "2026-08-11T00:10:00Z", 123)

        self.assertEqual(200, result["terminal_outcomes"])
        self.assertIn("--set=api_key_id=123", captured["command"])
        self.assertNotIn("sk-", " ".join(captured["command"]))

    def test_metrics_sql_accepts_empty_all_production_scope(self):
        sql_path = pathlib.Path(__file__).parents[1] / "rollout" / "gate_metrics.sql"
        sql = sql_path.read_text(encoding="utf-8")
        self.assertNotIn("api_key_id = :'api_key_id'::bigint", sql)
        self.assertEqual(2, sql.count("api_key_id = NULLIF(:'api_key_id', '')::bigint"))

    def test_metrics_sql_counts_unrecovered_http_200_stream_failures(self):
        sql_path = pathlib.Path(__file__).parents[1] / "rollout" / "gate_metrics.sql"
        sql = sql_path.read_text(encoding="utf-8")
        self.assertIn("terminal_error_rows", sql)
        self.assertIn("error.error_owner = 'provider'", sql)
        self.assertIn("NOT EXISTS", sql)
        self.assertIn("usage.request_id = error.request_id", sql)
        self.assertIn("error_owner = 'provider'", sql)

    def test_already_rolled_back_is_idempotent(self):
        state = {
            "topology_mode": "blue_green",
            "phase": "rolled_back",
            "active_slot": "blue",
            "stable_slot": "blue",
            "candidate_slot": "green",
            "rollback_target_slot": "blue",
            "rollback_schema_compatible": True,
        }
        status = {
            **state,
            "blue": {"health": "healthy", "weight": 100, "active_streams": 0},
            "green": {"health": "healthy", "weight": 0, "active_streams": 4},
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        mutations = []
        self.controller.set_server = lambda *args, **kwargs: mutations.append((args, kwargs))
        captured = []
        self.controller.emit = lambda payload, code=0: captured.append(payload) or code
        args = argparse.Namespace(actor="test", reason="test", action_id="rollback-test", dry_run=False)

        rc = self.controller.rollback(args)

        self.assertEqual(0, rc)
        self.assertEqual([], mutations)
        self.assertEqual("already_rolled_back", captured[0]["result"])
        self.assertTrue(captured[0]["drain_pending"])

    def test_rollback_enables_target_before_draining_source(self):
        digest = "sha256:" + "a" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 9,
            "phase": "soak_100",
            "active_slot": "green",
            "stable_slot": "blue",
            "candidate_slot": "green",
            "rollback_target_slot": "blue",
            "rollback_schema_compatible": True,
            "blue": {"digest": digest, "version": "stable"},
            "green": {"digest": "sha256:" + "b" * 64, "version": "candidate"},
        }
        status = {
            **state,
            "blue": {"health": "healthy", "weight": 0, "active_streams": 0, "image_id": digest},
            "green": {"health": "healthy", "weight": 100, "active_streams": 3, "image_id": state["green"]["digest"]},
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        self.controller.wait_healthy = lambda slot, timeout: None
        calls = []
        self.controller.set_server = lambda slot, runtime_state, weight, backends=self.controller.BACKENDS: calls.append((slot, runtime_state, weight))
        self.controller.verify_weights = lambda target, source: True
        self.controller.stable_probe = lambda target: True
        writes = []
        self.controller.atomic_write_json = lambda path, value: writes.append(dict(value))
        self.controller.append_history = lambda event: None
        captured = []
        self.controller.emit = lambda payload, code=0: captured.append(payload) or code
        args = argparse.Namespace(actor="test", reason="test", action_id="rollback-test", dry_run=False)

        rc = self.controller.rollback(args)

        self.assertEqual(0, rc)
        self.assertEqual(("blue", "ready", 100), calls[0])
        self.assertEqual(("green", "drain", 0), calls[1])
        self.assertEqual("rollback_applying", writes[0]["phase"])
        self.assertEqual("rolled_back", writes[-1]["phase"])
        self.assertEqual("blue", writes[-1]["rollback_target_slot"])
        self.assertEqual("traffic_restored", captured[0]["result"])

    def test_rollback_after_completed_release_uses_active_slot_as_source(self):
        old_digest = "sha256:" + "a" * 64
        new_digest = "sha256:" + "b" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 12,
            "phase": "complete",
            "active_slot": "green",
            "stable_slot": "green",
            "candidate_slot": "blue",
            "rollback_target_slot": "blue",
            "rollback_schema_compatible": True,
            "blue": {"digest": old_digest, "version": "old"},
            "green": {"digest": new_digest, "version": "new"},
        }
        status = {
            **state,
            "blue": {"health": "healthy", "weight": 0, "console_weight": 0, "active_streams": 0, "image_id": old_digest},
            "green": {"health": "healthy", "weight": 100, "console_weight": 100, "active_streams": 2, "image_id": new_digest},
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        self.controller.wait_healthy = lambda slot, timeout: None
        calls = []
        self.controller.set_server = lambda slot, runtime_state, weight, backends=self.controller.BACKENDS: calls.append((slot, runtime_state, weight))
        self.controller.verify_weights = lambda target, source: True
        self.controller.stable_probe = lambda target: True
        writes = []
        self.controller.atomic_write_json = lambda path, value: writes.append(dict(value))
        self.controller.append_history = lambda event: None
        self.controller.emit = lambda payload, code=0: code
        args = argparse.Namespace(actor="test", reason="test", action_id="rollback-completed", dry_run=False)

        self.assertEqual(0, self.controller.rollback(args))
        self.assertEqual([("blue", "ready", 100), ("green", "drain", 0)], calls)
        self.assertEqual("blue", writes[-1]["stable_slot"])
        self.assertEqual("green", writes[-1]["candidate_slot"])

    def test_rollback_probe_failure_restores_previous_traffic_safely(self):
        old_digest = "sha256:" + "a" * 64
        new_digest = "sha256:" + "b" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 13,
            "phase": "soak_100",
            "active_slot": "green",
            "stable_slot": "blue",
            "candidate_slot": "green",
            "rollback_target_slot": "blue",
            "rollback_schema_compatible": True,
            "blue": {"digest": old_digest},
            "green": {"digest": new_digest},
        }
        status = {
            **state,
            "blue": {"health": "healthy", "weight": 0, "console_weight": 0, "active_streams": 0, "image_id": old_digest},
            "green": {"health": "healthy", "weight": 100, "console_weight": 100, "active_streams": 2, "image_id": new_digest},
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        self.controller.wait_healthy = lambda slot, timeout: None
        calls = []
        self.controller.set_server = lambda slot, runtime_state, weight, backends=self.controller.BACKENDS: calls.append((slot, runtime_state, weight))
        self.controller.verify_weights = lambda target, source: True
        self.controller.stable_probe = lambda target: False
        self.controller.haproxy_stats = lambda: {
            ("api_pool", "blue"): {"weight": "0"},
            ("api_pool", "green"): {"weight": "100"},
            ("console_pool", "blue"): {"weight": "0"},
            ("console_pool", "green"): {"weight": "100"},
        }
        writes = []
        self.controller.atomic_write_json = lambda path, value: writes.append(dict(value))
        history = []
        self.controller.append_history = lambda event: history.append(event)
        args = argparse.Namespace(actor="test", reason="test", action_id="rollback-probe-fail", dry_run=False)

        with self.assertRaises(self.controller.ControllerError) as raised:
            self.controller.rollback(args)

        self.assertEqual("rollback_reverted_safe", raised.exception.error_code)
        self.assertEqual([
            ("blue", "ready", 100),
            ("green", "drain", 0),
            ("green", "ready", 100),
            ("blue", "drain", 0),
            ("green", "ready", 100),
            ("blue", "drain", 0),
        ], calls)
        self.assertEqual("rollback_reverted_safe", writes[-1]["phase"])
        self.assertEqual("green", writes[-1]["active_slot"])
        self.assertEqual("rollback_reverted_safe", history[-1]["result"])

    def test_rollback_partial_runtime_failure_never_drains_previous_source(self):
        old_digest = "sha256:" + "a" * 64
        new_digest = "sha256:" + "b" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 14,
            "phase": "canary_10",
            "active_slot": "blue",
            "stable_slot": "blue",
            "candidate_slot": "green",
            "rollback_target_slot": "blue",
            "rollback_schema_compatible": True,
            "blue": {"digest": old_digest},
            "green": {"digest": new_digest},
        }
        status = {
            **state,
            "blue": {"health": "healthy", "weight": 90, "console_weight": 100, "active_streams": 1, "image_id": old_digest},
            "green": {"health": "healthy", "weight": 10, "console_weight": 0, "active_streams": 1, "image_id": new_digest},
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        self.controller.wait_healthy = lambda slot, timeout: None
        calls = []

        def set_server(slot, runtime_state, weight, backends=self.controller.BACKENDS):
            calls.append((slot, runtime_state, weight))
            if len(calls) == 2:
                raise self.controller.ControllerError(7, "simulated_runtime_failure", "simulated")

        self.controller.set_server = set_server
        self.controller.verify_weights = lambda target, source: True
        self.controller.haproxy_stats = lambda: {
            ("api_pool", "blue"): {"weight": "90"},
            ("api_pool", "green"): {"weight": "10"},
            ("console_pool", "blue"): {"weight": "100"},
            ("console_pool", "green"): {"weight": "0"},
        }
        writes = []
        self.controller.atomic_write_json = lambda path, value: writes.append(dict(value))
        self.controller.append_history = lambda event: None
        args = argparse.Namespace(actor="test", reason="test", action_id="rollback-partial", dry_run=False)

        with self.assertRaises(self.controller.ControllerError) as raised:
            self.controller.rollback(args)

        self.assertEqual("rollback_reverted_safe", raised.exception.error_code)
        self.assertEqual([
            ("blue", "ready", 100),
            ("green", "drain", 0),
            ("blue", "ready", 90),
            ("green", "ready", 10),
            ("blue", "ready", 100),
            ("green", "drain", 0),
        ], calls)
        self.assertEqual("blue", writes[-1]["active_slot"])

    def test_restore_snapshot_fails_closed_when_stats_are_unavailable(self):
        status = {
            "blue": {"weight": 100, "console_weight": 100},
            "green": {"weight": 0, "console_weight": 0},
        }
        self.controller.set_server = lambda *args, **kwargs: None

        def stats_unavailable():
            raise self.controller.ControllerError(7, "haproxy_stats_empty", "missing")

        self.controller.haproxy_stats = stats_unavailable

        restored, errors = self.controller.restore_weight_snapshot(status)

        self.assertFalse(restored)
        self.assertEqual(["haproxy_stats_empty"], errors)

    def test_complete_swaps_stable_and_candidate_but_keeps_warm_rollback_target(self):
        old_digest = "sha256:" + "a" * 64
        new_digest = "sha256:" + "b" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 11,
            "phase": "soak_100",
            "active_slot": "green",
            "stable_slot": "blue",
            "candidate_slot": "green",
            "rollback_target_slot": "blue",
            "last_gate_passed": True,
            "gate_consecutive_passes": 3,
            "promotion_started_at": "2020-01-01T00:00:00Z",
            "blue": {"digest": old_digest, "version": "old"},
            "green": {"digest": new_digest, "version": "new"},
        }
        status = {
            **state,
            "blue": {"health": "healthy", "weight": 0, "active_streams": 1, "image_id": old_digest},
            "green": {"health": "healthy", "weight": 100, "active_streams": 2, "image_id": new_digest},
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        self.controller.append_history = lambda event: None
        writes = []
        self.controller.atomic_write_json = lambda path, value: writes.append(dict(value))
        captured = []
        self.controller.emit = lambda payload, code=0: captured.append(payload) or code
        args = argparse.Namespace(actor="test", action_id="complete-test", min_soak_seconds=0)

        self.assertEqual(0, self.controller.complete_release(args))
        final = writes[-1]
        self.assertEqual("complete", final["phase"])
        self.assertEqual("green", final["stable_slot"])
        self.assertEqual("blue", final["candidate_slot"])
        self.assertEqual("blue", final["rollback_target_slot"])
        self.assertTrue(captured[0]["drain_pending"])

    def test_stage_requires_idle_slot_metadata_to_match(self):
        stable_digest = "sha256:" + "a" * 64
        candidate_digest = "sha256:" + "b" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 12,
            "phase": "preparing",
            "release_id": "release-noop",
            "active_slot": "green",
            "stable_slot": "green",
            "candidate_slot": "blue",
            "rollback_target_slot": "blue",
            "green": {"digest": stable_digest, "version": "stable"},
            "blue": {"digest": "sha256:" + "c" * 64, "version": "old"},
        }
        status = {
            **state,
            "green": {"health": "healthy", "weight": 100, "active_streams": 0, "image_id": stable_digest},
            "blue": {"health": "healthy", "weight": 0, "active_streams": 0, "image_id": candidate_digest},
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        self.controller.container_deployment_metadata = lambda slot: {
            "SUB2API_DEPLOYMENT_SLOT": "blue",
            "SUB2API_RELEASE_ID": "release-noop",
            "SUB2API_DEPLOYMENT_VERSION": "0.1.172+noop.1",
            "SUB2API_DEPLOYMENT_DIGEST": candidate_digest,
        }
        self.controller.append_history = lambda event: None
        writes = []
        self.controller.atomic_write_json = lambda path, value: writes.append(dict(value))
        self.controller.emit = lambda payload, code=0: code
        args = argparse.Namespace(
            release_id="release-noop",
            candidate_slot="blue",
            candidate_version="0.1.172+noop.1",
            candidate_digest=candidate_digest,
            schema_compatible=True,
            actor="test",
            action_id="stage-noop",
        )

        self.assertEqual(0, self.controller.stage_release(args))
        self.assertEqual("staged", writes[-1]["phase"])
        self.assertEqual("green", writes[-1]["rollback_target_slot"])
        self.assertEqual(candidate_digest, writes[-1]["blue"]["digest"])

    def test_prepare_stage_moves_rollback_target_before_idle_slot_replacement(self):
        stable_digest = "sha256:" + "a" * 64
        candidate_digest = "sha256:" + "b" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 20,
            "phase": "complete",
            "release_id": "release-current",
            "active_slot": "green",
            "stable_slot": "green",
            "candidate_slot": "blue",
            "rollback_target_slot": "blue",
            "rollback_schema_compatible": True,
            "green": {"digest": stable_digest},
            "blue": {"digest": candidate_digest},
        }
        status = {
            **state,
            "green": {"health": "healthy", "weight": 100, "console_weight": 100, "active_streams": 1, "image_id": stable_digest},
            "blue": {"health": "healthy", "weight": 0, "console_weight": 0, "active_streams": 0, "image_id": candidate_digest},
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        writes = []
        self.controller.atomic_write_json = lambda path, value: writes.append(dict(value))
        self.controller.append_history = lambda event: None
        self.controller.emit = lambda payload, code=0: code
        args = argparse.Namespace(
            release_id="release-noop",
            candidate_slot="blue",
            schema_compatible=True,
            actor="test",
            action_id="prepare-noop",
        )

        self.assertEqual(0, self.controller.prepare_stage(args))
        prepared = writes[-1]
        self.assertEqual("preparing", prepared["phase"])
        self.assertEqual("green", prepared["rollback_target_slot"])
        self.assertEqual("green", prepared["active_slot"])
        self.assertEqual("release-noop", prepared["release_id"])

    def test_identical_image_attestation_requires_private_clean_paired_evidence(self):
        digest = "sha256:" + "a" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 21,
            "phase": "staged",
            "release_id": "release-noop",
            "active_slot": "green",
            "stable_slot": "green",
            "candidate_slot": "blue",
            "rollback_target_slot": "green",
            "green": {"digest": digest},
            "blue": {"digest": digest},
        }
        status = {
            **state,
            "green": {"health": "healthy", "weight": 100, "active_streams": 0, "image_id": digest},
            "blue": {"health": "healthy", "weight": 0, "active_streams": 0, "image_id": digest},
        }
        evidence = pathlib.Path(self.temp.name) / "reports" / "paired-clean"
        evidence.mkdir(parents=True)
        window = {"duration_seconds": 300}
        stable_report = {
            "requests": 10,
            "statuses": {"200": 10},
            "terminals": {"response.completed": 10},
            "ttft_seconds": {"p50": 1.4},
            "tokens": {"input": 1000, "cached": 800, "cache_rate": 0.8},
        }
        candidate_report = {
            **stable_report,
            "ttft_seconds": {"p50": 1.5},
            "tokens": {"input": 1000, "cached": 750, "cache_rate": 0.75},
        }
        for name, payload in (("window.json", window), ("green.json", stable_report), ("blue.json", candidate_report)):
            path = evidence / name
            path.write_text(json.dumps(payload), encoding="utf-8")
            path.chmod(0o600)

        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: status
        writes = []
        self.controller.atomic_write_json = lambda path, value: writes.append(dict(value))
        self.controller.append_history = lambda event: None
        captured = []
        self.controller.emit = lambda payload, code=0: captured.append(payload) or code
        args = argparse.Namespace(evidence_dir=str(evidence), actor="test", action_id="attest-noop")

        self.assertEqual(0, self.controller.attest_identical_image(args))
        attested = writes[-1]
        self.assertTrue(attested["noop_attested"])
        self.assertEqual("identical_image_attestation", attested["gate_mode"])
        self.assertEqual(3, attested["gate_consecutive_passes"])
        self.assertEqual("identical_image_attested", captured[-1]["result"])

    def test_stage_retry_with_same_action_id_is_idempotent_after_phase_advance(self):
        state = {
            "topology_mode": "blue_green",
            "phase": "staged",
            "last_action_id": "stage-same",
            "last_action_result": "release_staged",
        }
        self.controller.load_state = lambda: state
        self.controller.actual_status = lambda unused: {**state, "blue": {}, "green": {}}
        self.controller.container_deployment_metadata = lambda slot: self.fail("must not inspect on idempotent retry")
        self.controller.emit = lambda payload, code=0: code
        args = argparse.Namespace(
            release_id="release-noop",
            candidate_slot="blue",
            candidate_version="0.1.172+noop.1",
            candidate_digest="sha256:" + "a" * 64,
            schema_compatible=True,
            actor="test",
            action_id="stage-same",
        )

        self.assertEqual(0, self.controller.stage_release(args))

    def test_promotion_is_blocked_until_three_gate_cycles_pass(self):
        digest = "sha256:" + "b" * 64
        state = {
            "topology_mode": "blue_green",
            "stable_slot": "blue",
            "candidate_slot": "green",
            "last_gate_passed": True,
            "gate_consecutive_passes": 2,
            "green": {"digest": digest},
        }
        self.controller.load_state = lambda: state
        self.controller.container_state = lambda slot: {"health": "healthy", "image_id": digest}
        mutations = []
        self.controller.set_server = lambda *args, **kwargs: mutations.append((args, kwargs))
        args = argparse.Namespace(blue=0, green=100, phase="soak_100")

        with self.assertRaises(self.controller.ControllerError) as raised:
            self.controller.change_weights(args)

        self.assertEqual("gate_not_satisfied", raised.exception.error_code)
        self.assertEqual([], mutations)

    def test_canary_rejects_arbitrary_weights(self):
        state = {"topology_mode": "blue_green", "stable_slot": "blue", "candidate_slot": "green"}
        self.controller.load_state = lambda: state
        args = argparse.Namespace(blue=80, green=20, phase="canary_10")

        with self.assertRaises(self.controller.ControllerError) as raised:
            self.controller.change_weights(args)

        self.assertEqual("weights_do_not_match_phase", raised.exception.error_code)

    def test_weight_change_partial_failure_restores_previous_snapshot(self):
        stable_digest = "sha256:" + "a" * 64
        candidate_digest = "sha256:" + "b" * 64
        state = {
            "topology_mode": "blue_green",
            "generation": 30,
            "phase": "staged",
            "active_slot": "green",
            "stable_slot": "green",
            "candidate_slot": "blue",
            "green": {"digest": stable_digest},
            "blue": {"digest": candidate_digest},
        }
        before = {
            **state,
            "green": {"health": "healthy", "weight": 100, "console_weight": 100, "active_streams": 0, "image_id": stable_digest},
            "blue": {"health": "healthy", "weight": 0, "console_weight": 0, "active_streams": 0, "image_id": candidate_digest},
        }
        self.controller.load_state = lambda: state
        self.controller.container_state = lambda slot: {"health": "healthy", "image_id": candidate_digest}
        self.controller.actual_status = lambda unused: before
        calls = []

        def set_server(slot, runtime_state, weight, backends=self.controller.BACKENDS):
            calls.append((slot, runtime_state, weight, backends))
            if len(calls) == 2:
                raise self.controller.ControllerError(7, "simulated_runtime_failure", "simulated")

        self.controller.set_server = set_server
        self.controller.haproxy_stats = lambda: {
            ("api_pool", "blue"): {"weight": "0"},
            ("api_pool", "green"): {"weight": "100"},
            ("console_pool", "blue"): {"weight": "0"},
            ("console_pool", "green"): {"weight": "100"},
        }
        history = []
        self.controller.append_history = lambda event: history.append(event)
        args = argparse.Namespace(blue=10, green=90, phase="canary_10")

        with self.assertRaises(self.controller.ControllerError) as raised:
            self.controller.change_weights(args)

        self.assertEqual("weight_change_reverted_safe", raised.exception.error_code)
        self.assertEqual(("green", "ready", 100, ("api_pool",)), calls[2])
        self.assertEqual(("blue", "drain", 0, ("api_pool",)), calls[3])
        self.assertEqual("weight_change_reverted_safe", history[-1]["result"])

    def test_release_version_allows_semver_build_metadata(self):
        self.controller.validate_audit_value("release_metadata", "0.1.172+bluegreen.2")

    def test_show_stat_field_names_containing_error_are_not_rejections(self):
        class FakeSocket:
            def __init__(self, response):
                self.chunks = [response, b""]

            def __enter__(self):
                return self

            def __exit__(self, *unused):
                return False

            def settimeout(self, unused):
                pass

            def connect(self, unused):
                pass

            def sendall(self, unused):
                pass

            def shutdown(self, unused):
                pass

            def recv(self, unused):
                return self.chunks.pop(0)

        self.controller.SOCKET_PATH = pathlib.Path(self.temp.name) / "admin.sock"
        self.controller.SOCKET_PATH.touch()
        fake = FakeSocket(b"# pxname,h3_internal_error\napi_pool,0\n")
        with mock.patch.object(self.controller.socket, "socket", return_value=fake):
            response = self.controller.runtime_command("show stat")
        self.assertIn("h3_internal_error", response)

    def test_persist_server_state_writes_restart_durable_runtime_snapshot(self):
        target = pathlib.Path(self.temp.name) / "runtime" / "server-state"
        self.controller.SERVER_STATE_PATH = target
        self.controller.runtime_command = lambda command: (
            "1\n# be_id be_name srv_id srv_name srv_addr srv_op_state\n1 api_pool 1 blue 10.0.0.2 2"
        )

        written = self.persist_server_state_impl()

        self.assertEqual(target, written)
        self.assertIn("srv_name", target.read_text(encoding="utf-8"))
        self.assertEqual(0o600, target.stat().st_mode & 0o777)

    def test_mutating_runtime_command_rejects_nonempty_response(self):
        class FakeSocket:
            def __init__(self):
                self.chunks = [b"runtime command rejected\n", b""]

            def __enter__(self):
                return self

            def __exit__(self, *unused):
                return False

            def settimeout(self, unused):
                pass

            def connect(self, unused):
                pass

            def sendall(self, unused):
                pass

            def shutdown(self, unused):
                pass

            def recv(self, unused):
                return self.chunks.pop(0)

        self.controller.SOCKET_PATH = pathlib.Path(self.temp.name) / "admin.sock"
        self.controller.SOCKET_PATH.touch()
        with mock.patch.object(self.controller.socket, "socket", return_value=FakeSocket()):
            with self.assertRaises(self.controller.ControllerError) as raised:
                self.controller.runtime_command("set server api_pool/green weight 10")
        self.assertEqual("haproxy_runtime_rejected", raised.exception.error_code)


if __name__ == "__main__":
    unittest.main()
